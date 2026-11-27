//go:build integration

package certify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// fakeStripe answers the suite's own Stripe calls (Test Clocks, products,
// prices) for test-mode Setup; failClock / failPriceAfter inject refusals.
type fakeStripe struct {
	mu             sync.Mutex
	failClock      bool
	failPriceAfter int // -1: never
	prices         int
	clockDeleted   bool
}

func (f *fakeStripe) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/test_helpers/test_clocks" && f.failClock:
			http.Error(w, `{"error":{"type":"invalid_request_error"}}`, http.StatusBadRequest)
		case r.URL.Path == "/v1/test_helpers/test_clocks":
			_, _ = w.Write([]byte(`{"id":"clock_certify"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/test_helpers/test_clocks/"):
			f.clockDeleted = true
			_, _ = w.Write([]byte(`{"id":"clock_certify","deleted":true}`))
		case r.URL.Path == "/v1/products":
			_, _ = w.Write([]byte(`{"id":"prod_certify"}`))
		case r.URL.Path == "/v1/prices":
			f.prices++
			if f.failPriceAfter >= 0 && f.prices > f.failPriceAfter {
				http.Error(w, `{"error":{"type":"api_error"}}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"id":"price_certify_` + string(rune('0'+f.prices)) + `"}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// dbExists reports whether a run database is still on the server.
func dbExists(t *testing.T, name string) bool {
	t.Helper()
	ctx := context.Background()
	p, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: serverDSN, PoolName: "certify_probe", MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var n int
	if err := p.WithConn(ctx, func(ctx context.Context, c *pgcommon.Conn) error {
		return c.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestSetupTestMode(t *testing.T) {
	ctx := context.Background()

	t.Run("clock refused", func(t *testing.T) {
		fs := &fakeStripe{failClock: true, failPriceAfter: -1}
		id := runID("tm")
		if _, _, err := Setup(ctx, Options{Target: TargetTestMode, ServerDSN: serverDSN, StripeKey: "rk_test_x", StripeBaseURL: fs.serve(t), RunID: id, Log: nopLog{}}); err == nil {
			t.Fatal("refused test clock accepted")
		}
		if dbExists(t, "certify_"+id) {
			t.Error("run database left behind")
		}
	})

	for name, after := range map[string]int{"plan price refused": 0, "token pack refused": 3} {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStripe{failPriceAfter: after}
			id := runID("tm")
			_, _, err := Setup(ctx, Options{Target: TargetTestMode, ServerDSN: serverDSN, StripeKey: "sk_test_x", StripeBaseURL: fs.serve(t), RunID: id, Log: nopLog{}})
			if err == nil || !strings.Contains(err.Error(), "price book") {
				t.Fatalf("want a price book error, got %v", err)
			}
			if !fs.clockDeleted || dbExists(t, "certify_"+id) {
				t.Errorf("teardown: clock deleted %v, database dropped %v", fs.clockDeleted, !dbExists(t, "certify_"+id))
			}
		})
	}

	t.Run("ready", func(t *testing.T) {
		fs := &fakeStripe{failPriceAfter: -1}
		id := runID("tm")
		env, teardown, err := Setup(ctx, Options{Target: TargetTestMode, ServerDSN: serverDSN, StripeKey: "sk_test_x", StripeBaseURL: fs.serve(t), RunID: id, Log: nopLog{}})
		if err != nil {
			t.Fatal(err)
		}
		if !env.Stateful() || env.Clock.(*TestClock).ID != "clock_certify" || len(env.Prices) != 3 || env.TokenPack != "certify_1m" {
			t.Errorf("env %+v", env)
		}
		if _, ok := env.Provider.(*mockState); ok {
			t.Error("test mode must not use the stripe-mock state layer")
		}
		n, err := Scalar[int](ctx, env.DB, `SELECT count(*) FROM plan_prices WHERE provider = 'stripe' AND provider_price_id LIKE 'price_certify_%'`)
		if err != nil || n != 3 {
			t.Errorf("stripe price book rows %d %v", n, err)
		}
		teardown()
		if !fs.clockDeleted || dbExists(t, "certify_"+id) {
			t.Errorf("teardown: clock deleted %v, database dropped %v", fs.clockDeleted, !dbExists(t, "certify_"+id))
		}
	})

	t.Run("adapter refuses an empty key", func(t *testing.T) {
		id := runID("sm")
		if _, _, err := Setup(ctx, Options{Target: TargetStripeMock, ServerDSN: serverDSN, StripeBaseURL: stripeMock, RunID: id, Log: nopLog{}}); err == nil || !strings.Contains(err.Error(), "API key") {
			t.Fatalf("empty key: %v", err)
		}
		if dbExists(t, "certify_"+id) {
			t.Error("run database left behind")
		}
	})

	t.Run("run id reused", func(t *testing.T) {
		id := runID("dup")
		db, err := OpenDB(ctx, serverDSN, id)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Drop(ctx) }()
		if _, err := OpenDB(ctx, serverDSN, id); err == nil || !strings.Contains(err.Error(), "CREATE") {
			t.Fatalf("second database with the same run id: %v", err)
		}
		// A role that cannot log in fails the migration or the pools, and
		// the half-built database is dropped.
		for role, want := range map[string]string{"billing_migrator": "migrate", "billing_app": "billing_app"} {
			if err := execOn(ctx, serverDSN, `ALTER ROLE `+role+` NOLOGIN`); err != nil {
				t.Fatal(err)
			}
			id := runID("nologin")
			_, err := OpenDB(ctx, serverDSN, id)
			if rerr := execOn(ctx, serverDSN, `ALTER ROLE `+role+` LOGIN`); rerr != nil {
				t.Fatal(rerr)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s without login: %v", role, err)
			}
			if dbExists(t, "certify_"+id) {
				t.Errorf("%s: run database left behind", role)
			}
		}
	})
}
