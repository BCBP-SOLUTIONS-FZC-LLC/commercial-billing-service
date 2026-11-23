package main

import "testing"

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
	}
}

func TestUnknownJobIsUsageError(t *testing.T) {
	if got := run([]string{"--job", "nope"}); got != exitUsage {
		t.Fatalf("want %d, got %d", exitUsage, got)
	}
	if got := run(nil); got != exitUsage {
		t.Fatalf("missing --job: want %d, got %d", exitUsage, got)
	}
}

func TestKnownJobNotImplementedYet(t *testing.T) {
	if got := run([]string{"--job", "saga-sweeper"}); got != exitNotImplemented {
		t.Fatalf("want %d, got %d", exitNotImplemented, got)
	}
}
