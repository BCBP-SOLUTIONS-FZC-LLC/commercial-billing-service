//go:build integration

// Package certify_test runs the cert-1.0 suite against stripe-mock and a
// throwaway Postgres (Phase 8 "done when": the suite passes against
// stripe-mock locally). The record it writes is a rehearsal: it must not be
// submittable.
package certify_test

import (
	"context"
	"fmt"
	"os"
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

func TestSuiteAgainstStripeMock(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Terminate(ctx) })
	dsn, _ := pg.ConnectionString(ctx, "sslmode=disable")
	sm, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{Image: "stripe/stripe-mock:latest", ExposedPorts: []string{"12111/tcp"},
			WaitingFor: wait.ForListeningPort("12111/tcp").WithStartupTimeout(60 * time.Second)},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Terminate(ctx) })
	host, _ := sm.Host(ctx)
	p, _ := sm.MappedPort(ctx, "12111")

	env, teardown, err := certify.Setup(ctx, certify.Options{Target: certify.TargetStripeMock, ServerDSN: dsn, StripeKey: "sk_test_123",
		StripeBaseURL: fmt.Sprintf("http://%s:%s", host, p.Port()), RunID: "t" + fmt.Sprint(time.Now().Unix()), Log: nopLog{}})
	if err != nil {
		t.Fatal(err)
	}
	results := certify.Run(ctx, env, certify.Cases(env.Provider.Capabilities().Proration))
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s/%s: %s", r.Capability, r.Case, r.Error)
		}
	}
	dir := t.TempDir()
	rec, err := certify.WriteEvidence(env, results, dir, "s3://commercial-cert-evidence", time.Now(), time.Now(), certify.MockAccountRef)
	if err != nil {
		t.Fatal(err)
	}
	if m := certify.Missing(rec); len(m) > 0 {
		t.Errorf("missing %v", m)
	}
	if len(rec.Results) != 31 { // 30 required + 1 optional
		t.Errorf("%d results, want 30 required + 1 optional", len(rec.Results))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	// The record path: B-O14 records it pending_review; the submitter
	// cannot approve it; a second PCI owner group member can (BIL-PROV-4);
	// only then does the release gate pass (§14.6).
	rec.ProviderAccountRef, rec.Reason = "acct_test_certify", "Certification suite end-to-end check"
	submitter := app.Caller{UserID: "00000000-0000-4000-8000-0000000000a1", Roles: []string{domain.RolePlatformOperator, domain.RolePCIOwnerReviewer}, IdempotencyKey: "k-cert-submit-1"}
	reviewer := app.Caller{UserID: "00000000-0000-4000-8000-0000000000a2", Roles: []string{domain.RolePlatformOperator, domain.RolePCIOwnerReviewer}, IdempotencyKey: "k-cert-review-1"}
	v, err := env.Svc.SubmitCertification(ctx, submitter, rec)
	if err != nil || v.Status != "pending_review" {
		t.Fatalf("B-O14 %+v %v", v, err)
	}
	if ok, err := certify.Certified(ctx, env.DB.DSN(), rec.Provider, rec.AdapterVersion, rec.SuiteVersion); err != nil || ok {
		t.Fatalf("gate before review: %v %v", ok, err)
	}
	if _, err := env.Svc.ReviewCertification(ctx, submitter, v.ID, app.ReviewRequest{Decision: "approve", Reason: "approving my own run"}); !domain.HasCode(err, domain.ErrSelfReviewNotAllowed) {
		t.Fatalf("self review: %v", err)
	}
	if v, err = env.Svc.ReviewCertification(ctx, reviewer, v.ID, app.ReviewRequest{Decision: "approve", Reason: "evidence reviewed by a second member"}); err != nil || v.Status != "passed" {
		t.Fatalf("B-O17 %+v %v", v, err)
	}
	if ok, err := certify.Certified(ctx, env.DB.DSN(), rec.Provider, rec.AdapterVersion, rec.SuiteVersion); err != nil || !ok {
		t.Fatalf("gate after review: %v %v", ok, err)
	}

	name := env.DB.Name
	teardown()
	if _, err := certify.OpenDB(ctx, dsn, name[len("certify_"):]); err != nil {
		t.Fatalf("database not dropped (re-create failed): %v", err)
	}
}
