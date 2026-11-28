//go:build integration

package bus_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// publishIAM publishes an RP-style event to topic: Glue-framed when framed
// (RP publishes with its Glue codec), plain JSON otherwise.
func publishIAM(t *testing.T, topicARN, typ, tenant string, at time.Time, data string, framed bool) {
	t.Helper()
	env := map[string]any{"id": uuid.Must(uuid.NewV7()).String(), "type": typ, "source": "iam-realm-provisioner", "specversion": "1",
		"tenant_id": tenant, "subject": tenant, "time": at.Format(time.RFC3339Nano), "data": json.RawMessage(data)}
	if framed {
		version := uuid.New()
		b := append([]byte{0x03, 0x00}, version[:]...)
		b = append(b, data...)
		env["data"] = base64.StdEncoding.EncodeToString(b)
		env["dataschema"] = version.String()
	}
	body, _ := json.Marshal(env)
	str := func(s string) snstypes.MessageAttributeValue {
		return snstypes.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(s)}
	}
	if _, err := snsClient().Publish(context.Background(), &sns.PublishInput{TopicArn: &topicARN, Message: aws.String(string(body)),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{"EventType": str(typ), "TenantID": str(tenant)}}); err != nil {
		t.Fatal(err)
	}
}

func lifecycleOf(t *testing.T, tenant string) string {
	t.Helper()
	var s string
	err := pgcommon.RunInTx(context.Background(), superPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		return tx.QueryRow(ctx, `SELECT iam_lifecycle::text FROM billing_accounts WHERE tenant_id = $1`, tenant).Scan(&s)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// §7.1 end to end: tenant-billing-q (filtered subscription on
// iam-tenant-events) applies Glue-framed RP events to the account; a
// future-time poison pill and a schema violation go straight to the DLQ
// (BIL-EVT-3); usage-billing-q stores period totals.
func TestBIL_OPS_2_ConsumersEndToEnd(t *testing.T) {
	ctx := context.Background()
	topicName := unique("iam-tenant-events")
	topic, err := snsClient().CreateTopic(ctx, &sns.CreateTopicInput{Name: &topicName})
	if err != nil {
		t.Fatal(err)
	}
	q := unique("tenant-billing-q")
	qURL, dlqURL := queueWithDLQ(t, q, 4)
	subscribe(t, *topic.TopicArn, q, `{"EventType":["TenantConverted","TenantSuspended","TenantReactivated","TenantOffboarded"]}`)
	uq := unique("usage-billing-q")
	uURL, _ := queueWithDLQ(t, uq, 4)

	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := pgadapter.NewInboxRunner(appPool, app.ConsumerTenantBilling, app.ConsumerUsageBilling)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &app.LifecycleConsumer{Inbox: inbox, Lock: pgadapter.TenantLocker{}, Accounts: pgadapter.NewLifecycleRepository(appPool),
		Subs: pgadapter.NewSubscriptionRepository(appPool), MaxSkew: 300 * time.Second}
	usage := &app.UsageConsumer{Inbox: inbox, Usage: pgadapter.NewUsageRecordRepository(appPool)}
	client := sqsClient()
	dlq, err := events.NewSQSDLQPublisherWithClient(events.DLQConfig{ConsumerName: "commercial-billing"}, client)
	if err != nil {
		t.Fatal(err)
	}
	start := func(url string, h events.Handler) {
		c, err := events.NewSQSConsumerWithClient(events.SQSConfig{QueueURL: url, Region: flociRegion, EndpointURL: flociEndpoint, WaitSeconds: 1},
			client, consumer.RouteRejectsToDLQ(h, dlq, nil),
			events.WithConsumerCodec(events.GlueDecodeCodec{}), events.WithDLQForwarding(dlq), events.WithMaxReceiveCount(4))
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		go func() { _ = c.Start(runCtx) }()
		t.Cleanup(func() { cancel(); _ = c.Stop() })
	}
	start(qURL, consumer.TenantBilling(lifecycle, schemas.ValidateConsumed, nil))
	start(uURL, consumer.UsageBilling(usage, schemas.ValidateConsumed, nil))

	tenant := newID()
	exec(t, `INSERT INTO billing_accounts (tenant_id, provider, provider_customer_id, currency, iam_lifecycle)
		VALUES ($1::uuid, 'stripe', 'cus_'||$1::uuid::text, 'USD', 'active')`, tenant)
	now := time.Now().UTC()

	publishIAM(t, *topic.TopicArn, "TenantSuspended", tenant, now.Add(-time.Minute),
		`{"tenant_id":"`+tenant+`","realm":"r","source":"operator","suspended_at":"`+now.Format(time.RFC3339)+`"}`, true)
	eventually(t, "suspension applied", func() bool { return lifecycleOf(t, tenant) == "suspended" })

	// Filtered out at SNS: never delivered.
	publishIAM(t, *topic.TopicArn, "TrialStarted", tenant, now, `{"tenant_id":"`+tenant+`"}`, false)
	// Poison pill (10 min ahead) and schema violation (bad source): DLQ.
	publishIAM(t, *topic.TopicArn, "TenantReactivated", tenant, now.Add(10*time.Minute),
		`{"tenant_id":"`+tenant+`","realm":"r","source":"operator"}`, true)
	publishIAM(t, *topic.TopicArn, "TenantReactivated", tenant, now,
		`{"tenant_id":"`+tenant+`","realm":"r","source":"someone"}`, false)
	dead := receive(t, dlqURL, 2, 20*time.Second)
	if len(dead) != 2 {
		t.Fatalf("dlq %d", len(dead))
	}
	if got := lifecycleOf(t, tenant); got != "suspended" {
		t.Fatalf("rejected events changed the account: %s", got)
	}

	// Reactivation, then usage on its own queue (sent directly, as SNS would).
	publishIAM(t, *topic.TopicArn, "TenantReactivated", tenant, now, `{"tenant_id":"`+tenant+`","realm":"r","source":"operator"}`, true)
	eventually(t, "reactivation applied", func() bool { return lifecycleOf(t, tenant) == "active" })

	usageTopicName := unique("usage-events")
	usageTopic, err := snsClient().CreateTopic(ctx, &sns.CreateTopicInput{Name: &usageTopicName})
	if err != nil {
		t.Fatal(err)
	}
	subscribe(t, *usageTopic.TopicArn, uq, `{"EventType":["UsagePeriodClosed"]}`)
	publishIAM(t, *usageTopic.TopicArn, "UsagePeriodClosed", tenant, now, `{"tenant_id":"`+tenant+`","period_start":"2026-10-01T00:00:00Z",
		"period_end":"2026-11-01T00:00:00Z","tokens_total":42,"api_requests_total":7,"addon_consumed":0,"limits":{"llm_tokens":500000}}`, true)
	eventually(t, "usage stored", func() bool {
		var n int
		_ = pgcommon.RunInTx(ctx, superPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM usage_period_records WHERE tenant_id = $1 AND tokens_total = 42`, tenant).Scan(&n)
		})
		return n == 1
	})
}
