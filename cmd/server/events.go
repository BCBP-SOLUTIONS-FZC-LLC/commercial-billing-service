package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	evconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Bus timing (§7.1, §7.3.8). The SQS handler timeout stays well inside the
// visibility timeout.
const (
	outboxPublishTimeout = 10 * time.Second
	sqsHandlerTimeout    = 20 * time.Second
	queueDepthInterval   = 60 * time.Second
	sqsDrainTimeout      = 15 * time.Second // also the cap: shutdown budget (lifecycle.go)
	outboxDrainTimeout   = 15 * time.Second // cap on OUTBOX_DRAIN_TIMEOUT (platform-events default 30 s)
	dlqStartupTimeout    = 5 * time.Second
)

// dlqStartupAttempts and dlqStartupBackoff retry a transient DLQ lookup at
// startup (≈ 3×5 s + 2×2 s, inside the startup probe's 120 s), so a
// deterministic RedrivePolicy problem hidden behind one throttled call
// still fails startup rather than the running consumer.
var (
	dlqStartupAttempts = 3
	dlqStartupBackoff  = 2 * time.Second
)

// bus is everything on billing.events, tenant-billing-q and usage-billing-q.
type bus struct {
	events    port.EventEnqueuer
	outbox    *outbox.Runner // nil in dev without a topic
	consumers []queueConsumer
	emissions *app.EmissionWorker
}

// queueConsumer is one SQS consumer with its queue name (liveness and the
// background-loop gauge name it).
type queueConsumer struct {
	name string
	events.Consumer
}

