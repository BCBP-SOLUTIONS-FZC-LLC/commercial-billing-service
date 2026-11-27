package iamclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
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

// RP-4 sends the reason and the key; RP answers 200 on success.
func TestRevertConversion(t *testing.T) {
	var got seen
	s := server(t, 200, `{}`, &got)
	a := NewRealmProvisioner(s.URL, time.Second).RevertConversion(context.Background(), tenant, "refunded", "revert:t:s")
	if a.Status != 200 || got.path != "/api/v1/internal/tenants/"+tenant+"/revert-conversion" || got.headers.Get("Idempotency-Key") != "revert:t:s" {
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

// RP answers problem errorCodes in UPPER_SNAKE (iam-realm-provisioner
// errors.go); the adapter lower-cases them, so ClassifyRP3 and B-O8's RP-4
// mapping see the codes they match on.
func TestRPProblemCodesReachTheClassifier(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   domain.RP3Action
	}{
		{409, "REALM_ALREADY_DEDICATED", domain.RP3Advance},
		{409, "OPERATION_IN_PROGRESS", domain.RP3Retry},
		{409, "OPTIMISTIC_LOCK_CONFLICT", domain.RP3Retry},
		{409, "REALM_NAME_UNAVAILABLE", domain.RP3Fail},
		{502, "CONVERSION_FAILED", domain.RP3Retry},
	} {
		s := server(t, tc.status, `{"type":"x","status":`+strconv.Itoa(tc.status)+`,"errorCode":"`+tc.code+`"}`, nil)
		rp := NewRealmProvisioner(s.URL, time.Second)
		a := rp.Convert(context.Background(), tenant, port.ConvertRequest{Plan: "pro"}, "convert:t:s")
		if got := domain.ClassifyRP3(a); got != tc.want || a.ErrorCode != strings.ToLower(tc.code) {
			t.Errorf("RP-3 %d %s: %+v -> %v, want %v", tc.status, tc.code, a, got, tc.want)
		}
		r := rp.RevertConversion(context.Background(), tenant, "refunded", "revert:t:s")
		if r.Status != tc.status || r.ErrorCode != strings.ToLower(tc.code) {
			t.Errorf("RP-4 %d %s: %+v", tc.status, tc.code, r)
		}
	}
}

// RP-11 gives up after its 2 s budget even when RP_TIMEOUT_MS is longer.
func TestRealmBudget(t *testing.T) {
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); s.Close() })
	start := time.Now()
	_, err := NewRealmProvisioner(s.URL, 30*time.Second).Realm(context.Background(), tenant)
	if !domain.HasCode(err, domain.ErrIAMUnavailable) || time.Since(start) > rp11Budget+time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

// An Istio RBAC denial is a 403 with a plain-text body, not a problem: the
// status survives and the errorCode is empty, so B-O8 treats it as terminal.
func TestRPNonProblemDenial(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("RBAC: access denied"))
	}))
	t.Cleanup(s.Close)
	a := NewRealmProvisioner(s.URL, time.Second).RevertConversion(context.Background(), tenant, "refunded", "revert:t:s")
	if a.Status != 403 || a.ErrorCode != "" {
		t.Fatalf("%+v", a)
	}
}
