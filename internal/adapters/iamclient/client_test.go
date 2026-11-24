package iamclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

const tenant = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

type seen struct {
	headers     http.Header
	path, query string
}

func server(t *testing.T, status int, body string, got *seen) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			*got = seen{r.Header.Clone(), r.URL.Path, r.URL.RawQuery}
		}
		ct := "application/json"
		if status >= 400 {
			ct = "application/problem+json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// CD-19 identity, I-11 decoding.
func TestSeatUsage(t *testing.T) {
	var got seen
	s := server(t, 200, `{"active_users":7,"pending_invitations":2,"licensed_seats":10,"other":1}`, &got)
	u, err := NewCore(s.URL, time.Second).SeatUsage(context.Background(), tenant)
	if err != nil || u.InUse() != 9 || u.LicensedSeats != 10 {
		t.Fatalf("%+v %v", u, err)
	}
	if got.path != "/api/v1/internal/tenants/"+tenant+"/seat-usage" || got.headers.Get("x-user-id") != "commercial-system" ||
		got.headers.Get("x-tenant-roles") != "commercial-system" || got.headers.Get("x-tenant-id") != tenant {
		t.Fatalf("request %+v", got)
	}
}

// I-18 fails closed; 404 is TENANT_NOT_KNOWN_TO_IAM.
func TestPlanChangeCheck(t *testing.T) {
	var got seen
	s := server(t, 200, `{"allowed":false,"blockers":[{"code":"sso_idp_configured","detail":"1 IdP"}]}`, &got)
	c, err := NewCore(s.URL, time.Second).PlanChangeCheck(context.Background(), tenant, domain.PlanStarter)
	if err != nil || c.Allowed || len(c.Blockers) != 1 || got.query != "target_plan=starter" {
		t.Fatalf("%+v %v %q", c, err, got.query)
	}
	for status, code := range map[int]domain.ErrorCode{503: domain.ErrIAMUnavailable, 500: domain.ErrIAMUnavailable,
		404: domain.ErrTenantNotKnownToIAM, 403: domain.ErrIAMUnavailable} {
		s := server(t, status, `{"errorCode":"x"}`, nil)
		if _, err := NewCore(s.URL, time.Second).PlanChangeCheck(context.Background(), tenant, domain.PlanStarter); !domain.HasCode(err, code) {
			t.Errorf("%d: %v", status, err)
		}
	}
	// Unreachable and timeout → 503.
	if _, err := NewCore("http://127.0.0.1:1", time.Second).SeatUsage(context.Background(), tenant); !domain.HasCode(err, domain.ErrIAMUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	if _, err := NewCore(slow.URL, 50*time.Millisecond).SeatUsage(context.Background(), tenant); !domain.HasCode(err, domain.ErrIAMUnavailable) {
		t.Errorf("timeout: %v", err)
	}
}

// RP-3 answers are normalised for ClassifyRP3 (§9.3.2); the key is sent.
func TestConvert(t *testing.T) {
	var got seen
	s := server(t, 409, `{"type":"x","status":409,"errorCode":"operation_in_progress"}`, &got)
	a := NewRealmProvisioner(s.URL, time.Second).Convert(context.Background(), tenant,
		port.ConvertRequest{Plan: domain.PlanPro, BillingSubscriptionID: "sub"}, "convert:t:s")
	if a.Status != 409 || a.ErrorCode != "operation_in_progress" || domain.ClassifyRP3(a) != domain.RP3Retry ||
		got.headers.Get("Idempotency-Key") != "convert:t:s" || got.path != "/api/v1/internal/tenants/"+tenant+"/convert" {
		t.Fatalf("%+v %+v", a, got)
	}
	a = NewRealmProvisioner("http://127.0.0.1:1", time.Second).Convert(context.Background(), tenant, port.ConvertRequest{}, "k")
	if a.Status != 0 || domain.ClassifyRP3(a) != domain.RP3Retry {
		t.Fatalf("transport: %+v", a)
	}
	s = server(t, 200, `{"tenant_id":"`+tenant+`","realm":"r"}`, nil)
	if a := NewRealmProvisioner(s.URL, time.Second).Convert(context.Background(), tenant, port.ConvertRequest{}, "k"); domain.ClassifyRP3(a) != domain.RP3Advance {
		t.Fatalf("200: %+v", a)
	}
}
