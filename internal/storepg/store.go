// Package storepg is the Postgres SubmissionStore implementation used
// by the stripenav binary when STORE_URL has a postgres:// scheme.
//
// All bridge state lives in a dedicated `stripenav` schema. The schema
// is created (if missing) and migrated to the current version on
// Open. Multi-replica safety is provided by ClaimBatch using
// SELECT … FOR UPDATE SKIP LOCKED inside a single UPDATE … RETURNING
// statement: concurrent claimers always receive disjoint row sets.
// Each claim carries a TTL lease so a crashed claimer's work becomes
// available to another worker after the lease expires.
package storepg

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	stripenav "github.com/bancsdan/go-stripenav"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Compile-time check.
var _ stripenav.SubmissionStore = (*Store)(nil)

// Store is the Postgres-backed SubmissionStore.
type Store struct {
	pool *pgxpool.Pool
}

// Open dials Postgres, applies the embedded schema migration, and
// returns a ready Store. dsn is a libpq-style connection string, e.g.
// "postgres://user:pw@host:5432/db?sslmode=require".
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("storepg: parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("storepg: pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storepg: ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.applyMigrations(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying pool.
func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) applyMigrations(ctx context.Context) error {
	sqlBytes, err := migrationsFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		return fmt.Errorf("storepg: read embedded migration: %w", err)
	}
	// pgx uses the extended query protocol which executes only the
	// first statement. Split the migration on `;` and run each
	// statement individually. The migration intentionally has no
	// procedures or DO blocks, so a naive split is safe.
	for _, stmt := range strings.Split(string(sqlBytes), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("storepg: apply migration: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// columnList is the SELECT projection used by every read query, so all
// scan helpers see the same column order.
const columnList = `
	event_id, kind, operation, invoice_number, parent_event_id,
	status, attempts, last_error, transaction_id,
	next_attempt_at, issued_at, created_at, updated_at, raw_event,
	claimed_by, claimed_until
`

// Put inserts a new submission. Returns a non-nil error if the event id
// already exists, so the webhook handler's dedup path triggers.
func (s *Store) Put(ctx context.Context, sub stripenav.Submission) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stripenav.submissions (
			event_id, kind, operation, invoice_number, parent_event_id,
			status, attempts, last_error, transaction_id,
			next_attempt_at, issued_at, created_at, updated_at, raw_event,
			claimed_by, claimed_until
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NULL, NULL)
	`,
		sub.EventID, string(sub.Kind), sub.Operation, sub.InvoiceNumber,
		nullable(sub.ParentEventID),
		string(sub.Status), sub.Attempts, sub.LastError, sub.TransactionID,
		sub.NextAttemptAt.UTC(), sub.IssuedAt.UTC(),
		sub.CreatedAt.UTC(), sub.UpdatedAt.UTC(),
		sub.RawEvent,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("storepg: submission %q already exists", sub.EventID)
		}
		return fmt.Errorf("storepg: put: %w", err)
	}
	return nil
}

// Get returns the submission for eventID or stripenav.ErrNotFound.
func (s *Store) Get(ctx context.Context, eventID string) (stripenav.Submission, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+columnList+` FROM stripenav.submissions WHERE event_id = $1`, eventID)
	sub, err := scanOne(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return stripenav.Submission{}, stripenav.ErrNotFound
	}
	if err != nil {
		return stripenav.Submission{}, fmt.Errorf("storepg: get: %w", err)
	}
	return sub, nil
}

// UpdateStatus loads the row under SELECT … FOR UPDATE, calls mut,
// and writes back — all in one transaction. Concurrent mutators of
// the same row serialise here.
func (s *Store) UpdateStatus(ctx context.Context, eventID string, mut func(*stripenav.Submission) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("storepg: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `SELECT `+columnList+` FROM stripenav.submissions WHERE event_id = $1 FOR UPDATE`, eventID)
	sub, err := scanOne(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return stripenav.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("storepg: update select: %w", err)
	}

	if err := mut(&sub); err != nil {
		return err
	}
	sub.UpdatedAt = time.Now().UTC()

	if _, err := tx.Exec(ctx, `
		UPDATE stripenav.submissions SET
			operation       = $2,
			status          = $3,
			attempts        = $4,
			last_error      = $5,
			transaction_id  = $6,
			next_attempt_at = $7,
			updated_at      = $8,
			raw_event       = $9,
			parent_event_id = $10,
			invoice_number  = $11
		WHERE event_id = $1
	`,
		eventID,
		sub.Operation, string(sub.Status), sub.Attempts,
		sub.LastError, sub.TransactionID,
		sub.NextAttemptAt.UTC(), sub.UpdatedAt,
		sub.RawEvent,
		nullable(sub.ParentEventID),
		sub.InvoiceNumber,
	); err != nil {
		return fmt.Errorf("storepg: update exec: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storepg: commit: %w", err)
	}
	return nil
}

// ClaimBatch atomically reserves up to limit non-terminal rows that
// are due and not currently held by a live claim. The implementation
// uses a single UPDATE … FROM (SELECT … FOR UPDATE SKIP LOCKED) so
// concurrent claimers from different replicas always receive disjoint
// rows: the SKIP LOCKED clause means contended rows are silently
// passed over rather than blocked on.
func (s *Store) ClaimBatch(ctx context.Context, claimer string, limit int, lease time.Duration) ([]stripenav.Submission, error) {
	if limit <= 0 {
		return nil, errors.New("storepg: limit must be > 0")
	}
	if claimer == "" {
		return nil, errors.New("storepg: claimer is required")
	}
	until := time.Now().UTC().Add(lease)
	rows, err := s.pool.Query(ctx, `
		UPDATE stripenav.submissions
		   SET claimed_by    = $1,
		       claimed_until = $2,
		       updated_at    = now()
		 WHERE event_id IN (
		     SELECT event_id
		       FROM stripenav.submissions
		      WHERE status IN ('pending', 'submitted', 'processing')
		        AND next_attempt_at <= now()
		        AND (claimed_by IS NULL OR claimed_until IS NULL OR claimed_until < now())
		      ORDER BY created_at ASC
		      LIMIT $3
		      FOR UPDATE SKIP LOCKED
		 )
		 RETURNING `+columnList+`
	`, claimer, until, limit)
	if err != nil {
		return nil, fmt.Errorf("storepg: claim batch: %w", err)
	}
	defer rows.Close()
	return scanMany(rows)
}

// RenewClaim extends the lease on a row that claimer already holds.
// Returns stripenav.ErrClaimLost if the row no longer belongs to
// claimer (lease expired and stolen by another worker, or the row was
// released).
func (s *Store) RenewClaim(ctx context.Context, eventID, claimer string, lease time.Duration) error {
	if claimer == "" {
		return errors.New("storepg: claimer is required")
	}
	until := time.Now().UTC().Add(lease)
	tag, err := s.pool.Exec(ctx, `
		UPDATE stripenav.submissions
		   SET claimed_until = $3,
		       updated_at    = now()
		 WHERE event_id = $1
		   AND claimed_by = $2
	`, eventID, claimer, until)
	if err != nil {
		return fmt.Errorf("storepg: renew claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row is gone or some other claimer owns it now.
		// Distinguish for a clearer error.
		var exists bool
		_ = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM stripenav.submissions WHERE event_id = $1)`, eventID).Scan(&exists)
		if !exists {
			return stripenav.ErrNotFound
		}
		return stripenav.ErrClaimLost
	}
	return nil
}

