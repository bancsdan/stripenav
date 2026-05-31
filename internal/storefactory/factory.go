// Package storefactory selects a SubmissionStore implementation based
// on a URL scheme. Used by the stripenav binary to parse STORE_URL at
// startup.
package storefactory

import (
	"context"
	"fmt"
	"strings"

	stripenav "github.com/bancsdan/go-stripenav"
	"github.com/bancsdan/go-stripenav/storeinmem"
	"github.com/bancsdan/stripenav/internal/storepg"
)

// From returns a SubmissionStore appropriate for url along with a
// close function. The close function is always non-nil; callers
// invoke it on shutdown.
//
// Supported schemes:
//
//   - empty string or "memory:" → storeinmem.Store (dev only, lossy
//     on restart).
//   - "postgres://..." / "postgresql://..." → storepg.Store, migrations
//     applied on open.
//
// Unrecognised schemes return a descriptive error.
func From(ctx context.Context, url string) (stripenav.SubmissionStore, func(), error) {
	switch {
	case url == "" || url == "memory:":
		s := storeinmem.New()
		return s, func() {}, nil

	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		s, err := storepg.Open(ctx, url)
		if err != nil {
			return nil, nil, err
		}
		return s, s.Close, nil

	case strings.HasPrefix(url, "mysql://"):
		return nil, nil, fmt.Errorf("storefactory: mysql:// is not built into this binary; embed the library and implement SubmissionStore yourself (see docs/EMBED.md)")

	case strings.HasPrefix(url, "dynamodb://"):
		return nil, nil, fmt.Errorf("storefactory: dynamodb:// is not built into this binary; embed the library and implement SubmissionStore yourself (see docs/EMBED.md)")

	default:
		return nil, nil, fmt.Errorf("storefactory: unknown STORE_URL scheme %q (supported: empty/memory:, postgres://, postgresql://)", url)
	}
}
