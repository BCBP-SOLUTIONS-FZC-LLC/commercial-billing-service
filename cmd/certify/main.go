// Command certify is the provider certification tool (LLD §14.7, §19.6,
// BIL-PROV-1..4).
//
//	certify run    — run the suite (cert-1.0) and write the record file and evidence
//	certify submit — submit a test-mode record through B-O14 (pending_review)
//	certify review — approve or reject it through B-O17 (a different PCI owner group member)
//	certify check  — CI release gate: is <provider> <adapter_version> certified?
//	certify adapter-version — the Stripe adapter version of this build
//
// run environment: CERTIFY_SERVER_DSN (superuser DSN of a scratch Postgres
// server; the run creates and drops its own database), STRIPE_API_KEY
// (test mode: sk_test_ / rk_test_), STRIPE_BASE_URL (stripe-mock only).
// submit / review: BILLING_URL (gateway base URL), OPERATOR_TOKEN (the
// caller's bearer token). check: CERTIFY_CHECK_DSN (read-only role on the
// environment's billing database).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/certify"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 64
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: certify run|submit|review|check|adapter-version …")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "submit":
		return cmdSubmit(ctx, args[1:], stdout, stderr)
	case "review":
		return cmdReview(ctx, args[1:], stdout, stderr)
	case "check":
		return cmdCheck(ctx, args[1:], stdout, stderr)
	case "adapter-version":
		// The version this build would deploy (the release gate's argument).
		_, _ = fmt.Fprintln(stdout, certify.StripeAdapterVersion())
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "certify: unknown command %q\n", args[0])
		return exitUsage
	}
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", string(certify.TargetTestMode), "test-mode | stripe-mock")
	out := fs.String("out", "certification-record.json", "record file (§19.6)")
	dir := fs.String("evidence-dir", "evidence", "local evidence directory")
	base := fs.String("evidence-base-uri", "s3://commercial-cert-evidence", "where the evidence directory is uploaded (evidence_ref prefix)")
	if fs.Parse(args) != nil {
		return exitUsage
	}
	t := certify.Target(*target)
	if t != certify.TargetTestMode && t != certify.TargetStripeMock {
		_, _ = fmt.Fprintf(stderr, "certify: unknown target %q\n", *target)
		return exitUsage
	}
	log, err := logger.NewLogger("dev")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitFailed
	}
	runID := time.Now().UTC().Format("20060102t150405") + "_" + uuid.NewString()[:8]
	ctx, cancel := context.WithTimeout(ctx, 3*time.Hour)
	defer cancel()
	env, teardown, err := certify.Setup(ctx, certify.Options{Target: t, ServerDSN: os.Getenv("CERTIFY_SERVER_DSN"),
		StripeKey: os.Getenv("STRIPE_API_KEY"), StripeBaseURL: os.Getenv("STRIPE_BASE_URL"), RunID: runID, Log: log})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "certify: setup: %v\n", err)
		return exitFailed
	}
	defer teardown()
	started := time.Now().UTC()
	results := certify.Run(ctx, env, certify.Cases(env.Provider.Capabilities().Proration))
	finished := time.Now().UTC()
	account := certify.MockAccountRef
	if t == certify.TargetTestMode {
		if account, err = env.Deliver.API.AccountRef(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "certify: account: %v\n", err)
			return exitFailed
		}
	}
	rec, err := certify.WriteEvidence(env, results, *dir, *base, started, finished, account)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "certify: evidence: %v\n", err)
		return exitFailed
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(*out, b, 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "certify: %v\n", err)
		return exitFailed
	}
	failed := 0
	for _, r := range results {
		mark := "PASS"
		if !r.Passed {
			mark, failed = "FAIL", failed+1
		}
		_, _ = fmt.Fprintf(stdout, "%s  %s/%s  %s\n", mark, r.Capability, r.Case, r.Error)
	}
	missing := certify.Missing(rec)
	_, _ = fmt.Fprintf(stdout, "\n%d cases, %d failed; record %s; evidence %s\n", len(results), failed, *out, *dir)
	if t == certify.TargetStripeMock {
		_, _ = fmt.Fprintln(stdout, "stripe-mock rehearsal: the record is not submittable (provider_account_ref stripe-mock).")
	} else {
		_, _ = fmt.Fprintf(stdout, "upload the evidence before submitting:  aws s3 sync %s %s\n", *dir, *base)
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(stdout, "required cases not passed (B-O14 would answer 422 CERTIFICATION_INCOMPLETE): %s\n", strings.Join(missing, ", "))
		return exitFailed
	}
	return exitOK
}

