-- 001_init.sql
-- Initial schema for the Postgres SubmissionStore adapter.
--
-- Applied on store open via embedded SQL — idempotent (IF NOT EXISTS
-- everywhere) so it's safe to run on every boot.

CREATE TABLE IF NOT EXISTS stripenav_submissions (
    event_id          TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    operation         TEXT NOT NULL,
    invoice_number    TEXT NOT NULL,
    parent_event_id   TEXT,
    status            TEXT NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    transaction_id    TEXT NOT NULL DEFAULT '',
    next_attempt_at   TIMESTAMPTZ NOT NULL,
    issued_at         TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    raw_event         BYTEA NOT NULL
);

-- For findParentSubmission lookup in the webhook handler.
CREATE INDEX IF NOT EXISTS stripenav_submissions_invoice_number_idx
    ON stripenav_submissions(invoice_number);

-- For worker tick (ListPending). Partial index keeps it small.
CREATE INDEX IF NOT EXISTS stripenav_submissions_pending_idx
    ON stripenav_submissions(next_attempt_at)
    WHERE status IN ('pending', 'submitted', 'processing');
