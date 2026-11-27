package postgres

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Transport failures map to DB_UNAVAILABLE; "no rows" and application
// errors pass through unchanged.
func TestMapDBError(t *testing.T) {
	dial := fmt.Errorf("failed to connect: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if !domain.HasCode(mapDBError(dial), domain.ErrDBUnavailable) {
		t.Error("dial failure not DB_UNAVAILABLE")
	}
	if err := mapDBError(pgcommon.ErrNoRows); !errors.Is(err, pgcommon.ErrNoRows) {
		t.Errorf("no rows changed: %v", err)
	}
	plain := errors.New("bad input")
	if mapDBError(plain) != plain {
		t.Error("application error changed")
	}
}
