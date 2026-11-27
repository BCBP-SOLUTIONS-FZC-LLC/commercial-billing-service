package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// recLog records every line by level.
type recLog struct {
	mu    sync.Mutex
	lines []string
	last  map[string]any
}

func (l *recLog) rec(level, msg string, f map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
	l.last = f
}
func (l *recLog) Debug(m string, f map[string]any) { l.rec("debug", m, f) }
func (l *recLog) Info(m string, f map[string]any)  { l.rec("info", m, f) }
func (l *recLog) Warn(m string, f map[string]any)  { l.rec("warn", m, f) }
func (l *recLog) Error(m string, f map[string]any) { l.rec("error", m, f) }

// pgcommon's log lines reach Billing's logger with their fields as a map.
func TestLoggerAdapter(t *testing.T) {
	l := &recLog{}
	a := NewLoggerAdapter(l)
	a.Debug("d", pgdomain.Field{Key: "pool", Value: "app"})
	a.Info("i")
	a.Warn("w")
	a.Error("e", pgdomain.Field{Key: "sql", Value: "SELECT 1"}, pgdomain.Field{Key: "ms", Value: 3})
	if strings.Join(l.lines, ",") != "debug d,info i,warn w,error e" || l.last["sql"] != "SELECT 1" || l.last["ms"] != 3 {
		t.Errorf("lines %v last %v", l.lines, l.last)
	}
}

func TestExplainMigrationError(t *testing.T) {
	for in, want := range map[error]string{
		pgmigrate.ErrMigrationDirty:     "left the schema dirty",
		pgmigrate.ErrVersionNotInSource: "newer than this image",
		errBoom:                         "domain migrations: boom",
	} {
		err := explainMigrationError("domain migrations", in)
		if !errors.Is(err, in) || !strings.Contains(err.Error(), want) {
			t.Errorf("%v → %v", in, err)
		}
	}
}

// An unreachable database fails the first (outbox) stage, with the stage named.
func TestMigrateSchemaUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := MigrateSchema(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", &recLog{})
	if err == nil || !strings.HasPrefix(err.Error(), "outbox schema:") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrationDSNPrefersMigrationURL(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://billing_migrator@db:5432/billing")
	if got := MigrationDSN(); got != "postgres://billing_migrator@db:5432/billing" {
		t.Errorf("MigrationDSN = %q", got)
	}
}

// A keyword DSN cannot carry another role's credentials: refused.
func TestRoleConfigNeedsURLDSN(t *testing.T) {
	t.Setenv("DATABASE_URL", "host=localhost user=billing_app dbname=billing")
	if _, err := AuditDrainerConfig(Role{User: "billing_audit_drainer", Password: "p"}, nil); err == nil || !strings.Contains(err.Error(), "not a URL") {
		t.Errorf("audit drainer: %v", err)
	}
	if _, err := OpenPools(context.Background(), PoolOptions{System: Role{User: "billing_system", Password: "p"}}); err == nil || !strings.Contains(err.Error(), "system pool") {
		t.Errorf("open pools: %v", err)
	}
}

func TestAuditDrainerConfigWithLogger(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://billing_app:x@db:5432/billing?sslmode=disable")
	cfg, err := AuditDrainerConfig(Role{User: "billing_audit_drainer", Password: "secret", MaxConns: 2}, &recLog{})
	if err != nil || cfg.Logger == nil || cfg.MinConns != 0 || cfg.MaxConns != 2 || !strings.Contains(cfg.DSN, "billing_audit_drainer:secret@") ||
		cfg.PoolName != AuditDrainerPoolName {
		t.Fatalf("cfg %+v %v", cfg, err)
	}
	if _, err := AuditDrainerConfig(Role{User: "billing_audit_drainer"}, nil); err == nil {
		t.Error("drainer without a password accepted")
	}
}

// A role pool smaller than pgcommon's minimum lowers the minimum with it.
func TestRoleConfigCapsMinConns(t *testing.T) {
	cfg, reuses, err := roleConfig(pgcommon.Config{DSN: "postgres://a:b@db/x", MinConns: 4, MaxConns: 10}, Role{User: "s", Password: "p", MaxConns: 2}, SystemPoolName)
	if err != nil || reuses || cfg.MaxConns != 2 || cfg.MinConns != 2 {
		t.Fatalf("cfg %+v reuses=%v %v", cfg, reuses, err)
	}
}

type spanRec struct{ names []string }

func (s *spanRec) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	s.names = append(s.names, name)
	return ctx, func() {}
}

