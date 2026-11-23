package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Bus timing (§7.1, §7.3.8). The SQS handler timeout stays well inside the
// visibility timeout.
const (
	outboxPublishTimeout = 10 * time.Second
	sqsHandlerTimeout    = 20 * time.Second
	sqsDrainTimeout      = 15 * time.Second
	dlqStartupTimeout    = 5 * time.Second
)

// bus is everything on billing.events, tenant-billing-q and usage-billing-q.
type bus struct {
	events    port.EventEnqueuer
	outbox    *outbox.Runner // nil in dev without a topic
	consumers []events.Consumer
	emissions *app.EmissionWorker
}

// buildBus wires the event side of §7. Outside developer environments every
// piece is required (config validation has already checked the settings);
// in dev a missing topic or queue URL leaves that piece out with a warning.
func buildBus(ctx context.Context, cfg config.Config, appPool, sysPool *pgcommon.Pool, log port.Logger) (*bus, error) {
	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		return nil, err
	}
	b := &bus{events: pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate)}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if b.outbox, err = outboxRunner(ctx, cfg, awsCfg, appPool, log); err != nil {
		return nil, err
	}
	if b.consumers, err = sqsConsumers(ctx, cfg, awsCfg, appPool, schemas, log); err != nil {
		return nil, err
	}
	b.emissions = &app.EmissionWorker{
		Emissions: pgadapter.NewEmissionRepository(appPool, sysPool),
		Subs:      pgadapter.NewSubscriptionRepository(appPool),
		Tx:        pgadapter.NewTxRunner(appPool),
		Lock:      pgadapter.TenantLocker{},
		Events:    b.events,
		Log:       log,
	}
	return b, nil
}

func endpoint() string { return os.Getenv("AWS_ENDPOINT_URL") }

// outboxRunner builds the ordered-outbox publisher to SNS billing-events
// (BIL-EVT-2, BIL-EVT-5). The SNS codec is Glue outside developer
// environments, so every message carries the schema version of the code
// that produced it; plain JSON in dev. The outbox itself always stores
// plain, validated JSON. Glue versions are resolved here, so a deploy ahead
// of schema registration fails to start.
func outboxRunner(ctx context.Context, cfg config.Config, awsCfg aws.Config, appPool *pgcommon.Pool, log port.Logger) (*outbox.Runner, error) {
	if cfg.SNSBillingEventsTopicARN == "" {
		log.Warn("SNS_BILLING_EVENTS_TOPIC_ARN unset; outbox rows are kept but not published (dev only)", nil)
		return nil, nil
	}
	var codec events.Codec = eventbus.NoopCodec{}
	if config.IsDevLike(cfg.AppEnv) {
		log.Warn("dev environment: billing.events published as plain JSON without the Glue header", nil)
	} else {
		var opts []func(*glue.Options)
		if ep := endpoint(); ep != "" {
			opts = append(opts, func(o *glue.Options) { o.BaseEndpoint = &ep })
		}
		g, err := eventbus.NewGlueCodec(ctx, glue.NewFromConfig(awsCfg, opts...), cfg.GlueRegistryName)
		if err != nil {
			return nil, err
		}
		codec = g
	}
	pub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: cfg.SNSBillingEventsTopicARN, Region: awsCfg.Region, EndpointURL: endpoint(), Logger: log,
	}, events.WithCodec(codec))
	if err != nil {
		return nil, fmt.Errorf("sns publisher: %w", err)
	}
	return outbox.NewRunner(outbox.Config{
		Pool: appPool, Publisher: pub, Logger: log,
		PollInterval: cfg.OutboxPollInterval, BatchSize: cfg.OutboxBatchSize, MaxAttempts: cfg.OutboxMaxAttempts,
		ClaimLeaseDuration: outboxClaimLease(cfg.OutboxBatchSize), PublishTimeout: outboxPublishTimeout,
	})
}

// outboxClaimLease outlives the worst-case publish time of a claimed batch,
// so a slow batch is never re-claimed and published twice; platform-events
// requires at least BatchSize×PublishTimeout + 1 min. Floor 10 min (IAM).
func outboxClaimLease(batchSize int) time.Duration {
	return max(10*time.Minute, time.Duration(batchSize)*outboxPublishTimeout+2*time.Minute)
}

