package iamclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// I-18: an unreadable body fails closed; absent blockers become an empty list.
func TestPlanChangeCheckBodies(t *testing.T) {
	s := server(t, 200, `not json`, nil)
	if _, err := NewCore(s.URL, time.Second).PlanChangeCheck(context.Background(), tenant, domain.PlanPro); !domain.HasCode(err, domain.ErrIAMUnavailable) {
		t.Errorf("unreadable: %v", err)
	}
	s = server(t, 200, `{"allowed":true}`, nil)
	c, err := NewCore(s.URL, time.Second).PlanChangeCheck(context.Background(), tenant, domain.PlanPro)
	if err != nil || !c.Allowed || c.Blockers == nil || len(c.Blockers) != 0 {
		t.Errorf("no blockers: %+v %v", c, err)
	}
}

// A 4xx other than 404 is never a silent allow: 503 with ErrUnexpected as cause.
func TestCoreErrorOn4xx(t *testing.T) {
	s := server(t, 400, `{"type":"x","status":400,"errorCode":"VALIDATION_ERROR"}`, nil)
	_, err := NewCore(s.URL, time.Second).SeatUsage(context.Background(), tenant)
	if !domain.HasCode(err, domain.ErrIAMUnavailable) || !errors.Is(err, httpx.ErrUnexpected) {
		t.Errorf("400: %v", err)
	}
}

// RP-4 sends the reason and the key; transport errors are status 0.
func TestRevertConversion(t *testing.T) {
	var got seen
	s := server(t, 202, `{}`, &got)
	a := NewRealmProvisioner(s.URL, time.Second).RevertConversion(context.Background(), tenant, "refunded", "revert:t:s")
	if a.Status != 202 || got.path != "/api/v1/internal/tenants/"+tenant+"/revert-conversion" || got.headers.Get("Idempotency-Key") != "revert:t:s" {
		t.Fatalf("%+v %+v", a, got)
	}
}

// RP-11 decodes the realm view; a 404 is TENANT_NOT_KNOWN_TO_IAM.
func TestRealm(t *testing.T) {
	var got seen
	s := server(t, 200, `{"strategy":"dedicated","status":"active","realm_name":"t-r","idp_count":2,"record_version":3}`, &got)
	v, err := NewRealmProvisioner(s.URL, time.Second).Realm(context.Background(), tenant)
	if err != nil || v.Strategy != "dedicated" || v.IDPCount != 2 || v.RecordVersion != 3 || got.path != "/api/v1/internal/tenants/"+tenant+"/realm" {
		t.Fatalf("%+v %v %+v", v, err, got)
	}
	s = server(t, 404, `{"errorCode":"TENANT_NOT_FOUND"}`, nil)
	if _, err := NewRealmProvisioner(s.URL, time.Second).Realm(context.Background(), tenant); !domain.HasCode(err, domain.ErrTenantNotKnownToIAM) {
		t.Errorf("404: %v", err)
	}
}
