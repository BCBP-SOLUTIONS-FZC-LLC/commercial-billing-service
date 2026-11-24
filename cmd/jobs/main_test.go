package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
)

// The CronJob list is LLD §13.2: ten jobs, including pii-minimisation
// (BIL-OPS-9) and renewal-repricing (BIL-SUB-8).
func TestJobListMatchesLLD(t *testing.T) {
	want := map[string]string{
		"saga-sweeper":           "* * * * *",
		"seat-renewal":           "*/15 * * * *",
		"downgrade-apply":        "*/15 * * * *",
		"purchase-expiry":        "*/5 * * * *",
		"seat-projection-check":  "7,37 * * * *",
		"pii-minimisation":       "0 5 * * *",
		"outbox-prune":           "0 3 * * *",
		"processed-events-prune": "0 4 * * *",
		"renewal-repricing":      "20 * * * *",
		"idempotency-prune":      "30 4 * * *",
	}
	if len(Jobs) != len(want) {
		t.Fatalf("got %d jobs, want %d", len(Jobs), len(want))
	}
	for name, sched := range want {
		if Jobs[name] != sched {
			t.Errorf("%s schedule = %q, want %q", name, Jobs[name], sched)
		}
		if runners[name] == nil {
			t.Errorf("%s has no runner", name)
		}
	}
	if len(runners) != len(want) {
		t.Errorf("got %d runners, want %d", len(runners), len(want))
	}
}

func TestUnknownJobIsUsageError(t *testing.T) {
	never := func(string) int { t.Fatal("exec called"); return 0 }
	if got := run([]string{"--job", "nope"}, never); got != exitUsage {
		t.Fatalf("want %d, got %d", exitUsage, got)
	}
	if got := run(nil, never); got != exitUsage {
		t.Fatalf("missing --job: want %d, got %d", exitUsage, got)
	}
}

func TestKnownJobIsExecuted(t *testing.T) {
	var ran string
	if got := run([]string{"--job", "saga-sweeper"}, func(n string) int { ran = n; return exitOK }); got != exitOK || ran != "saga-sweeper" {
		t.Fatalf("got %d, ran %q", got, ran)
	}
}

type nopLog struct{}

func (nopLog) Debug(string, map[string]any) {}
func (nopLog) Info(string, map[string]any)  {}
func (nopLog) Warn(string, map[string]any)  {}
func (nopLog) Error(string, map[string]any) {}

// A job error fails the run; failed items alone do not (retried next run).
func TestRunJobExitStatus(t *testing.T) {
	d := deps{pruneOutbox: func(context.Context, time.Duration) (int64, error) { return 3, nil },
		pruneProcessed: func(context.Context, time.Duration) (int64, error) { return 0, errors.New("db down") }}
	if got, outcome := runJob(context.Background(), "outbox-prune", d, nopLog{}); got != exitOK || outcome != "success" {
		t.Fatalf("outbox-prune: want %d, got %d", exitOK, got)
	}
	if got, outcome := runJob(context.Background(), "processed-events-prune", d, nopLog{}); got != exitFailed || outcome != "failure" {
		t.Fatalf("processed-events-prune: want %d, got %d", exitFailed, got)
	}
	_ = app.JobResult{}
}

// pushRun PUTs the run's metrics to /metrics/job/<cronjob name>.
func TestPushRun(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, body = r.Method+" "+r.URL.Path, string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "commercial_cross_tenant_job_runs_total", Help: "h"}, []string{"job", "outcome"})
	reg.MustRegister(c)
	c.WithLabelValues("seat-renewal", "success").Inc()
	if err := pushRun(srv.URL, "seat-renewal", reg); err != nil {
		t.Fatal(err)
	}
	if path != "PUT /metrics/job/seat-renewal" || !strings.Contains(body, "commercial_cross_tenant_job_runs_total") {
		t.Fatalf("%s %q", path, body)
	}
	if err := pushRun("", "seat-renewal", reg); err != nil {
		t.Fatal("empty URL must be a no-op")
	}
}