// sqsConsumers builds tenant-billing-q and usage-billing-q (§7.1).
func sqsConsumers(ctx context.Context, cfg config.Config, awsCfg aws.Config, appPool *pgcommon.Pool, schemas *eventbus.Schemas, log port.Logger) ([]events.Consumer, error) {
	var sqsOpts []func(*sqs.Options)
	if ep := endpoint(); ep != "" {
		sqsOpts = append(sqsOpts, func(o *sqs.Options) { o.BaseEndpoint = &ep })
	}
	client := sqs.NewFromConfig(awsCfg, sqsOpts...)
	dlq, err := events.NewSQSDLQPublisherWithClient(events.DLQConfig{ConsumerName: cfg.AppName, Logger: log}, client)
	if err != nil {
		return nil, fmt.Errorf("dlq publisher: %w", err)
	}
	inboxRunner, err := pgadapter.NewInboxRunner(appPool, app.ConsumerTenantBilling, app.ConsumerUsageBilling)
	if err != nil {
		return nil, err
	}
	lifecycle := &app.LifecycleConsumer{
		Inbox: inboxRunner, Lock: pgadapter.TenantLocker{}, Accounts: pgadapter.NewLifecycleRepository(appPool),
		Subs: pgadapter.NewSubscriptionRepository(appPool), MaxSkew: cfg.MaxLifecycleEventSkew, Log: log,
	}
	usage := &app.UsageConsumer{Inbox: inboxRunner, Usage: pgadapter.NewUsageRecordRepository(appPool), Log: log}

	queues := []struct {
		name, url string
		handler   events.Handler
	}{
		{"tenant-billing-q", cfg.SQSTenantBillingQueueURL, consumer.TenantBilling(lifecycle, schemas.ValidateConsumed, log)},
		{"usage-billing-q", cfg.SQSUsageBillingQueueURL, consumer.UsageBilling(usage, schemas.ValidateConsumed, log)},
	}
	var out []events.Consumer
	for _, q := range queues {
		if q.url == "" {
			log.Warn("queue URL unset; consumer not started (dev only)", map[string]any{"queue": q.name})
			continue
		}
		if err := requireDLQ(ctx, dlq, q.url, cfg, log); err != nil {
			return nil, err
		}
		c, err := events.NewSQSConsumerWithClient(events.SQSConfig{
			QueueURL: q.url, Region: awsCfg.Region, EndpointURL: endpoint(),
			WaitSeconds: int32(cfg.SQSWaitTimeSeconds), Logger: log,
		}, client, consumer.RouteRejectsToDLQ(q.handler, dlq, log),
			events.WithConsumerCodec(events.GlueDecodeCodec{}),
			events.WithDLQForwarding(dlq),
			events.WithMaxReceiveCount(cfg.SQSMaxReceiveCount),
			events.WithVisibilityTimeout(cfg.SQSVisibilityTimeout),
			events.WithHandlerTimeout(sqsHandlerTimeout),
			events.WithDrainTimeout(sqsDrainTimeout),
		)
		if err != nil {
			return nil, fmt.Errorf("%s consumer: %w", q.name, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// requireDLQ refuses to start, outside dev, on a queue whose RedrivePolicy
// is missing: dead-lettered messages would be lost. A transient lookup
// failure only warns; forwards resolve the DLQ again.
func requireDLQ(ctx context.Context, dlq events.DLQPublisher, queueURL string, cfg config.Config, log port.Logger) error {
	rctx, cancel := context.WithTimeout(ctx, dlqStartupTimeout)
	defer cancel()
	_, err := dlq.ResolveDLQ(rctx, queueURL)
	switch {
	case err == nil:
		return nil
	case (errors.Is(err, events.ErrDLQNotConfigured) || errors.Is(err, events.ErrDLQInvalidRedrivePolicy)) && !config.IsDevLike(cfg.AppEnv):
		return fmt.Errorf("queue %s has no usable RedrivePolicy: %w", queueURL, err)
	default:
		log.Warn("could not resolve the DLQ at startup; forwards resolve it again", map[string]any{"queue_url": queueURL, "error": err.Error()})
		return nil
	}
}