func cmdSubmit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("record", "certification-record.json", "record file written by certify run")
	reason := fs.String("reason", "", "submission reason (10–1000 characters)")
	if fs.Parse(args) != nil || len(*reason) < 10 {
		_, _ = fmt.Fprintln(stderr, "certify submit: --record and a --reason of at least 10 characters are required")
		return exitUsage
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitFailed
	}
	var rec certify.Record
	if err := json.Unmarshal(b, &rec); err != nil {
		_, _ = fmt.Fprintf(stderr, "certify submit: %v\n", err)
		return exitFailed
	}
	if err := submittable(rec); err != nil {
		_, _ = fmt.Fprintf(stderr, "certify submit: %v\n", err)
		return exitFailed
	}
	rec.Reason = *reason
	return operatorCall(ctx, http.MethodPost, "/api/v1/operator/billing/provider-certifications", rec, stdout, stderr)
}

// submittable refuses what B-O14 must never record: a stripe-mock
// rehearsal or a record with a failed required case.
func submittable(rec certify.Record) error {
	if rec.ProviderAccountRef == certify.MockAccountRef || !strings.HasPrefix(rec.ProviderAccountRef, "acct_") {
		return errors.New("only a test-mode run against a provider account can be submitted")
	}
	if m := certify.Missing(rec); len(m) > 0 {
		return fmt.Errorf("required cases not passed: %s", strings.Join(m, ", "))
	}
	return nil
}

func cmdReview(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "certification id (B-O14's answer)")
	decision := fs.String("decision", "", "approve | reject")
	reason := fs.String("reason", "", "review reason (10–1000 characters)")
	if fs.Parse(args) != nil || *id == "" || (*decision != "approve" && *decision != "reject") || len(*reason) < 10 {
		_, _ = fmt.Fprintln(stderr, "certify review: --id, --decision approve|reject and a --reason of at least 10 characters are required")
		return exitUsage
	}
	// BIL-PROV-4: the server refuses the submitter (422 SELF_REVIEW_NOT_ALLOWED)
	// and any caller without pci_owner_reviewer.
	return operatorCall(ctx, http.MethodPost, "/api/v1/operator/billing/provider-certifications/"+*id+"/review",
		map[string]string{"decision": *decision, "reason": *reason}, stdout, stderr)
}

func operatorCall(ctx context.Context, method, path string, body any, stdout, stderr io.Writer) int {
	base, token := strings.TrimRight(os.Getenv("BILLING_URL"), "/"), os.Getenv("OPERATOR_TOKEN")
	if base == "" || token == "" {
		_, _ = fmt.Fprintln(stderr, "BILLING_URL and OPERATOR_TOKEN are required")
		return exitUsage
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(b))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitFailed
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "certify-"+uuid.NewString())
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitFailed
	}
	defer func() { _ = resp.Body.Close() }()
	ans, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_, _ = fmt.Fprintf(stdout, "%d %s\n", resp.StatusCode, ans)
	if resp.StatusCode >= 300 {
		return exitFailed
	}
	return exitOK
}

// cmdCheck is the release gate (§14.6): exit 0 only when a passed record
// exists for the provider, adapter version and suite version.
func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	suite := fs.String("suite", certify.SuiteVersion, "suite version (PROVIDER_CERT_SUITE_VERSION)")
	if fs.Parse(args) != nil || fs.NArg() != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: certify check [--suite cert-1.0] <provider> <adapter_version>")
		return exitUsage
	}
	dsn := os.Getenv("CERTIFY_CHECK_DSN")
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "CERTIFY_CHECK_DSN is required")
		return exitUsage
	}
	ok, err := certify.Certified(ctx, dsn, fs.Arg(0), fs.Arg(1), *suite)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "certify check: %v\n", err)
		return exitFailed
	}
	if !ok {
		_, _ = fmt.Fprintf(stdout, "NOT CERTIFIED: %s %s (%s) — run certify, submit (B-O14) and review (B-O17) first\n", fs.Arg(0), fs.Arg(1), *suite)
		return exitFailed
	}
	_, _ = fmt.Fprintf(stdout, "certified: %s %s (%s)\n", fs.Arg(0), fs.Arg(1), *suite)
	return exitOK
}
