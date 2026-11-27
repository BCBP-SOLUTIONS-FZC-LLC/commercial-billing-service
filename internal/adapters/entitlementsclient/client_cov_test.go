package entitlementsclient

import (
	"context"
	"testing"
	"time"
)

// W-1 eligibility arrives as a string or as {"eligible","reason"}.
func TestTokenQuotaEligibilityShapes(t *testing.T) {
	for body, want := range map[string]string{
		`"not_active"`:                        "not_active",
		`{"eligible":true}`:                   "eligible",
		`{"eligible":false,"reason":"trial"}`: "trial",
		`[1,2]`:                               "",
	} {
		q, err := New(serve(t, 200, `{"limits":{"llm_token_quota_monthly":{"value":1}},"addon_eligibility":{"llm_tokens":`+body+`}}`), time.Second).
			TokenQuota(context.Background(), "t")
		if err != nil || q.Eligibility != want {
			t.Errorf("%s: eligibility %q (%v), want %q", body, q.Eligibility, err, want)
		}
	}
}
