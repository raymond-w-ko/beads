//go:build !cgo

package embeddeddolt

import (
	"context"
	"database/sql"
)

// OpenSQL is a stub that returns an error when CGO is not enabled.
func OpenSQL(_ context.Context, _, _, _ string) (*sql.DB, func() error, error) {
	return nil, nil, errNoCGO
}