// ReleaseClaim clears claimer's hold on the row. Returns
// stripenav.ErrClaimLost if the row no longer belongs to claimer.
func (s *Store) ReleaseClaim(ctx context.Context, eventID, claimer string) error {
	if claimer == "" {
		return errors.New("storepg: claimer is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE stripenav.submissions
		   SET claimed_by    = NULL,
		       claimed_until = NULL,
		       updated_at    = now()
		 WHERE event_id = $1
		   AND claimed_by = $2
	`, eventID, claimer)
	if err != nil {
		return fmt.Errorf("storepg: release claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		_ = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM stripenav.submissions WHERE event_id = $1)`, eventID).Scan(&exists)
		if !exists {
			return stripenav.ErrNotFound
		}
		return stripenav.ErrClaimLost
	}
	return nil
}

// FindByInvoiceNumber returns all submissions recorded for the given
// NAV invoice number, in CreatedAt order.
func (s *Store) FindByInvoiceNumber(ctx context.Context, invoiceNumber string) ([]stripenav.Submission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+columnList+`
		FROM stripenav.submissions
		WHERE invoice_number = $1
		ORDER BY created_at ASC
	`, invoiceNumber)
	if err != nil {
		return nil, fmt.Errorf("storepg: find by invoice: %w", err)
	}
	defer rows.Close()
	return scanMany(rows)
}

func scanOne(row pgx.Row) (stripenav.Submission, error) {
	var (
		sub           stripenav.Submission
		kind, status  string
		parentEventID *string
		claimedBy     *string
		claimedUntil  *time.Time
	)
	if err := row.Scan(
		&sub.EventID, &kind, &sub.Operation, &sub.InvoiceNumber, &parentEventID,
		&status, &sub.Attempts, &sub.LastError, &sub.TransactionID,
		&sub.NextAttemptAt, &sub.IssuedAt, &sub.CreatedAt, &sub.UpdatedAt,
		&sub.RawEvent,
		&claimedBy, &claimedUntil,
	); err != nil {
		return stripenav.Submission{}, err
	}
	sub.Kind = stripenav.EventKind(kind)
	sub.Status = stripenav.SubmissionStatus(status)
	if parentEventID != nil {
		sub.ParentEventID = *parentEventID
	}
	if claimedBy != nil {
		sub.ClaimedBy = *claimedBy
	}
	if claimedUntil != nil {
		sub.ClaimedUntil = *claimedUntil
	}
	return sub, nil
}

func scanMany(rows pgx.Rows) ([]stripenav.Submission, error) {
	var out []stripenav.Submission
	for rows.Next() {
		sub, err := scanOne(rows)
		if err != nil {
			return nil, fmt.Errorf("storepg: scan: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storepg: rows: %w", err)
	}
	return out, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
