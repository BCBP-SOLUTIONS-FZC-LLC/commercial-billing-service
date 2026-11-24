package main

import (
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
)

// platform-events refuses a lease below BatchSize×PublishTimeout + 1 min.
func TestOutboxClaimLease(t *testing.T) {
	for _, batch := range []int{1, 50, 500} {
		if got, min := outboxClaimLease(batch, outboxPublishTimeout), time.Duration(batch)*outboxPublishTimeout+time.Minute; got < min || got < 10*time.Minute {
			t.Errorf("batch %d: lease %s below %s", batch, got, min)
		}
	}
}

// Billing's §12 values are applied on top of platform-events' SQS config,
// and the queue-depth sampler BillingDLQNotEmpty reads is on by default.
func TestSQSEnvAppliesBillingValues(t *testing.T) {
	t.Setenv("SQS_VISIBILITY_TIMEOUT", "")
	t.Setenv("SQS_QUEUE_DEPTH_INTERVAL", "")
	cfg := config.Config{SQSWaitTimeSeconds: 20, SQSVisibilityTimeout: 60 * time.Second, SQSMaxReceiveCount: 4}
	env := sqsEnv(cfg)
	if env.WaitSeconds != 20 || env.VisibilityTimeout != 60*time.Second || env.MaxReceiveCount != 4 {
		t.Errorf("billing values not applied: %+v", env)
	}
	if env.HandlerTimeout != sqsHandlerTimeout || env.DrainTimeout != sqsDrainTimeout || env.QueueDepthInterval != queueDepthInterval {
		t.Errorf("defaults not applied: %+v", env)
	}
	t.Setenv("SQS_QUEUE_DEPTH_INTERVAL", "30s")
	if got := sqsEnv(cfg).QueueDepthInterval; got != 30*time.Second {
		t.Errorf("platform-events env value overridden: %s", got)
	}
}

func TestSNSEnvUsesBillingTopic(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:1:other")
	if got := snsEnv(config.Config{SNSBillingEventsTopicARN: "arn:aws:sns:ap-south-1:1:billing-events"}).TopicARN; got != "arn:aws:sns:ap-south-1:1:billing-events" {
		t.Errorf("topic %s", got)
	}
}
