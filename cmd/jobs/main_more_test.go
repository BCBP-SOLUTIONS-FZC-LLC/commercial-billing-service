package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

func TestBadFlagIsUsageError(t *testing.T) {
	never := func(string) int { t.Fatal("exec called"); return 0 }
	if got := run([]string{"--bogus"}, never); got != exitUsage {
		t.Fatalf("want %d, got %d", exitUsage, got)
	}
}

func TestLoggerEnv(t *testing.T) {
	for env, want := range map[string]string{"local": "dev", "dev": "dev", "test": "dev", "staging": "staging", "prod": "prod"} {
		if got := loggerEnv(env); got != want {
			t.Errorf("%s: %s, want %s", env, got, want)
		}
	}
}

// one tenant with expired idempotency records whose prune fails.
type failingPrune struct{ port.JobRepo }

func (failingPrune) ExpiredIdempotencyTenants(context.Context, time.Time, int) ([]string, error) {
	return []string{"00000000-0000-4000-8000-000000000001"}, nil
}

type failingTx struct{}

func (failingTx) RunInTenantTx(context.Context, string, func(context.Context) error) error {
	return errors.New("tenant transaction failed")
}

// Items that fail are retried next run: the run still succeeds, as partial.
func TestRunJobPartial(t *testing.T) {
	d := deps{svc: &app.Service{Jobs: failingPrune{}, Tx: failingTx{}}}
	if code, outcome := runJob(context.Background(), "idempotency-prune", d, nopLog{}); code != exitOK || outcome != "partial" {
		t.Fatalf("got %d %s, want %d partial", code, outcome, exitOK)
	}
}
