package main

import (
	"context"
	"fmt"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Outbox claim lease and publish bounds (§7.3.8). The lease must outlive a
// batch's publish time; the library re-claims rows whose lease expired.
const (
	outboxClaimLease     = 2 * time.Minute
	outboxPublishTimeout = 10 * time.Second
)

// wireCodec is the codec the SNS publisher applies to each event (§7.3):
// Glue outside developer environments, so every message carries the schema
// version of the code that produced it; plain JSON in dev without a
// registry. The outbox itself always stores plain, validated JSON.
func wireCodec(ctx context.Context, cfg config.Config, log port.Logger) (events.Codec, error) {
	if config.IsDevLike(cfg.AppEnv) {
		log.Warn("dev environment: billing.events published as plain JSON without the Glue header", nil)
		return eventbus.NoopCodec{}, nil
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	var opts []func(*glue.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		opts = append(opts, func(o *glue.Options) { o.BaseEndpoint = &ep })
	}
	return eventbus.NewGlueCodec(ctx, glue.NewFromConfig(awsCfg, opts...), cfg.GlueRegistryName)
}

// outboxRunner builds the ordered-outbox publisher to SNS billing-events
// (BIL-EVT-2, BIL-EVT-5). It returns nil in a developer environment without
// a topic ARN; config validation has refused to start elsewhere.
func outboxRunner(ctx context.Context, cfg config.Config, appPool *pgcommon.Pool, log port.Logger) (*outbox.Runner, error) {
	if cfg.SNSBillingEventsTopicARN == "" {
		log.Warn("SNS_BILLING_EVENTS_TOPIC_ARN unset; outbox rows are kept but not published (dev only)", nil)
		return nil, nil
	}
	codec, err := wireCodec(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	pub, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: cfg.SNSBillingEventsTopicARN, Region: os.Getenv("AWS_REGION"), EndpointURL: os.Getenv("AWS_ENDPOINT_URL"), Logger: log,
	}, events.WithCodec(codec))
	if err != nil {
		return nil, fmt.Errorf("sns publisher: %w", err)
	}
	return outbox.NewRunner(outbox.Config{
		Pool: appPool, Publisher: pub, Logger: log,
		PollInterval: cfg.OutboxPollInterval, BatchSize: cfg.OutboxBatchSize, MaxAttempts: cfg.OutboxMaxAttempts,
		ClaimLeaseDuration: outboxClaimLease, PublishTimeout: outboxPublishTimeout,
	})
}
