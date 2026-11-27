package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/certify"
)

func TestAdapterVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if got := run([]string{"adapter-version"}, &out, &errb); got != exitOK || strings.TrimSpace(out.String()) != certify.StripeAdapterVersion() {
		t.Fatalf("exit %d, out %q", got, out.String())
	}
}

func TestRunRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	if got := run([]string{"run", "--bogus"}, &out, &errb); got != exitUsage {
		t.Errorf("bad flag: exit %d", got)
	}
	// The scratch server is unreachable: setup fails before any case runs.
	t.Setenv("LOG_FILE", "")
	t.Setenv("CERTIFY_SERVER_DSN", "postgres://postgres:x@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1")
	errb.Reset()
	if got := run([]string{"run", "--target", "stripe-mock"}, &out, &errb); got != exitFailed || !strings.Contains(errb.String(), "certify: setup:") {
		t.Errorf("unreachable server: exit %d, stderr %q", got, errb.String())
	}
	// A test-mode run refuses a live key before touching anything.
	t.Setenv("STRIPE_API_KEY", "sk_live_x")
	errb.Reset()
	if got := run([]string{"run"}, &out, &errb); got != exitFailed || !strings.Contains(errb.String(), "BIL-OPS-7") {
		t.Errorf("live key: exit %d, stderr %q", got, errb.String())
	}
	// A logger that cannot open its file fails the run.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FILE", filepath.Join(blocker, "app.log"))
	errb.Reset()
	if got := run([]string{"run", "--target", "stripe-mock"}, &out, &errb); got != exitFailed || errb.Len() == 0 {
		t.Errorf("unwritable LOG_FILE: exit %d, stderr %q", got, errb.String())
	}
}

func writeRecord(t *testing.T, rec any) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type operatorCallRec struct {
	method, path, auth, idem, ctype string
	body                            map[string]any
}

// operator answers B-O14 / B-O17 with code and records the request.
func operator(t *testing.T, code int, got *operatorCallRec) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = operatorCallRec{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			idem: r.Header.Get("Idempotency-Key"), ctype: r.Header.Get("Content-Type")}
		_ = json.Unmarshal(b, &got.body)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"status":"pending_review"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSubmit(t *testing.T) {
	reason := "--reason=suite run on the test account"
	var out, errb bytes.Buffer
	if got := run([]string{"submit", "--record", filepath.Join(t.TempDir(), "missing.json"), reason}, &out, &errb); got != exitFailed {
		t.Errorf("missing record: exit %d", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if got := run([]string{"submit", "--record", bad, reason}, &out, &errb); got != exitFailed || !strings.Contains(errb.String(), "certify submit:") {
		t.Errorf("invalid record: exit %d %q", got, errb.String())
	}
	// A stripe-mock rehearsal never reaches B-O14.
	var call operatorCallRec
	t.Setenv("BILLING_URL", operator(t, http.StatusCreated, &call))
	t.Setenv("OPERATOR_TOKEN", "tok")
	errb.Reset()
	if got := run([]string{"submit", "--record", writeRecord(t, fullRecord(certify.MockAccountRef)), reason}, &out, &errb); got != exitFailed ||
		!strings.Contains(errb.String(), "only a test-mode run") || call.method != "" {
		t.Errorf("stripe-mock record: exit %d %q, call %+v", got, errb.String(), call)
	}
	// A complete test-mode record is posted with the reason.
	out.Reset()
	if got := run([]string{"submit", "--record", writeRecord(t, fullRecord("acct_test_1")), reason}, &out, &errb); got != exitOK {
		t.Fatalf("submit: exit %d %q", got, errb.String())
	}
	if call.method != http.MethodPost || call.path != "/api/v1/operator/billing/provider-certifications" || call.auth != "Bearer tok" ||
		!strings.HasPrefix(call.idem, "certify-") || call.ctype != "application/json" ||
		call.body["reason"] != "suite run on the test account" || call.body["provider_account_ref"] != "acct_test_1" {
		t.Errorf("B-O14 request %+v", call)
	}
	if !strings.HasPrefix(out.String(), "201 ") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestReview(t *testing.T) {
	var call operatorCallRec
	t.Setenv("BILLING_URL", operator(t, http.StatusOK, &call)+"/")
	t.Setenv("OPERATOR_TOKEN", "tok")
	var out, errb bytes.Buffer
	if got := run([]string{"review", "--id", "c1", "--decision", "approve", "--reason", "evidence reviewed"}, &out, &errb); got != exitOK {
		t.Fatalf("review: exit %d %q", got, errb.String())
	}
	if call.path != "/api/v1/operator/billing/provider-certifications/c1/review" || call.body["decision"] != "approve" || call.body["reason"] != "evidence reviewed" {
		t.Errorf("B-O17 request %+v", call)
	}
	// The server refusing (BIL-PROV-4 self review) fails the command.
	t.Setenv("BILLING_URL", operator(t, http.StatusUnprocessableEntity, &call))
	out.Reset()
	if got := run([]string{"review", "--id", "c1", "--decision", "reject", "--reason", "evidence reviewed"}, &out, &errb); got != exitFailed || !strings.HasPrefix(out.String(), "422 ") {
		t.Errorf("refused review: exit %d %q", got, out.String())
	}
}

func TestOperatorCallFailures(t *testing.T) {
	review := []string{"review", "--id", "c1", "--decision", "approve", "--reason", "evidence reviewed"}
	var out, errb bytes.Buffer
	t.Setenv("BILLING_URL", "")
	t.Setenv("OPERATOR_TOKEN", "tok")
	if got := run(review, &out, &errb); got != exitUsage || !strings.Contains(errb.String(), "BILLING_URL and OPERATOR_TOKEN") {
		t.Errorf("no BILLING_URL: exit %d %q", got, errb.String())
	}
	t.Setenv("BILLING_URL", "http://bad\x7fhost")
	if got := run(review, &out, &errb); got != exitFailed {
		t.Errorf("invalid URL: exit %d", got)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	t.Setenv("BILLING_URL", closed.URL)
	if got := run(review, &out, &errb); got != exitFailed {
		t.Errorf("unreachable server: exit %d", got)
	}
}

func TestCheckRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	t.Setenv("CERTIFY_CHECK_DSN", "")
	if got := run([]string{"check", "stripe", "1.0.0"}, &out, &errb); got != exitUsage || !strings.Contains(errb.String(), "CERTIFY_CHECK_DSN") {
		t.Errorf("no DSN: exit %d %q", got, errb.String())
	}
	t.Setenv("CERTIFY_CHECK_DSN", "::not a dsn")
	errb.Reset()
	if got := run([]string{"check", "stripe", "1.0.0"}, &out, &errb); got != exitFailed || !strings.Contains(errb.String(), "certify check:") {
		t.Errorf("bad DSN: exit %d %q", got, errb.String())
	}
}
