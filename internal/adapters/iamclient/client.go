// Package iamclient calls Core (I-11, I-18) and Realm Provisioner (RP-3,
// RP-4, RP-11) as commercial-system (LLD §18.1, §18.2), only on the five
// routes CD-19 admits.
package iamclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Core is port.CoreClient.
type Core struct {
	base string
	http *http.Client
}

var _ port.CoreClient = (*Core)(nil)

// NewCore builds the Core client (CORE_BASE_URL, CORE_TIMEOUT_MS).
func NewCore(baseURL string, timeout time.Duration) *Core {
	return &Core{base: strings.TrimRight(baseURL, "/"), http: httpx.New("core", timeout)}
}

func (c *Core) tenantURL(tenantID, suffix string) string {
	return c.base + "/api/v1/internal/tenants/" + url.PathEscape(tenantID) + suffix
}

// SeatUsage is I-11.
func (c *Core) SeatUsage(ctx context.Context, tenantID string) (port.SeatUsage, error) {
	var out port.SeatUsage
	r, err := httpx.Call(ctx, c.http, http.MethodGet, c.tenantURL(tenantID, "/seat-usage"), tenantID, "", nil)
	if err := coreError("I-11", r, err); err != nil {
		return out, err
	}
	return out, unavailableOnDecode("I-11", r.Decode(&out))
}

// PlanChangeCheck is I-18 (fail closed: any failure is 503).
func (c *Core) PlanChangeCheck(ctx context.Context, tenantID string, target domain.PlanCode) (port.PlanChangeCheck, error) {
	var out port.PlanChangeCheck
	u := c.tenantURL(tenantID, "/plan-change-check?target_plan="+url.QueryEscape(string(target)))
	r, err := httpx.Call(ctx, c.http, http.MethodGet, u, tenantID, "", nil)
	if err := coreError("I-18", r, err); err != nil {
		return out, err
	}
	if err := r.Decode(&out); err != nil {
		return out, unavailableOnDecode("I-18", err)
	}
	if out.Blockers == nil {
		out.Blockers = []port.PlanChangeBlock{}
	}
	return out, nil
}

func coreError(route string, r httpx.Response, err error) error {
	switch {
	case err != nil || r.Status == 0 || r.Status >= 500:
		e := domain.NewError(domain.ErrIAMUnavailable, route+" unavailable")
		e.Cause = err
		return e
	case r.Status == http.StatusNotFound:
		return domain.NewError(domain.ErrTenantNotKnownToIAM, "the tenant is not known to IAM")
	case route == "I-18" && r.Status == http.StatusBadRequest:
		// 400 invalid_target_plan: plans are enums on both sides, so this is
		// a Billing bug → 500 INTERNAL (§5.4.1 B-P4 downgrade, §18.2).
		e := domain.NewError(domain.ErrInternal, fmt.Sprintf("I-18 answered 400 %s", r.ErrorCode))
		e.Cause = httpx.ErrUnexpected
		return e
	case !r.OK():
		// 400 / 401 / 403: a Billing bug or an admission problem; never a
		// silent allow.
		e := domain.NewError(domain.ErrIAMUnavailable, fmt.Sprintf("%s answered %d %s", route, r.Status, r.ErrorCode))
		e.Cause = httpx.ErrUnexpected
		return e
	}
	return nil
}

func unavailableOnDecode(route string, err error) error {
	if err == nil {
		return nil
	}
	e := domain.NewError(domain.ErrIAMUnavailable, route+" returned an unreadable body")
	e.Cause = err
	return e
}

// RealmProvisioner is port.RealmProvisionerClient.
type RealmProvisioner struct {
	base string
	http *http.Client
}

var _ port.RealmProvisionerClient = (*RealmProvisioner)(nil)

// NewRealmProvisioner builds the RP client (RP_BASE_URL, RP_TIMEOUT_MS).
func NewRealmProvisioner(baseURL string, timeout time.Duration) *RealmProvisioner {
	return &RealmProvisioner{base: strings.TrimRight(baseURL, "/"), http: httpx.New("realm_provisioner", timeout)}
}

func (c *RealmProvisioner) tenantURL(tenantID, suffix string) string {
	return c.base + "/api/v1/internal/tenants/" + url.PathEscape(tenantID) + suffix
}

// Convert is RP-3.
func (c *RealmProvisioner) Convert(ctx context.Context, tenantID string, req port.ConvertRequest, idemKey string) domain.RP3Answer {
	r, err := httpx.Call(ctx, c.http, http.MethodPost, c.tenantURL(tenantID, "/convert"), tenantID, idemKey, req)
	return answer(r, err)
}

// RevertConversion is RP-4.
func (c *RealmProvisioner) RevertConversion(ctx context.Context, tenantID, reason, idemKey string) domain.RP3Answer {
	r, err := httpx.Call(ctx, c.http, http.MethodPost, c.tenantURL(tenantID, "/revert-conversion"), tenantID, idemKey,
		map[string]string{"reason": reason})
	return answer(r, err)
}

func answer(r httpx.Response, err error) domain.RP3Answer {
	if err != nil {
		return domain.RP3Answer{Status: 0, ErrorCode: "transport_error"}
	}
	return domain.RP3Answer{Status: r.Status, ErrorCode: strings.ToLower(r.ErrorCode)}
}

// rp11Budget is RP-11's p99 budget (LLD §18.10): the operator view (B-O7)
// shows "unavailable" rather than wait the RP_TIMEOUT_MS sized for RP-3/RP-4.
const rp11Budget = 2 * time.Second

// Realm is RP-11, bounded by rp11Budget.
func (c *RealmProvisioner) Realm(ctx context.Context, tenantID string) (port.RealmView, error) {
	ctx, cancel := context.WithTimeout(ctx, rp11Budget)
	defer cancel()
	var out port.RealmView
	r, err := httpx.Call(ctx, c.http, http.MethodGet, c.tenantURL(tenantID, "/realm"), tenantID, "", nil)
	if err := coreError("RP-11", r, err); err != nil {
		return out, err
	}
	return out, unavailableOnDecode("RP-11", r.Decode(&out))
}
