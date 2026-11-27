package main

import (
	"errors"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

type recLog struct{ warns int }

func (*recLog) Debug(string, map[string]any)  {}
func (*recLog) Info(string, map[string]any)   {}
func (l *recLog) Warn(string, map[string]any) { l.warns++ }
func (*recLog) Error(string, map[string]any)  {}

// A successful run stamps the last-success gauge and PUTs the group; a
// failed run leaves the stamp alone and POSTs (BillingCronJobNotSucceeding).
func TestReportPushesBySuccess(t *testing.T) {
	log0, err := logger.NewLogger("dev")
	if err != nil {
		t.Fatal(err)
	}
	// The identity execute() uses with APP_ENV=test (integration tests run
	// it in the same process; gincommon refuses a second identity).
	version := os.Getenv("BUILD_VERSION")
	if version == "" {
		version = buildVersion
	}
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Logger: log0, ServiceName: "commercial-billing", Domain: "commercial", Environment: "test", BuildVersion: version})
	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	pushSucceeded = func(string, string) error { calls = append(calls, "put"); return nil }
	pushFailed = func(string, string) error { calls = append(calls, "post"); return errors.New("gateway down") }
	t.Cleanup(func() { pushSucceeded, pushFailed = metrics.PushRun, metrics.PushFailedRun })

	stamped := func() bool {
		fams, _ := reg.Gather()
		for _, f := range fams {
			if f.GetName() == metrics.CronJobLastSuccess {
				return len(f.GetMetric()) > 0
			}
		}
		return false
	}
	log := &recLog{}
	report("http://pgw", "seat-renewal", exitFailed, "failure", m, log)
	if stamped() || len(calls) != 1 || calls[0] != "post" || log.warns != 1 {
		t.Fatalf("failed run: stamped=%v calls=%v warns=%d", stamped(), calls, log.warns)
	}
	report("http://pgw", "seat-renewal", exitOK, "partial", m, log)
	if !stamped() || calls[1] != "put" {
		t.Fatalf("successful run: stamped=%v calls=%v", stamped(), calls)
	}
}
