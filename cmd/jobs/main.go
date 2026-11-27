// Command jobs is Billing's single CronJob binary: one --job=<name> per
// CronJob of LLD §13.2 (concurrencyPolicy Forbid, startingDeadlineSeconds
// 120). Each job pushes commercial_cross_tenant_job_runs_total to the
// Pushgateway when it is implemented.
//
// Phase 0: the dispatcher and the ten job names. Every job reports
// "not implemented" with exit status 2 until Phase 7.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Jobs is the authoritative list from LLD §13.2 (ten CronJobs).
var Jobs = map[string]string{
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

// Exit statuses.
const (
	exitUsage          = 64
	exitNotImplemented = 2
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	name := fs.String("job", "", "job to run: "+strings.Join(names(), ", "))
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if _, ok := Jobs[*name]; !ok {
		fmt.Fprintf(os.Stderr, "jobs: unknown or missing --job %q; valid: %s\n", *name, strings.Join(names(), ", "))
		return exitUsage
	}
	fmt.Fprintf(os.Stderr, "jobs: %s is not implemented yet (Phase 7)\n", *name)
	return exitNotImplemented
}

func names() []string {
	out := make([]string, 0, len(Jobs))
	for n := range Jobs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
