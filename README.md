# stripenav

[![CI](https://github.com/bancsdan/stripenav/actions/workflows/ci.yml/badge.svg)](https://github.com/bancsdan/stripenav/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/bancsdan/stripenav)](https://github.com/bancsdan/stripenav/releases)
[![Container](https://img.shields.io/badge/ghcr.io-bancsdan%2Fstripenav-blue?logo=docker)](https://github.com/bancsdan/stripenav/pkgs/container/stripenav)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A small deployable service that bridges Stripe webhook events to Hungary's NAV
[Online Számla v3.0](https://onlineszamla.nav.gov.hu/) invoice reporting API.

Drop the container in front of your Stripe webhooks. It verifies signatures,
translates invoices into NAV's XML format, submits them, retries failures,
and respects the 24-hour reporting deadline. Works for any backend stack —
the language you write your app in doesn't matter.

Built on the [go-stripenav](https://github.com/bancsdan/go-stripenav) Go
library. If you write Go and prefer to embed the bridge inside your own
HTTP server instead of running a sidecar, use the library directly. This
repo packages the same code as a container.

## Quickstart

```bash
docker run --rm -p 8080:8080 --env-file .env \
  ghcr.io/bancsdan/stripenav:latest
```

Then in Stripe, register a webhook endpoint at
`https://your-host:8080/webhooks/stripe` and subscribe to:

- `invoice.finalized`
- `invoice.voided`
- `invoice.marked_uncollectible`
- `credit_note.created`
- `credit_note.voided`

## Required environment

| Variable | Required | Notes |
| --- | --- | --- |
| `STRIPE_WEBHOOK_SECRET` | yes | Stripe endpoint signing secret (`whsec_…`). |
| `NAV_BASE_URL` | yes | `https://api.onlineszamla.nav.gov.hu/invoiceService/v3` for production, `https://api-test.onlineszamla.nav.gov.hu/invoiceService/v3` for the test env. |
| `NAV_LOGIN` | yes | NAV technical user login. |
| `NAV_PASSWORD` | yes | NAV technical user password (plaintext — the binary hashes it). |
| `NAV_TAX_NUMBER` | yes | 8-digit Hungarian tax base. Any extra characters are stripped. |
| `NAV_SIGN_KEY` | yes | Technical user signature key. |
| `NAV_EXCHANGE_KEY` | yes | Technical user exchange key (exactly 16 chars — AES-128). |
| `NAV_SOFTWARE_ID` | yes | 18 chars `[0-9A-Z]`. Convention: `<ISO 3166 alpha-2 country><dev tax-base><serial>` (e.g. `HU12345678STRIPENAV`). |
| `NAV_DEV_NAME`, `NAV_DEV_CONTACT` | yes | Developer info on the software block. |
| `NAV_DEBUG` | no | Set to `true` to log every NAV request/response body. **Local debugging only — bodies include the signed envelope.** |
| `SUPPLIER_TAX_NUMBER` | yes | 11-character Hungarian VAT number for the merchant (with or without hyphens). |
| `SUPPLIER_NAME` | yes | Supplier's registered legal name. |
| `SUPPLIER_COUNTRY` | no | Defaults to `HU`. |
| `SUPPLIER_POSTAL_CODE` | yes | |
| `SUPPLIER_CITY` | yes | |
| `SUPPLIER_ADDRESS` | no | Street + number etc. |
| `LISTEN_ADDR` | no | Defaults to `:8080`. |
| `STORE_URL` | no | See [Persistence](#persistence). Empty / `memory:` → in-memory (dev only). `postgres://...` → Postgres adapter, migrations run on boot. |

A starter `.env` is in [`.env.example`](.env.example).

## Endpoints

| Path | Purpose |
| --- | --- |
| `POST /webhooks/stripe` | The webhook endpoint to register with Stripe. |
| `GET /healthz` | Liveness probe — returns 204 No Content. |
| `GET /readyz` | Readiness probe — returns 204 No Content. |

## docker-compose

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: stripenav
      POSTGRES_PASSWORD: stripenavpw
      POSTGRES_DB: stripenav
    volumes:
      - pgdata:/var/lib/postgresql/data

  stripenav:
    image: ghcr.io/bancsdan/stripenav:latest
    depends_on: [postgres]
    ports:
      - "8080:8080"
    env_file: .env
    environment:
      STORE_URL: postgres://stripenav:stripenavpw@postgres:5432/stripenav?sslmode=disable
    restart: unless-stopped

volumes:
  pgdata: {}
```

## Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: stripenav
spec:
  replicas: 1   # see "Scaling" below
  selector:
    matchLabels:
      app: stripenav
  template:
    metadata:
      labels:
        app: stripenav
    spec:
      containers:
        - name: stripenav
          image: ghcr.io/bancsdan/stripenav:v0.1.0
          ports:
            - containerPort: 8080
          envFrom:
            - secretRef:
                name: stripenav-env
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8080
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
```

## Persistence

The container ships two `SubmissionStore` implementations, selected by `STORE_URL`:

| `STORE_URL` value | Adapter | When to use |
| --- | --- | --- |
| unset, or `memory:` | in-memory | Local dev, smoke tests. State lost on restart. |
| `postgres://user:pw@host:port/db?sslmode=…` | Postgres | Production. |
| `postgresql://…` | Postgres (same) | Production. |
| `mysql://…`, `dynamodb://…` | not built in | Returns startup error. Use the [library](https://github.com/bancsdan/go-stripenav) and implement your own. |

### In-memory (default) — IMPORTANT

State is lost on restart. That means:

- A pod restart between a CREATE going to NAV and the worker polling its
  status loses the transaction id; the submission is never marked `accepted`.
- Retries of failed submissions don't survive restart.
- The bridge's event-id deduplication breaks across restarts; you'll get
  duplicate NAV submissions on Stripe re-deliveries.

OK for: local dev, staging against the NAV test env, low-volume production
where occasional restart-loss is acceptable. Anything else: use Postgres.

### Postgres

Set `STORE_URL` to a libpq-style connection string:

```
STORE_URL=postgres://stripenav:secret@db.internal:5432/stripenav?sslmode=require
```

On boot the binary:

1. Opens a connection pool (default 10 conns, 1 hr lifetime).
2. Applies the embedded migration `001_init.sql` — creates the
   `stripenav_submissions` table and its two indexes. Idempotent.
3. Serves requests.

The DB user needs at least `INSERT`, `SELECT`, `UPDATE` on
`stripenav_submissions`, and `CREATE TABLE`, `CREATE INDEX` the first time
so the migration succeeds.

## Scaling

With the in-memory store: **exactly one replica.** Two pods sharing nothing
means two independent stores and two independent workers — they'll race on
submissions and produce duplicates.

With Postgres: `UpdateStatus` is atomic (it uses `SELECT … FOR UPDATE`),
so multi-pod is safe for state updates. The remaining caveat is
`ListPending`: it doesn't yet claim rows with `SELECT … FOR UPDATE SKIP
LOCKED`, so two workers reading the table at the same instant may both try
to submit the same record. Each `attemptSubmit` is fronted by the
`UpdateStatus` lock, so the *second* worker will see the row already in
`submitted` state and skip it — but NAV may still receive duplicate
`manageInvoice` calls in a tight race.

Until claim-with-skip-locked lands, **one container replica.**

## Observability

The binary writes JSON-structured logs to stdout via `slog`. Levels:

- `INFO` — server lifecycle, submission state transitions.
- `WARN` — bad signatures, missing parent for a STORNO, deferred work.
- `ERROR` — NAV submission failures, processing exceptions, deadline
  breaches.

No metrics endpoint yet. Wire your own `stripenav.MetricsRecorder` by
embedding the [library](https://github.com/bancsdan/go-stripenav) if you
need Prometheus integration.

## Local development

Clone alongside `go-stripenav` so the `replace` directive in `go.mod`
resolves the library against your local checkout:

```bash
mkdir -p ~/src/github.com/bancsdan
cd ~/src/github.com/bancsdan
git clone git@github.com:bancsdan/go-stripenav.git
git clone git@github.com:bancsdan/stripenav.git
cd stripenav
cp .env.example .env  # then edit
task dev
```

Other useful tasks:

```bash
task docker:build              # build the container image locally
task docker:run                # run it with .env mounted in
task pg:up                     # spin up throwaway Postgres on :25432
task test:pg                   # run storepg integration tests
task stripe:listen             # forward Stripe webhooks to localhost
task stripe:trigger EVENT=invoice.finalized
task nav:status TX=5EE7G1KH050R7P6K
```

## Updates

The image is published on every git tag matching `v*`. Pin to a specific
version in production:

```
ghcr.io/bancsdan/stripenav:v0.1.0   # exact version
ghcr.io/bancsdan/stripenav:0.1      # minor track
ghcr.io/bancsdan/stripenav:0        # major track
ghcr.io/bancsdan/stripenav:latest   # tip — fine for dev, never prod
```

## Compliance reminder

The bridge submits invoices to NAV. NAV's reporting requirement is a legal
obligation; the `aborted` terminal state and 24-hour deadline logs are how
the bridge reports its own failure modes back to you. Wire those into your
oncall.

## License

MIT — see [LICENSE](LICENSE).
