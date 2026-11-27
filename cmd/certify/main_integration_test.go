//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/certify"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

type nopLog struct{}

func (nopLog) Debug(string, map[string]any) {}
func (nopLog) Info(string, map[string]any)  {}
func (nopLog) Warn(string, map[string]any)  {}
func (nopLog) Error(string, map[string]any) {}

// containers starts a scratch Postgres 17 server and stripe-mock and
// returns the superuser DSN and the stripe-mock URL.
func containers(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	dsn, _ := pg.ConnectionString(ctx, "sslmode=disable")
	sm, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{Image: "stripe/stripe-mock:latest", ExposedPorts: []string{"12111/tcp"},
			WaitingFor: wait.ForListeningPort("12111/tcp").WithStartupTimeout(60 * time.Second)},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Terminate(context.Background()) })
	host, _ := sm.Host(ctx)
	p, _ := sm.MappedPort(ctx, "12111")
	return dsn, fmt.Sprintf("http://%s:%s", host, p.Port())
}

// refusingStripe fronts stripe-mock and lets only the price book through,
// so every case fails fast.
func refusingStripe(t *testing.T, stripeMock string) string {
	t.Helper()
	u, _ := url.Parse(stripeMock)
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/products" || r.URL.Path == "/v1/prices" {
			rp.ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"refused by the test"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunAgainstStripeMock(t *testing.T) {
	dsn, stripeMock := containers(t)
	t.Setenv("LOG_FILE", "")
	t.Setenv("CERTIFY_SERVER_DSN", dsn)
	t.Setenv("STRIPE_API_KEY", "sk_test_123")
	dir := t.TempDir()
	out := filepath.Join(dir, "record.json")
	ev := filepath.Join(dir, "evidence")

	t.Run("rehearsal passes and is not submittable", func(t *testing.T) {
		t.Setenv("STRIPE_BASE_URL", stripeMock)
		var stdout, stderr bytes.Buffer
		if got := run([]string{"run", "--target", "stripe-mock", "--out", out, "--evidence-dir", ev}, &stdout, &stderr); got != exitOK {
			t.Fatalf("exit %d\n%s\n%s", got, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), "FAIL ") || !strings.Contains(stdout.String(), "31 cases, 0 failed") ||
			!strings.Contains(stdout.String(), "the record is not submittable") {
			t.Errorf("stdout:\n%s", stdout.String())
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var rec certify.Record
		if err := json.Unmarshal(b, &rec); err != nil || rec.ProviderAccountRef != certify.MockAccountRef || len(rec.Results) != 31 {
			t.Fatalf("record %+v %v", rec, err)
		}
		if _, err := os.Stat(filepath.Join(ev, "stripe")); err != nil {
			t.Errorf("evidence: %v", err)
		}
		// cmd/certify refuses to submit it.
		t.Setenv("BILLING_URL", "http://127.0.0.1:1")
		t.Setenv("OPERATOR_TOKEN", "tok")
		stderr.Reset()
		if got := run([]string{"submit", "--record", out, "--reason", "rehearsal record"}, &stdout, &stderr); got != exitFailed ||
			!strings.Contains(stderr.String(), "only a test-mode run") {
			t.Errorf("submit of a rehearsal: exit %d %q", got, stderr.String())
		}
	})

	t.Setenv("STRIPE_BASE_URL", refusingStripe(t, stripeMock))
	t.Run("failed cases", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := run([]string{"run", "--target", "stripe-mock", "--out", out, "--evidence-dir", ev}, &stdout, &stderr); got != exitFailed {
			t.Fatalf("exit %d", got)
		}
		if !strings.Contains(stdout.String(), "FAIL  recurring_subscriptions/create") || !strings.Contains(stdout.String(), "required cases not passed") ||
			!strings.Contains(stdout.String(), "CERTIFICATION_INCOMPLETE") {
			t.Errorf("stdout:\n%s", stdout.String())
		}
	})
	t.Run("evidence not writable", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := run([]string{"run", "--target", "stripe-mock", "--out", out, "--evidence-dir", out}, &stdout, &stderr); got != exitFailed ||
			!strings.Contains(stderr.String(), "certify: evidence:") {
			t.Errorf("exit %d %q", got, stderr.String())
		}
	})
	t.Run("record not writable", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if got := run([]string{"run", "--target", "stripe-mock", "--out", filepath.Join(dir, "missing", "r.json"), "--evidence-dir", ev}, &stdout, &stderr); got != exitFailed ||
			!strings.HasPrefix(stderr.String(), "certify: ") {
			t.Errorf("exit %d %q", got, stderr.String())
		}
	})
}

// The release gate passes only once a record is reviewed (§14.6).
func TestCheckGate(t *testing.T) {
	dsn, stripeMock := containers(t)
	ctx := context.Background()
	env, teardown, err := certify.Setup(ctx, certify.Options{Target: certify.TargetStripeMock, ServerDSN: dsn, StripeKey: "sk_test_123",
		StripeBaseURL: stripeMock, RunID: fmt.Sprintf("gate%d", time.Now().Unix()), Log: nopLog{}})
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()
	t.Setenv("CERTIFY_CHECK_DSN", env.DB.DSN())
	version := certify.StripeAdapterVersion()
	check := func(want int, wantOut string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run([]string{"check", "stripe", version}, &stdout, &stderr); got != want || !strings.HasPrefix(stdout.String(), wantOut) {
			t.Errorf("check: exit %d (want %d), stdout %q, stderr %q", got, want, stdout.String(), stderr.String())
		}
	}
	check(exitFailed, "NOT CERTIFIED: stripe "+version)

	// A complete record as certify run writes it, from a test-mode account.
	var results []certify.Result
	for _, c := range certify.Cases(env.Provider.Capabilities().Proration) {
		results = append(results, certify.Result{Capability: c.Capability, Case: c.ID, Passed: true})
	}
	rec, err := certify.WriteEvidence(env, results, t.TempDir(), "s3://commercial-cert-evidence", time.Now().Add(-time.Hour), time.Now(), "acct_test_gate")
	if err != nil {
		t.Fatal(err)
	}
	rec.Reason = "Certification gate check"
	if rec.AdapterVersion != version {
		t.Fatalf("record adapter version %s, build %s", rec.AdapterVersion, version)
	}
	submitter := app.Caller{UserID: "00000000-0000-4000-8000-0000000000b1", Roles: []string{domain.RolePlatformOperator, domain.RolePCIOwnerReviewer}, IdempotencyKey: "k-gate-1"}
	reviewer := app.Caller{UserID: "00000000-0000-4000-8000-0000000000b2", Roles: []string{domain.RolePlatformOperator, domain.RolePCIOwnerReviewer}, IdempotencyKey: "k-gate-2"}
	v, err := env.Svc.SubmitCertification(ctx, submitter, rec)
	if err != nil {
		t.Fatal(err)
	}
	check(exitFailed, "NOT CERTIFIED")
	if _, err := env.Svc.ReviewCertification(ctx, reviewer, v.ID, app.ReviewRequest{Decision: "approve", Reason: "evidence reviewed by a second member"}); err != nil {
		t.Fatal(err)
	}
	check(exitOK, "certified: stripe "+version+" (cert-1.0)")
}
