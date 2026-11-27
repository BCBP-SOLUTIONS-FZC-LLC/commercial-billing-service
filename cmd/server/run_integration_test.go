//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// fakeAWS answers the AWS calls the server makes at startup and while
// idle: Glue schema lookups (every produced schema registered and
// AVAILABLE), SQS queue attributes with a RedrivePolicy, DLQ URL lookups,
// and empty long polls.
func fakeAWS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		target := r.Header.Get("X-Amz-Target")
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch {
		case strings.HasSuffix(target, "GetSchemaByDefinition"):
			var in struct {
				SchemaID struct{ SchemaName string } `json:"SchemaId"`
			}
			_ = json.Unmarshal(body, &in)
			_, _ = fmt.Fprintf(w, `{"SchemaVersionId":%q,"Status":"AVAILABLE"}`, uuid.NewSHA1(uuid.NameSpaceOID, []byte(in.SchemaID.SchemaName)).String())
		case strings.HasSuffix(target, "GetQueueAttributes"):
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			redrive, _ := json.Marshal(map[string]string{"deadLetterTargetArn": "arn:aws:sqs:ap-south-1:000000000000:billing-dlq", "maxReceiveCount": "5"})
			_, _ = fmt.Fprintf(w, `{"Attributes":{"RedrivePolicy":%q,"ApproximateNumberOfMessages":"0","ApproximateNumberOfMessagesNotVisible":"0"}}`, redrive)
		case strings.HasSuffix(target, "GetQueueUrl"):
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = fmt.Fprintf(w, `{"QueueUrl":"http://%s/000000000000/billing-dlq"}`, r.Host)
		case strings.HasSuffix(target, "ReceiveMessage"):
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
			_, _ = w.Write([]byte(`{"Messages":[]}`))
		default:
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var (
	db       *testkit.DB
	hostPort string // the container's host:port on localhost
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
		return m.Run()
	}())
}

