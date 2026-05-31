// Module github.com/bancsdan/stripenav — the deployable bridge service
// and Docker image. Imports the bridge library
// (github.com/bancsdan/go-stripenav) as a regular Go dependency.
//
// For local development against an in-tree go-stripenav checkout, put a
// go.work (gitignored) at the repo root:
//
//     go 1.26.2
//     use .
//     use ../go-stripenav
//
// The go.work overrides this go.mod's require directive for local
// builds. CI and the released container don't see go.work and fetch
// the library from the module proxy at the pinned version below.

module github.com/bancsdan/stripenav

go 1.26.2

require (
	github.com/bancsdan/go-stripenav v0.2.0
	github.com/jackc/pgx/v5 v5.9.2
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/stripe/stripe-go/v82 v82.5.1 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)