// buildBus wires the event side of §7 through platform-events only: its
// config package loads the SNS, SQS and outbox settings (LoadSNS, LoadSQS,
// LoadOutbox), Billing applies its validated §12 values on top (topic and
// queue names, wait, visibility, max receive, poll, batch, attempts), and
// the publisher, outbox runner, consumers and DLQ publisher are
// platform-events' own constructors. Outside developer environments every
// piece is required (config validation has already checked it); in dev a
// missing topic or queue URL leaves that piece out with a warning.
func buildBusWith(ctx context.Context, cfg config.Config, appPool, sysPool *pgcommon.Pool, conv app.ConversionConfirmer, log port.Logger) (*bus, error) {
	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		return nil, err
	}
	b := &bus{events: pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate)}
	if b.outbox, err = outboxRunner(ctx, cfg, appPool, log); err != nil {
		return nil, err
	}
	if b.consumers, err = sqsConsumers(ctx, cfg, appPool, schemas, conv, log); err != nil {
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

// snsEnv is platform-events' SNS config with Billing's topic (§12).
func snsEnv(cfg config.Config) evconfig.SNSConfigEnv {
	env := evconfig.LoadSNS()
	env.TopicARN = cfg.SNSBillingEventsTopicARN
	return env
}

// sqsEnv is platform-events' SQS config with Billing's §12 values: wait,
// visibility, max receive (below the queues' redrive 5), and the handler,
// drain and queue-depth settings when the environment leaves them unset.
func sqsEnv(cfg config.Config) evconfig.SQSConfigEnv {
	env := evconfig.LoadSQS()
	env.WaitSeconds = int32(cfg.SQSWaitTimeSeconds)
	env.VisibilityTimeout = cfg.SQSVisibilityTimeout
	env.MaxReceiveCount = cfg.SQSMaxReceiveCount
	if env.HandlerTimeout == 0 {
		env.HandlerTimeout = sqsHandlerTimeout
	}
	if env.DrainTimeout == 0 || env.DrainTimeout > sqsDrainTimeout {
		env.DrainTimeout = sqsDrainTimeout // Stop waits drain + 5 s; must fit the shutdown budget
	}
	if env.QueueDepthInterval == 0 {
		env.QueueDepthInterval = queueDepthInterval // platform_queue_depth / platform_dlq_depth (BillingDLQNotEmpty)
	}
	return env
}

// outboxRunner builds the ordered-outbox publisher to SNS billing-events
// (BIL-EVT-2, BIL-EVT-5). The SNS codec is Glue outside developer
// environments, so every message carries the schema version of the code
// that produced it; plain JSON in dev. The outbox itself always stores
// plain, validated JSON. Glue versions are resolved here, so a deploy ahead
// of schema registration fails to start.
func outboxRunner(ctx context.Context, cfg config.Config, appPool *pgcommon.Pool, log port.Logger) (*outbox.Runner, error) {
	sns := snsEnv(cfg)
	if sns.TopicARN == "" {
		log.Warn("SNS_BILLING_EVENTS_TOPIC_ARN unset; outbox rows are kept but not published (dev only)", nil)
		return nil, nil
	}
	var codec events.Codec = events.NoopCodec{}
	if config.IsDevLike(cfg.AppEnv) {
		log.Warn("dev environment: billing.events published as plain JSON without the Glue header", nil)
	} else {
		g, err := eventbus.NewGlueCodecFor(ctx, sns.Region, sns.EndpointURL, cfg.GlueRegistryName)
		if err != nil {
			return nil, err
		}
		codec = g
	}
	pub, err := events.NewSNSPublisher(evconfig.SNSConfigFromEnv(sns, log), events.WithCodec(codec))
	if err != nil {
		return nil, fmt.Errorf("sns publisher: %w", err)
	}
	ob := evconfig.LoadOutbox()
	evconfig.LogWarningsTo(log, ob.Warnings)
	ob.PollInterval, ob.BatchSize, ob.MaxAttempts = cfg.OutboxPollInterval, cfg.OutboxBatchSize, cfg.OutboxMaxAttempts
	if ob.PublishTimeout == 0 {
		ob.PublishTimeout = outboxPublishTimeout
	}
	// Stop waits up to DrainTimeout for the batch in flight; an unfinished
	// batch is re-claimed after its lease, so the cap only bounds shutdown.
	ob.DrainTimeout = min(ob.DrainTimeout, outboxDrainTimeout)
	if min := outboxClaimLease(ob.BatchSize, ob.PublishTimeout); ob.ClaimLeaseDuration < min {
		ob.ClaimLeaseDuration = min
	}
	return outbox.NewRunner(evconfig.RunnerConfigFromEnv(ob, appPool, pub, log))
}

// outboxClaimLease outlives the worst-case publish time of a claimed batch,
// so a slow batch is never re-claimed and published twice; platform-events
// requires at least BatchSize×PublishTimeout + 1 min. Floor 10 min (IAM).
func outboxClaimLease(batchSize int, publishTimeout time.Duration) time.Duration {
	return max(10*time.Minute, time.Duration(batchSize)*publishTimeout+2*time.Minute)
}

// sqsConsumers builds tenant-billing-q and usage-billing-q (§7.1) with
// platform-events' consumer and DLQ publisher (they build their own SQS
// clients from the loaded region and endpoint).
func sqsConsumers(ctx context.Context, cfg config.Config, appPool *pgcommon.Pool, schemas *eventbus.Schemas,
	conv app.ConversionConfirmer, log port.Logger) ([]queueConsumer, error) {
	env := sqsEnv(cfg)
	evconfig.LogWarningsTo(log, env.Warnings)
	dlq, err := events.NewSQSDLQPublisher(events.DLQConfig{Region: env.Region, EndpointURL: env.EndpointURL, ConsumerName: cfg.AppName, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("dlq publisher: %w", err)
	}
	inboxRunner, err := pgadapter.NewInboxRunner(appPool, app.ConsumerTenantBilling, app.ConsumerUsageBilling)
	if err != nil {
		return nil, err
	}
	lifecycle := &app.LifecycleConsumer{
		Inbox: inboxRunner, Lock: pgadapter.TenantLocker{}, Accounts: pgadapter.NewLifecycleRepository(appPool),
		Subs: pgadapter.NewSubscriptionRepository(appPool), Conversions: conv, MaxSkew: cfg.MaxLifecycleEventSkew, Log: log,
	}
	usage := &app.UsageConsumer{Inbox: inboxRunner, Usage: pgadapter.NewUsageRecordRepository(appPool), Log: log}

	queues := []struct {
		name, url string
		handler   events.Handler
	}{
		{"tenant-billing-q", cfg.SQSTenantBillingQueueURL, consumer.TenantBilling(lifecycle, schemas.ValidateConsumed, log)},
	}
	// usage-billing-q stays off until UsagePeriodClosed is ratified
	// (BL-OQ-24, USAGE_BILLING_CONSUMER_ENABLED).
	if cfg.UsageBillingConsumerEnabled {
		queues = append(queues, struct {
			name, url string
			handler   events.Handler
		}{"usage-billing-q", cfg.SQSUsageBillingQueueURL, consumer.UsageBilling(usage, schemas.ValidateConsumed, log)})
	} else {
		log.Info("usage-billing-q consumer disabled (USAGE_BILLING_CONSUMER_ENABLED=false, BL-OQ-24)", nil)
	}
	var out []queueConsumer
	for _, q := range queues {
		if q.url == "" {
			log.Warn("queue URL unset; consumer not started (dev only)", map[string]any{"queue": q.name})
			continue
		}
		forward, err := requireDLQ(ctx, dlq, q.url, cfg, log)
		if err != nil {
			return nil, err
		}
		qenv := env
		qenv.QueueURL = q.url
		opts := append(evconfig.SQSConsumerOptions(qenv), events.WithConsumerCodec(events.GlueDecodeCodec{}))
		handler := q.handler
		if forward {
			// Start re-checks the RedrivePolicy and returns on a permanent
			// failure; requireDLQ has ruled that out at startup.
			opts = append(opts, events.WithDLQForwarding(dlq))
			handler = consumer.RouteRejectsToDLQ(q.handler, dlq, log)
		}
		c, err := events.NewSQSConsumer(evconfig.SQSConfigFromEnv(qenv, log), handler, opts...)
		if err != nil {
			return nil, fmt.Errorf("%s consumer: %w", q.name, err)
		}
		out = append(out, queueConsumer{name: q.name, Consumer: c})
	}
	return out, nil
}

// requireDLQ resolves the queue's DLQ before the listener opens (§7.1,
// §13.1) and reports whether DLQ forwarding can be enabled. Outside dev a
// missing or unusable RedrivePolicy refuses startup (dead-lettered messages
// would be lost, and platform-events' consumer would stop at once on the
// same check): the deploy fails instead of the running pods. In dev that
// queue runs without DLQ forwarding, with a warning. A transient lookup
// failure is retried (dlqStartupAttempts); if it persists the consumer
// starts with forwarding on (forwards resolve the DLQ again) and a later
// permanent failure is restarted and alerted by the supervisor.
func requireDLQ(ctx context.Context, dlq events.DLQPublisher, queueURL string, cfg config.Config, log port.Logger) (bool, error) {
	var err error
	for attempt := 1; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, dlqStartupTimeout)
		_, err = dlq.ResolveDLQ(rctx, queueURL)
		cancel()
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, events.ErrDLQNotConfigured) || errors.Is(err, events.ErrDLQInvalidRedrivePolicy):
			if !config.IsDevLike(cfg.AppEnv) {
				return false, fmt.Errorf("queue %s has no usable RedrivePolicy: %w", queueURL, err)
			}
			log.Warn("queue has no usable RedrivePolicy; DLQ forwarding off for it (dev only)", map[string]any{"queue_url": queueURL, "error": err.Error()})
			return false, nil
		}
		if attempt >= dlqStartupAttempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(dlqStartupBackoff):
		}
	}
	log.Warn("could not resolve the DLQ at startup; forwards resolve it again", map[string]any{"queue_url": queueURL, "error": err.Error()})
	return true, nil //nolint:nilerr // a transient lookup failure must not refuse startup (logged above)
}
