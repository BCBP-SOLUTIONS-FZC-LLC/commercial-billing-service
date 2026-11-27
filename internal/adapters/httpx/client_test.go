package httpx

import "testing"

func TestOperation(t *testing.T) {
	for _, c := range []struct{ m, p, want string }{
		{"POST", "/v1/subscriptions/sub_1QAbCdEfGh", "subscriptions.update"},
		{"POST", "/v1/subscriptions", "subscriptions.create"},
		{"GET", "/v1/invoices", "invoices.list"},
		{"GET", "/v1/invoices/in_1QAbCdEfGh", "invoices.retrieve"},
		{"POST", "/v1/subscription_schedules/sub_sched_1QAbCdEfGh/release", "subscription_schedules.release.create"},
		{"GET", "/api/v1/internal/tenants/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b/seat-usage", "tenants.seat-usage.list"},
		{"POST", "/api/v1/internal/tenants/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b/convert", "tenants.convert.create"},
	} {
		if got := Operation(c.m, c.p); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.m, c.p, got, c.want)
		}
	}
}
