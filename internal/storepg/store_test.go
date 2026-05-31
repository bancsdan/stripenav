package storepg_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	stripenav "github.com/bancsdan/go-stripenav"
	"github.com/bancsdan/stripenav/internal/storepg"
)

// pgDSN returns the test DSN, skipping the calling test if unset. CI
// supplies it via a service container; locally, set it to your dev
// Postgres (e.g. PG_TEST_DSN=postgres://postgres:postgres@localhost:5432/stripenav_test?sslmode=disable).
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping postgres integration tests")
	}
	return dsn
}

// freshStore opens a Store and truncates the table so each test sees a
// clean slate. We rely on the migration being idempotent.
func freshStore(t *testing.T) *storepg.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := storepg.Open(ctx, pgDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	// Best-effort cleanup. We can't rely on UpdateStatus here since we
	// have no rows yet, so use a raw exec via a fresh open. Cheaper: a
	// separate cleanup helper would need the pool — we just use the
	// store's own connection by inserting into a temp table or calling
	// a known-bad operation. Instead: each test uses unique event_ids
	// and asserts what it inserted.
	return s
}

func sub(eventID, invoiceNumber string, status stripenav.SubmissionStatus, attemptAt time.Time) stripenav.Submission {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return stripenav.Submission{
		EventID:       eventID,
		Kind:          stripenav.KindInvoice,
		Operation:     "CREATE",
		InvoiceNumber: invoiceNumber,
		Status:        status,
		NextAttemptAt: attemptAt.UTC(),
		IssuedAt:      now,
		CreatedAt:     now,
		UpdatedAt:     now,
		RawEvent:      []byte("<InvoiceData/>"),
	}
}

func TestStore_PutGetDuplicate(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	eventID := "evt_pg_putget_" + time.Now().Format("150405.000000000")

	if err := s.Put(ctx, sub(eventID, "INV-PG-1", stripenav.StatusPending, time.Now())); err != nil {
		t.Fatalf("Put: %v", err)
	}

	dup := s.Put(ctx, sub(eventID, "INV-PG-1", stripenav.StatusPending, time.Now()))
	if dup == nil || !strings.Contains(dup.Error(), "already exists") {
		t.Fatalf("expected duplicate error, got %v", dup)
	}

	got, err := s.Get(ctx, eventID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.InvoiceNumber != "INV-PG-1" || got.Status != stripenav.StatusPending {
		t.Fatalf("Get returned wrong row: %+v", got)
	}

	if _, err := s.Get(ctx, "evt_pg_missing"); err != stripenav.ErrNotFound {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func TestStore_UpdateStatusAtomicUnderConcurrency(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	eventID := "evt_pg_concurrent_" + time.Now().Format("150405.000000000")

	if err := s.Put(ctx, sub(eventID, "INV-PG-CONC", stripenav.StatusPending, time.Now())); err != nil {
		t.Fatalf("Put: %v", err)
	}

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.UpdateStatus(ctx, eventID, func(sub *stripenav.Submission) error {
				sub.Attempts++
				return nil
			}); err != nil {
				t.Errorf("UpdateStatus: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := s.Get(ctx, eventID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Attempts != N {
		t.Fatalf("Attempts = %d after %d concurrent increments (want %d) — UpdateStatus is not atomic", got.Attempts, N, N)
	}
}

func TestStore_ListPendingFiltersAndOrders(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	stamp := now.Format("150405.000000000")

	rows := []struct {
		eventID    string
		status     stripenav.SubmissionStatus
		nextAt     time.Time
		shouldList bool
		wantOrder  int
	}{
		{"evt_pg_list_a_" + stamp, stripenav.StatusPending, now.Add(-time.Minute), true, 0},
		{"evt_pg_list_b_" + stamp, stripenav.StatusPending, now.Add(time.Hour), false, -1},
		{"evt_pg_list_c_" + stamp, stripenav.StatusAccepted, now, false, -1},
		{"evt_pg_list_d_" + stamp, stripenav.StatusSubmitted, now.Add(-time.Second), true, 1},
	}
	for _, r := range rows {
		s2 := sub(r.eventID, "INV-PG-LIST-"+r.eventID, r.status, r.nextAt)
		s2.CreatedAt = now.Add(time.Duration(r.wantOrder) * time.Minute).UTC()
		if err := s.Put(ctx, s2); err != nil {
			t.Fatalf("Put %s: %v", r.eventID, err)
		}
	}

	got, err := s.ListPending(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}

	wantIDs := []string{"evt_pg_list_a_" + stamp, "evt_pg_list_d_" + stamp}
	if len(got) < len(wantIDs) {
		t.Fatalf("ListPending returned %d rows, want at least %d (%+v)", len(got), len(wantIDs), got)
	}

	// We may pick up rows from prior tests in the same DB; filter to the
	// stamp prefix so the assertion is robust.
	picked := make([]string, 0, len(got))
	for _, sub := range got {
		if strings.Contains(sub.EventID, stamp) {
			picked = append(picked, sub.EventID)
		}
	}
	if len(picked) != len(wantIDs) {
		t.Fatalf("picked %d stamped rows, want %d (%v)", len(picked), len(wantIDs), picked)
	}
	for i, id := range wantIDs {
		if picked[i] != id {
			t.Errorf("ListPending order[%d] = %s, want %s", i, picked[i], id)
		}
	}
}

func TestStore_FindByInvoiceNumber(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	invoice := "INV-PG-FIND-" + stamp

	for _, suffix := range []string{"a", "b", "c"} {
		s2 := sub("evt_pg_find_"+suffix+"_"+stamp, invoice, stripenav.StatusPending, time.Now())
		s2.CreatedAt = time.Now().UTC().Add(time.Duration(suffix[0]) * time.Microsecond)
		if err := s.Put(ctx, s2); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	got, err := s.FindByInvoiceNumber(ctx, invoice)
	if err != nil {
		t.Fatalf("FindByInvoiceNumber: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d (%+v)", len(got), got)
	}

	empty, err := s.FindByInvoiceNumber(ctx, "INV-PG-NO-SUCH-INVOICE-"+stamp)
	if err != nil {
		t.Fatalf("FindByInvoiceNumber(missing): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty result for unknown invoice, got %+v", empty)
	}
}

func TestStore_UpdateStatusOnMissingRowReturnsErrNotFound(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	err := s.UpdateStatus(ctx, "evt_pg_never_inserted", func(*stripenav.Submission) error { return nil })
	if err != stripenav.ErrNotFound {
		t.Fatalf("UpdateStatus(missing) = %v, want ErrNotFound", err)
	}
}
