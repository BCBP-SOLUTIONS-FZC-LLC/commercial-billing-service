//go:build integration

package bootstrap

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

var (
	db       *testkit.DB
	hostPort string // container's host:port as mapped on localhost
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		ctx := context.Background()
		d, stop, err := testkit.Start(ctx, "../..")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer stop()
		db = d
		if err := d.Super.WithConn(ctx, func(_ context.Context, c *pgcommon.Conn) error {
			cc := c.Conn().Config()
			hostPort = fmt.Sprintf("%s:%d", cc.Host, cc.Port)
			return nil
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		// platform-audit's metrics register on gincommon's registry, as in the server.
		log, err := logger.NewLogger("dev")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Logger: log, ServiceName: "commercial-billing", Domain: "commercial", Environment: "test", BuildVersion: "test"})
		return m.Run()
	}())
}

func dsn(user string, sslmode bool) string {
	s := "postgres://" + user + ":devpassword@" + hostPort + "/billing"
	if sslmode {
		s += "?sslmode=disable"
	}
	return s
}

// Pools opens both pools; without PG_SYSTEM_PASSWORD (dev) the system pool
// runs as billing_app, and pgcommon's configuration warnings are logged.
func TestPoolsWarnsInDev(t *testing.T) {
	t.Setenv("DATABASE_URL", dsn("billing_app", false))
	t.Setenv("PGSSLMODE", "disable")
	log := &recLog{}
	p, err := Pools(context.Background(), config.Config{PGSystemUser: "billing_system"}, "commercial-billing", log)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.SystemReusesApp {
		t.Error("system pool did not reuse billing_app without a password")
	}
	joined := strings.Join(log.warns, "\n")
	if !strings.Contains(joined, "postgres config warning") || !strings.Contains(joined, "PG_SYSTEM_PASSWORD not set") {
		t.Errorf("warnings %v", log.warns)
	}
}

// Audit: billing_app can enqueue; with the drainer password the runner is
// built on the billing_audit_drainer pool.
func TestAuditWithDrainer(t *testing.T) {
	t.Setenv("AUDIT_SOURCE_SERVICE", "commercial-billing")
	t.Setenv("AUDIT_CALLER_DOMAIN", "commercial")
	t.Setenv("DATABASE_URL", dsn("billing_app", true))
	cfg := config.Config{AppName: "commercial-billing", PGAuditDrainerUser: "billing_audit_drainer", PGAuditDrainerPassword: "devpassword", PGAuditDrainerMaxConns: 2}
	parts, err := Audit(context.Background(), cfg, db.App, "test", &recLog{})
	if err != nil {
		t.Fatal(err)
	}
	defer parts.Pool.Close()
	if parts.Writer == nil || parts.Runner == nil || parts.Pool == nil || parts.Pool.Name() != "audit_drainer" {
		t.Fatalf("parts %+v", parts)
	}

	// Without the drainer password (dev) entries only queue.
	log := &recLog{}
	cfg.PGAuditDrainerPassword = ""
	parts, err = Audit(context.Background(), cfg, db.App, "test", log)
	if err != nil || parts.Runner != nil || parts.Pool != nil || len(log.warns) != 1 {
		t.Fatalf("no drainer: %+v %v %v", parts, err, log.warns)
	}
}

// A pool whose role cannot insert audit entries fails startup.
func TestAuditRequiresEnqueueAccess(t *testing.T) {
	t.Setenv("AUDIT_SOURCE_SERVICE", "commercial-billing")
	t.Setenv("AUDIT_CALLER_DOMAIN", "commercial")
	ro, err := pgcommon.NewPool(context.Background(), pgcommon.Config{DSN: dsn("admin_readonly", true), PoolName: "readonly", MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	_, err = Audit(context.Background(), config.Config{AppName: "commercial-billing"}, ro, "test", &recLog{})
	if err == nil || !strings.Contains(err.Error(), "cannot enqueue audit entries") {
		t.Fatalf("err = %v", err)
	}
}

// A drainer role that cannot log in fails startup with the pool named.
func TestAuditDrainerLoginRefused(t *testing.T) {
	t.Setenv("AUDIT_SOURCE_SERVICE", "commercial-billing")
	t.Setenv("AUDIT_CALLER_DOMAIN", "commercial")
	t.Setenv("DATABASE_URL", dsn("billing_app", true))
	cfg := config.Config{AppName: "commercial-billing", PGAuditDrainerUser: "billing_audit_drainer", PGAuditDrainerPassword: "wrong", PGAuditDrainerMaxConns: 2}
	if _, err := Audit(context.Background(), cfg, db.App, "test", &recLog{}); err == nil || !strings.Contains(err.Error(), "audit drainer pool") {
		t.Fatalf("err = %v", err)
	}
	// A keyword DSN cannot carry the drainer's credentials.
	t.Setenv("DATABASE_URL", "host=localhost user=billing_app dbname=billing")
	cfg.PGAuditDrainerPassword = "devpassword"
	if _, err := Audit(context.Background(), cfg, db.App, "test", &recLog{}); err == nil || !strings.Contains(err.Error(), "not a URL") {
		t.Fatalf("keyword DSN: %v", err)
	}
}
