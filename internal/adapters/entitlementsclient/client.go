// Package entitlementsclient reads Entitlements' effective set for
// BIL-ADD-1 (LLD §18.4, BL-OQ-3): GET /api/v1/internal/tenants/:id/entitlements.
package entitlementsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Client is port.EntitlementsReader.
type Client struct {
	base string
	http *http.Client
}

var _ port.EntitlementsReader = (*Client)(nil)

// New builds the client (ENTITLEMENTS_BASE_URL, ENTITLEMENTS_TIMEOUT_MS).
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), http: httpx.New("entitlements", timeout)}
}

// effectiveSet is the part of the response Billing reads. Unknown fields
// are ignored.
type effectiveSet struct {
	Limits struct {
		LLMTokenQuotaMonthly *struct {
			Value *int64 `json:"value"`
		} `json:"llm_token_quota_monthly"`
	} `json:"limits"`
	AddonEligibility map[string]json.RawMessage `json:"addon_eligibility"`
}

// TokenQuota implements port.EntitlementsReader. Any failure, or a body
// without the quota limit, is 503 ENTITLEMENTS_UNAVAILABLE: Billing never
// sells on a guess (BIL-ADD-1).
func (c *Client) TokenQuota(ctx context.Context, tenantID string) (port.TokenQuota, error) {
	var out port.TokenQuota
	u := c.base + "/api/v1/internal/tenants/" + url.PathEscape(tenantID) + "/entitlements"
	r, err := httpx.Call(ctx, c.http, http.MethodGet, u, tenantID, "", nil)
	if err != nil || !r.OK() {
		e := domain.NewError(domain.ErrEntitlementsUnavailable, "entitlements unavailable")
		e.Cause = err
		return out, e
	}
	var set effectiveSet
	if err := json.Unmarshal(r.Body, &set); err != nil || set.Limits.LLMTokenQuotaMonthly == nil {
		e := domain.NewError(domain.ErrEntitlementsUnavailable, "entitlements response lacks llm_token_quota_monthly")
		e.Cause = err
		return out, e
	}
	out.MonthlyLimit = set.Limits.LLMTokenQuotaMonthly.Value
	if raw, ok := set.AddonEligibility[domain.ResourceLLMTokens]; ok {
		out.Eligibility = eligibility(raw)
	}
	return out, nil
}

// eligibility accepts the W-1 shape either as a string or as
// {"eligible": bool, "reason": "..."}.
func eligibility(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Eligible bool   `json:"eligible"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(raw, &o) == nil {
		if o.Eligible {
			return "eligible"
		}
		return o.Reason
	}
	return ""
}