// isolateAWS gives the SDK static credentials, no shared files, and endpoint.
func isolateAWS(t *testing.T, endpoint string) {
	t.Helper()
	dir := t.TempDir()
	awsCfg := filepath.Join(dir, "config")
	if err := os.WriteFile(awsCfg, []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": awsCfg, "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"), "AWS_PROFILE": "",
		"AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test", "AWS_REGION": "ap-south-1", "AWS_ENDPOINT_URL": endpoint,
		"AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func get(url string) (int, string) {
	resp, err := http.Get(url) //nolint:gosec,noctx // test client against the local server
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The composition root in a staging-like environment: migrations at
// startup, both pools, the audit drainer, Glue-encoded billing.events
// publisher, both SQS consumers with their DLQs, the API and metrics
// listeners; SIGTERM drains everything and exits 0. A second composition
// in the same process is refused at metrics registration (exit 1).
func TestRunStagingLifecycle(t *testing.T) {
	aws := fakeAWS(t)
	isolateAWS(t, aws.URL)
	appPort, metricsPort := freePort(t), freePort(t)
	for k, v := range map[string]string{
		"APP_ENV": "staging", "APP_PORT": fmt.Sprint(appPort), "METRICS_PORT": fmt.Sprint(metricsPort),
		"DATABASE_URL":           "postgres://billing_app:devpassword@" + hostPort + "/billing?sslmode=disable",
		"MIGRATION_DATABASE_URL": "postgres://billing_migrator:devpassword@" + hostPort + "/billing?sslmode=disable",
		"RUN_MIGRATIONS":         "true", "PG_SYSTEM_PASSWORD": "devpassword", "PG_AUDIT_DRAINER_PASSWORD": "devpassword",
		"AUDIT_SOURCE_SERVICE": "commercial-billing", "AUDIT_CALLER_DOMAIN": "commercial",
		"SNS_BILLING_EVENTS_TOPIC_ARN": "arn:aws:sns:ap-south-1:000000000000:billing-events",
		"SQS_TENANT_BILLING_QUEUE_URL": aws.URL + "/000000000000/tenant-billing-q",
		"SQS_USAGE_BILLING_QUEUE_URL":  aws.URL + "/000000000000/usage-billing-q",
		"TENANT_APP_BASE_URL":          "https://app.example.com",
		"CORE_BASE_URL":                "http://127.0.0.1:1", "RP_BASE_URL": "http://127.0.0.1:1", "ENTITLEMENTS_BASE_URL": "http://127.0.0.1:1",
		"STRIPE_API_KEY": "rk_test_coverage", "STRIPE_WEBHOOK_SECRETS": "whsec_coverage", "LOG_HASH_SALT": "salt",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "", "FEATURE_DIRECT_SIGNUP": "false",
	} {
		t.Setenv(k, v)
	}

	exit := make(chan int, 1)
	go func() { exit <- run() }()
	base := fmt.Sprintf("http://127.0.0.1:%d", appPort)
	deadline := time.Now().Add(90 * time.Second)
	for {
		if code, _ := get(base + "/healthz"); code == http.StatusOK {
			break
		}
		select {
		case c := <-exit:
			t.Fatalf("run() exited with %d before serving", c)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
	}

	if code, body := get(base + "/readyz"); code != http.StatusOK || !strings.Contains(body, "sys_database") {
		t.Errorf("readyz: %d %s", code, body)
	}
	// B-W1 is mounted without authentication: an unsigned callback is 400.
	resp, err := http.Post(base+"/api/v1/webhooks/stripe", "application/json", strings.NewReader(`{}`)) //nolint:noctx // test client
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unsigned webhook: %d", resp.StatusCode)
	}
	// FEATURE_DIRECT_SIGNUP=false: B-I1 is not registered.
	resp, err = http.Post(base+"/api/v1/internal/subscriptions/authorize-and-charge", "application/json", strings.NewReader(`{}`)) //nolint:noctx // test client
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B-I1 with its flag off: %d", resp.StatusCode)
	}
	// Metrics on their own listener, with the certification gauge set by
	// the first registry refresh (no certification yet: 0).
	if code, body := get(fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort)); code != http.StatusOK ||
		!strings.Contains(body, "commercial_billing_provider_certified{") {
		t.Errorf("metrics: %d (certified gauge present: %v)", code, strings.Contains(body, "commercial_billing_provider_certified"))
	}
	if code, _ := get(fmt.Sprintf("http://127.0.0.1:%d/metrics", appPort)); code != http.StatusNotFound {
		t.Errorf("metrics served on the API port: %d", code)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-exit:
		if c != 0 {
			t.Fatalf("exit %d after SIGTERM", c)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("no shutdown after SIGTERM")
	}
	if code, _ := get(base + "/healthz"); code != 0 {
		t.Errorf("still serving after shutdown: %d", code)
	}

	// Billing's metrics are already registered in this process.
	t.Setenv("RUN_MIGRATIONS", "false")
	if c := run(); c != 1 {
		t.Errorf("second composition: exit %d, want 1", c)
	}
}

// buildBusWith outside the full server: dev publishes plain JSON and skips
// unset queues; staging refuses an unregistered Glue schema and a queue
// without a RedrivePolicy; the inbox needs the app pool.
func TestBuildBusVariants(t *testing.T) {
	ctx := context.Background()
	aws := fakeAWS(t)
	isolateAWS(t, aws.URL)
	t.Setenv("OUTBOX_PUBLISH_TIMEOUT", "0s") // Billing's 10 s applies
	base := config.Config{AppName: "commercial-billing", OutboxPollInterval: time.Second, OutboxBatchSize: 10, OutboxMaxAttempts: 3,
		SQSWaitTimeSeconds: 1, SQSVisibilityTimeout: time.Minute, SQSMaxReceiveCount: 4, GlueRegistryName: "billing-events", MaxLifecycleEventSkew: time.Minute}

	dev := base
	dev.AppEnv, dev.SNSBillingEventsTopicARN = "dev", "arn:aws:sns:ap-south-1:000000000000:billing-events"
	log := &recLog{}
	b, err := buildBusWith(ctx, dev, db.App, db.System, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if b.outbox == nil || len(b.consumers) != 0 || b.emissions == nil || !log.has("published as plain JSON") || !log.has("queue URL unset") {
		t.Errorf("dev bus: outbox=%v consumers=%d lines=%v", b.outbox != nil, len(b.consumers), log.lines)
	}
	// Both loops start and stop cleanly with the workers' stop.
	w := &workers{cancel: func() {}, done: make(chan struct{}), outbox: b.outbox, log: log}
	close(w.done)
	w.stop(time.Second)

	// Staging: a Glue registry that does not know the schemas fails startup.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"EntityNotFoundException","message":"schema not found"}`))
	}))
	defer refusing.Close()
	t.Setenv("AWS_ENDPOINT_URL", refusing.URL)
	staging := dev
	staging.AppEnv = "staging"
	if _, err := buildBusWith(ctx, staging, db.App, db.System, nil, &recLog{}); err == nil || !strings.Contains(err.Error(), "glue schema") {
		t.Errorf("unregistered schema: %v", err)
	}

	// Staging: a queue without a RedrivePolicy fails startup.
	noRedrive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "GetQueueAttributes") {
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = w.Write([]byte(`{"Attributes":{}}`))
			return
		}
		aws.Config.Handler.ServeHTTP(w, r)
	}))
	defer noRedrive.Close()
	t.Setenv("AWS_ENDPOINT_URL", noRedrive.URL)
	staging.SQSTenantBillingQueueURL = noRedrive.URL + "/000000000000/tenant-billing-q"
	if _, err := buildBusWith(ctx, staging, db.App, db.System, nil, &recLog{}); err == nil || !strings.Contains(err.Error(), "RedrivePolicy") {
		t.Errorf("no redrive policy: %v", err)
	}

	t.Setenv("AWS_ENDPOINT_URL", aws.URL)
	// No app pool: the inbox cannot be built.
	if _, err := buildBusWith(ctx, base, nil, nil, nil, &recLog{}); err == nil || !strings.Contains(err.Error(), "inbox") {
		t.Errorf("nil pool: %v", err)
	}
}
