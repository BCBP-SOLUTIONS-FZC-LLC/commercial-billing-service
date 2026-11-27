//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
)

// billingDB starts Postgres 17 with the service's roles (scripts/init-db.sql)
// and migrates it as billing_migrator. It returns the superuser DSN.
func billingDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("billing"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		tcpostgres.WithInitScripts("../../scripts/init-db.sql"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgadapter.MigrateSchema(ctx, withUser(dsn, "billing_migrator"), nil); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func withUser(dsn, user string) string {
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(user, "devpassword")
	return u.String()
}

// pushgateway records the CronJob metric pushes and answers code.
type pushgateway struct {
	mu     sync.Mutex
	code   int
	bodies map[string]string
}

func (p *pushgateway) start(t *testing.T) string {
	t.Helper()
	p.bodies = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies[r.URL.Path] = string(b)
		p.mu.Unlock()
		w.WriteHeader(p.code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func jobEnv(t *testing.T, dsn, push string) {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", withUser(dsn, "billing_app"))
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("PG_SYSTEM_PASSWORD", "devpassword")
	t.Setenv("PG_AUDIT_DRAINER_PASSWORD", "devpassword")
	t.Setenv("PUSHGATEWAY_URL", push)
	t.Setenv("LOG_FILE", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("AUDIT_SOURCE_SERVICE", "commercial-billing")
	t.Setenv("AUDIT_CALLER_DOMAIN", "commercial")
	t.Setenv("AUDIT_LOG_BASE_URL", "http://127.0.0.1:1")
}

// A CronJob runs against a migrated database and pushes its run metric; a
// pushgateway that refuses the push does not fail the run.
// The job metrics register on gincommon's process-wide registry, so a
// second run in the same process cannot register them and fails (one run
// per process is what the CronJob does).
func TestExecute(t *testing.T) {
	dsn := billingDB(t)
	pg := &pushgateway{code: http.StatusBadGateway}
	jobEnv(t, dsn, pg.start(t))
	if got := execute("outbox-prune"); got != exitOK {
		t.Fatalf("outbox-prune: exit %d, want %d", got, exitOK)
	}
	body := pg.bodies["/metrics/job/outbox-prune"]
	if !strings.Contains(body, "commercial_billing_cross_tenant_job_runs_total") || !strings.Contains(body, `job_name="outbox-prune"`) ||
		!strings.Contains(body, `result="success"`) {
		t.Errorf("push body %q", body)
	}
	if got := execute("saga-sweeper"); got != exitFailed {
		t.Errorf("second run in one process: exit %d, want %d", got, exitFailed)
	}
}

func TestExecuteRefusesBadConfiguration(t *testing.T) {
	t.Setenv("APP_ENV", "nonsense")
	t.Setenv("LOG_FILE", "")
	if got := execute("saga-sweeper"); got != exitFailed {
		t.Errorf("invalid APP_ENV: exit %d, want %d", got, exitFailed)
	}
	// A logger that cannot open its file fails before anything runs.
	t.Setenv("APP_ENV", "prod")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FILE", filepath.Join(blocker, "app.log"))
	if got := execute("saga-sweeper"); got != exitFailed {
		t.Errorf("unwritable LOG_FILE: exit %d, want %d", got, exitFailed)
	}
}

// build wires every job's use case: each runner runs against the database.
func TestBuildRunsEveryRunner(t *testing.T) {
	dsn := billingDB(t)
	jobEnv(t, dsn, "")
	t.Setenv("PG_AUDIT_DRAINER_PASSWORD", "") // the audit runner is per process (see below)
	cfg, problems := config.Load()
	if len(problems) > 0 {
		t.Fatalf("config: %v", problems)
	}
	m, err := metrics.New(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, closeAll, err := build(ctx, cfg, m, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	for _, name := range names() {
		if code, outcome := runJob(ctx, name, d, nopLog{}); code != exitOK || outcome != "success" {
			t.Errorf("%s: exit %d outcome %s", name, code, outcome)
		}
	}

	// platform-audit allows one drainer runner per process: a second
	// build with the drainer configured is refused.
	t.Setenv("PG_AUDIT_DRAINER_PASSWORD", "devpassword")
	cfg, _ = config.Load()
	for i := range 2 {
		_, closeAudit, err := build(ctx, cfg, m, nopLog{})
		if err == nil {
			closeAudit()
			if i == 1 {
				t.Error("second audit runner accepted")
			}
		} else if !strings.Contains(err.Error(), "runner already exists") {
			t.Errorf("build %d: %v", i, err)
		}
	}

	// An unreachable database fails the build.
	t.Setenv("DATABASE_URL", "postgres://billing_app:x@127.0.0.1:1/billing?sslmode=disable&connect_timeout=2")
	cfg, _ = config.Load()
	if _, _, err := build(ctx, cfg, m, nopLog{}); err == nil {
		t.Error("build succeeded without a database")
	}
}
