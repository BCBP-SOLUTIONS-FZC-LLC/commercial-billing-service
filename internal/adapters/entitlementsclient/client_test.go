package entitlementsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func serve(t *testing.T, status int, body string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestTokenQuota(t *testing.T) {
	ctx := context.Background()
	q, err := New(serve(t, 200, `{"limits":{"llm_token_quota_monthly":{"value":5000000}},"addon_eligibility":{"llm_tokens":{"eligible":false,"reason":"trial"}}}`), time.Second).TokenQuota(ctx, "t")
	if err != nil || q.MonthlyLimit == nil || *q.MonthlyLimit != 5_000_000 || q.Eligibility != "trial" {
		t.Fatalf("%+v %v", q, err)
	}
	q, err = New(serve(t, 200, `{"limits":{"llm_token_quota_monthly":{"value":null}}}`), time.Second).TokenQuota(ctx, "t")
	if err != nil || q.MonthlyLimit != nil || q.Eligibility != "" {
		t.Fatalf("unlimited: %+v %v", q, err)
	}
	// Fail closed (BIL-ADD-1): errors and a body without the limit.
	for name, url := range map[string]string{
		"503": serve(t, 503, `{}`), "no limit": serve(t, 200, `{"limits":{}}`), "unreachable": "http://127.0.0.1:1",
	} {
		if _, err := New(url, time.Second).TokenQuota(ctx, "t"); !domain.HasCode(err, domain.ErrEntitlementsUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
