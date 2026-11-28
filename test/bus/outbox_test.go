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
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
)

// subscribe creates a raw-delivery SNS→SQS subscription with an EventType
// filter, as the consumer IaC does (§7.3.9).
func subscribe(t *testing.T, topicARN, queueName, filter string) {
	t.Helper()
	attrs := map[string]string{"RawMessageDelivery": "true"}
	if filter != "" {
		attrs["FilterPolicy"] = filter
	}
	if _, err := snsClient().Subscribe(context.Background(), &sns.SubscribeInput{TopicArn: &topicARN, Protocol: aws.String("sqs"),
		Endpoint: aws.String(arn("sqs", queueName)), Attributes: attrs}); err != nil {
		t.Fatal(err)
	}
}

// BIL-EVT-2/5 end to end: an event committed with its change is published
// by the outbox runner to SNS billing-events, Glue-framed (§7.2), with the
// SNS attributes the subscriber filters use (§7.3.1), and reaches only the
// queues whose filter admits its type.
func TestBIL_EVT_5_OutboxPublishesGlueFramedToSNS(t *testing.T) {
	ctx := context.Background()
	topicName := unique("billing-events")
	topic, err := snsClient().CreateTopic(ctx, &sns.CreateTopicInput{Name: &topicName})
	if err != nil {
		t.Fatal(err)
	}
	orgm := unique("billing-orgm-q")
	orgmURL, _ := queueWithDLQ(t, orgm, 5)
	subscribe(t, *topic.TopicArn, orgm, `{"EventType":["TenantSeatsChanged","TenantReactivated"]}`)
	ent := unique("billing-entitlements-q")
	entURL, _ := queueWithDLQ(t, ent, 5)
	subscribe(t, *topic.TopicArn, ent, `{"EventType":["TenantQuotaAddOnChanged"]}`)

	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	codec, err := eventbus.NewGlueCodec(ctx, glueClient(), registry(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := events.NewSNSPublisher(events.SNSConfig{TopicARN: *topic.TopicArn, Region: flociRegion, EndpointURL: flociEndpoint}, events.WithCodec(codec))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := outbox.NewRunner(outbox.Config{Pool: appPool, Publisher: pub, PollInterval: 100 * time.Millisecond, BatchSize: 10, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}

	tenant := newID()
	enq := pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate)
	at := time.Now().UTC().Truncate(time.Second)
	err = pgadapter.NewTxRunner(appPool).RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
		for _, p := range []domain.EventPayload{
			domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 9, Reason: domain.SeatsReactivation},
			domain.TenantReactivated{TenantID: tenant, Source: domain.ReactivationSource, ReactivatedAt: at, PreviousStatus: domain.StatusCancelled},
		} {
			if _, err := enq.Enqueue(ctx, p, port.EventMeta{UserAgent: "commercial-billing/webhook"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	go func() { _ = runner.Start(runCtx) }()
	defer func() { cancel(); _ = runner.Stop() }()

	msgs := receive(t, orgmURL, 2, 20*time.Second)
	var types []string
	for _, m := range msgs {
		var env struct {
			ID, Type, Source, Subject, TenantID, SchemaID string
			Time                                          time.Time
			Data                                          json.RawMessage
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*m.Body), &raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw["id"], &env.ID)
		_ = json.Unmarshal(raw["type"], &env.Type)
		_ = json.Unmarshal(raw["source"], &env.Source)
		_ = json.Unmarshal(raw["subject"], &env.Subject)
		_ = json.Unmarshal(raw["tenant_id"], &env.TenantID)
		_ = json.Unmarshal(raw["dataschema"], &env.SchemaID)
		types = append(types, env.Type)

		for k, want := range map[string]string{"EventType": env.Type, "TenantID": tenant, "Source": "commercial-billing", "EventID": env.ID, "Subject": tenant} {
			if a, ok := m.MessageAttributes[k]; !ok || a.StringValue == nil || *a.StringValue != want {
				t.Errorf("%s attribute %s = %v, want %s", env.Type, k, a.StringValue, want)
			}
		}
		// data: base64 string → [0x03][0x00][version uuid][payload JSON].
		var b64 string
		if err := json.Unmarshal(raw["data"], &b64); err != nil {
			t.Fatalf("%s: data is not a codec string: %s", env.Type, raw["data"])
		}
		framed, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(framed) < 18 || framed[0] != 0x03 || framed[1] != 0x00 {
			t.Fatalf("%s: not Glue framed", env.Type)
		}
		if u, _ := uuid.FromBytes(framed[2:18]); u.String() != env.SchemaID {
			t.Errorf("%s: header version %s, dataschema %s", env.Type, u, env.SchemaID)
		}
		if err := schemas.Validate(env.Type, framed[18:]); err != nil {
			t.Errorf("%s: published payload: %v", env.Type, err)
		}
	}
	// BIL-EVT-5: seat event first, status event last, in commit order.
	if len(types) != 2 || types[0] != "TenantSeatsChanged" || types[1] != "TenantReactivated" {
		t.Errorf("order %v", types)
	}
	// The Entitlements filter admits neither type.
	if extra := drain(t, entURL); extra != 0 {
		t.Errorf("filtered queue received %d messages", extra)
	}
}
