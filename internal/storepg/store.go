// Package storepg is the Postgres SubmissionStore implementation used by
// the gostripenav binary when STORE_URL has a postgres:// scheme.
//
// The schema is embedded via go:embed and applied (idempotently) on
// Open. Atomic state updates use SELECT … FOR UPDATE inside a
// transaction; multi-worker safety is currently limited to that — see
// the package doc for the operational note on running a single worker
// per Postgres until claim-with-skip-locked is added.
package storepg

import (
	"context"
	"embed"
	"errors"
	"fmt"
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

// Close releases the underlying pool. Safe to call multiple times via
// the pool's own protection.
func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) applyMigrations(ctx context.Context) error {
	sqlBytes, err := migrationsFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		return fmt.Errorf("storepg: read embedded migration: %w", err)
	}
	if _, err := s.pool.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("storepg: apply migration: %w", err)
	}
	return nil
}

// columnList is the SELECT projection used by every read query, so all
// scan helpers see the same column order.
const columnList = `
	event_id, kind, operation, invoice_number, parent_event_id,
	status, attempts, last_error, transaction_id,
	next_attempt_at, issued_at, created_at, updated_at, raw_event
`

// Put inserts a new submission. Returns a non-nil error if the event id
// already exists, so the webhook handler's dedup path triggers.
func (s *Store) Put(ctx context.Context, sub stripenav.Submission) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stripenav_submissions (
			event_id, kind, operation, invoice_number, parent_event_id,
			status, attempts, last_error, transaction_id,
			next_attempt_at, issued_at, created_at, updated_at, raw_event
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
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
	row := s.pool.QueryRow(ctx, `SELECT `+columnList+` FROM stripenav_submissions WHERE event_id = $1`, eventID)
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
// and writes back — all in one transaction. Concurrent workers
// touching the same row serialise here.
func (s *Store) UpdateStatus(ctx context.Context, eventID string, mut func(*stripenav.Submission) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("storepg: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `SELECT `+columnList+` FROM stripenav_submissions WHERE event_id = $1 FOR UPDATE`, eventID)
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
		UPDATE stripenav_submissions SET
			operation = $2,
			status = $3,
			attempts = $4,
			last_error = $5,
			transaction_id = $6,
			next_attempt_at = $7,
			updated_at = $8,
			raw_event = $9,
			parent_event_id = $10,
			invoice_number = $11
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

// ListPending returns non-terminal submissions whose NextAttemptAt has
// elapsed. Caller is responsible for processing them; UpdateStatus does
// the per-row locking when each is touched.
func (s *Store) ListPending(ctx context.Context, before time.Time, limit int) ([]stripenav.Submission, error) {
	if limit <= 0 {
		return nil, errors.New("storepg: limit must be > 0")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+columnList+`
		FROM stripenav_submissions
		WHERE status IN ('pending', 'submitted', 'processing')
		  AND next_attempt_at <= $1
		ORDER BY created_at ASC
		LIMIT $2
	`, before.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("storepg: list pending: %w", err)
	}
	defer rows.Close()
	return scanMany(rows)
}

// FindByInvoiceNumber returns all submissions recorded for the given
// NAV invoice number, in CreatedAt order.
func (s *Store) FindByInvoiceNumber(ctx context.Context, invoiceNumber string) ([]stripenav.Submission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+columnList+`
		FROM stripenav_submissions
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
		sub             stripenav.Submission
		kind, status    string
		parentEventID   *string
	)
	if err := row.Scan(
		&sub.EventID, &kind, &sub.Operation, &sub.InvoiceNumber, &parentEventID,
		&status, &sub.Attempts, &sub.LastError, &sub.TransactionID,
		&sub.NextAttemptAt, &sub.IssuedAt, &sub.CreatedAt, &sub.UpdatedAt,
		&sub.RawEvent,
	); err != nil {
		return stripenav.Submission{}, err
	}
	sub.Kind = stripenav.EventKind(kind)
	sub.Status = stripenav.SubmissionStatus(status)
	if parentEventID != nil {
		sub.ParentEventID = *parentEventID
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
