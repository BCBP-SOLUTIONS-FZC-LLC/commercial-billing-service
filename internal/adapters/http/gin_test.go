package http

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

// testGin is the gincommon configuration the server passes, so tests run
// the real RFC 9457 error boundary.
func testGin() gincommon.Config {
	log, err := logger.NewLogger("dev")
	if err != nil {
		panic(err)
	}
	return gincommon.Config{Logger: log, ServiceName: "commercial-billing", BuildVersion: "test", Domain: "commercial",
		Environment: "test", ProblemTypeBase: "https://errors.bcbpsolutions.com"}
}

var _ = testing.Short