// An unreachable database fails OpenPools with the pool named.
func TestOpenPoolsUnreachable(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://billing_app:x@127.0.0.1:1/billing?sslmode=disable&connect_timeout=2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := OpenPools(ctx, PoolOptions{Tracer: &spanRec{}, Log: &recLog{}})
	if err == nil {
		p.Close()
		t.Fatal("unreachable database accepted")
	}
	if !strings.HasPrefix(err.Error(), "app pool:") {
		t.Errorf("err = %v", err)
	}
}

// The prune-only outbox runner never publishes.
func TestRefusingPublisher(t *testing.T) {
	p := refusingPublisher{}
	if err := p.Publish(context.Background(), events.Envelope[json.RawMessage]{}); !errors.Is(err, errPruneOnly) {
		t.Errorf("Publish: %v", err)
	}
	if err := p.PublishBatch(context.Background(), nil); !errors.Is(err, errPruneOnly) {
		t.Errorf("PublishBatch: %v", err)
	}
}

// ── Event enqueuer ────────────────────────────────────────────────────────

type oddPayload struct {
	F        any `json:"f,omitempty"`
	invalid  bool
	noTenant bool
}

func (oddPayload) EventType() domain.EventType { return "TenantReactivated" }
func (p oddPayload) Tenant() string {
	if p.noTenant {
		return ""
	}
	return tid
}
func (p oddPayload) Validate() error {
	if p.invalid {
		return errors.New("invalid")
	}
	return nil
}

func okValidate(string, []byte) error { return nil }

func clock(at time.Time) pgcommon.Row { return fakeRow{vals: []any{at}} }

func TestEnqueueFailures(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	e := NewEventEnqueuer("commercial-billing", "1.0", okValidate)
	reactivated := domain.TenantReactivated{TenantID: tid, Source: domain.ReactivationSource, ReactivatedAt: now}

	if _, err := e.Enqueue(context.Background(), reactivated, port.EventMeta{}); !domain.HasCode(err, domain.ErrInternal) {
		t.Errorf("outside tx: %v", err)
	}
	if _, err := e.Enqueue(in(&fakeTx{}), oddPayload{invalid: true}, port.EventMeta{}); !domain.HasCode(err, domain.ErrInternal) {
		t.Errorf("invalid payload: %v", err)
	}
	if _, err := e.Enqueue(in(&fakeTx{}), oddPayload{F: func() {}}, port.EventMeta{}); err == nil || !strings.Contains(err.Error(), "marshal TenantReactivated") {
		t.Errorf("unencodable payload: %v", err)
	}
	strict := NewEventEnqueuer("commercial-billing", "1.0", func(string, []byte) error { return errors.New("schema says no") })
	if _, err := strict.Enqueue(in(&fakeTx{}), reactivated, port.EventMeta{}); !domain.HasCode(err, domain.ErrInternal) {
		t.Errorf("schema violation: %v", err)
	}
	if _, err := e.Enqueue(in(&fakeTx{}), reactivated, port.EventMeta{}); !errors.Is(err, errBoom) {
		t.Errorf("clock read: %v", err)
	}
	// The outbox refuses an event without an ordering key (the tenant).
	f := &fakeTx{row: []pgcommon.Row{clock(now)}}
	if _, err := e.Enqueue(in(f), oddPayload{noTenant: true}, port.EventMeta{UserAgent: "ua", CorrelationID: "c-1"}); err == nil || !strings.Contains(err.Error(), "ordering key") {
		t.Errorf("outbox insert: %v", err)
	}
}

// BIL-EVT-1: two events in one transaction at the same clock reading are
// 1 ms apart.
func TestNextEventTimeStrictlyIncreases(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 123456789, time.UTC)
	st := &txCtx{tx: &fakeTx{row: []pgcommon.Row{clock(now), clock(now)}}}
	a, err := nextEventTime(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	b, err := nextEventTime(context.Background(), st)
	if err != nil || !a.Equal(now.Truncate(time.Millisecond)) || b.Sub(a) != time.Millisecond {
		t.Fatalf("a %s b %s %v", a, b, err)
	}
}
