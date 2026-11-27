package config

import (
	"errors"
	"testing"
	"time"
)

// usage-billing-q is off by default (BL-OQ-24): its URL is required only
// when USAGE_BILLING_CONSUMER_ENABLED=true, and validated when present.
func TestUsageBillingConsumerFlag(t *testing.T) {
	env := validProd()
	delete(env, "SQS_USAGE_BILLING_QUEUE_URL")
	c, ps := LoadFrom(envMap(env))
	if len(ps) != 0 || c.UsageBillingConsumerEnabled {
		t.Fatalf("default off without a URL: enabled=%v %v", c.UsageBillingConsumerEnabled, ps)
	}
	env["USAGE_BILLING_CONSUMER_ENABLED"] = "true"
	if _, ps := LoadFrom(envMap(env)); !hasProblem(ps, "SQS_USAGE_BILLING_QUEUE_URL") {
		t.Error("enabled without a URL must fail")
	}
	env["SQS_USAGE_BILLING_QUEUE_URL"] = "not a url"
	env["USAGE_BILLING_CONSUMER_ENABLED"] = "false"
	if _, ps := LoadFrom(envMap(env)); !hasProblem(ps, "SQS_USAGE_BILLING_QUEUE_URL") {
		t.Error("a malformed URL fails even while disabled")
	}
}

// Fail closed: inside Kubernetes an unset APP_ENV refuses to start, with
// every strict check applied, instead of defaulting to dev.
func TestAppEnvUnsetInKubernetesFailsClosed(t *testing.T) {
	_, ps := LoadFrom(envMap(map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}))
	if !hasProblem(ps, "APP_ENV") || !hasProblem(ps, "PG_SYSTEM_PASSWORD") {
		t.Fatalf("problems %v", ps)
	}
	if _, err := ResolveEnv(envMap(map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"})); !errors.Is(err, ErrEnvUnsetInKubernetes) {
		t.Errorf("ResolveEnv: %v", err)
	}
	if env, err := ResolveEnv(envMap(map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "ENVIRONMENT": "Staging"})); err != nil || env != "staging" {
		t.Errorf("ENVIRONMENT in cluster: %q %v", env, err)
	}
	if env, err := ResolveEnv(envMap(nil)); err != nil || env != "dev" {
		t.Errorf("workstation default: %q %v", env, err)
	}
}

// Required secrets per binary (§12): the server needs the drainer password,
// the log salt and, with RUN_MIGRATIONS=true, the migration DSN; jobs need
// none of those and no event bus settings.
func TestRequiredSecretsPerBinary(t *testing.T) {
	env := validProd()
	env["RUN_MIGRATIONS"] = "true"
	if _, ps := LoadFrom(envMap(env)); !hasProblem(ps, "MIGRATION_DATABASE_URL") {
		t.Error("server migrating at startup without MIGRATION_DATABASE_URL must fail")
	}
	env["MIGRATION_DATABASE_URL"] = "postgres://billing_migrator@db:5432/billing"
	if _, ps := LoadFrom(envMap(env)); len(ps) != 0 {
		t.Errorf("server with the migration DSN: %v", ps)
	}

	jobs := validProd()
	for _, k := range []string{"PG_AUDIT_DRAINER_PASSWORD", "SNS_BILLING_EVENTS_TOPIC_ARN",
		"SQS_TENANT_BILLING_QUEUE_URL", "SQS_USAGE_BILLING_QUEUE_URL"} {
		delete(jobs, k)
	}
	jobs["USAGE_BILLING_CONSUMER_ENABLED"] = "true" // irrelevant to jobs
	if _, ps := LoadFromFor(envMap(jobs), BinaryJobs); len(ps) != 0 {
		t.Errorf("jobs: %v", ps)
	}
	if _, ps := LoadFromFor(envMap(jobs), BinaryServer); !hasProblem(ps, "PG_AUDIT_DRAINER_PASSWORD") || !hasProblem(ps, "SNS_BILLING_EVENTS_TOPIC_ARN") {
		t.Errorf("server: %v", ps)
	}
	for _, k := range []string{"PG_SYSTEM_PASSWORD", "LOG_HASH_SALT", "STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRETS"} {
		e := validProd()
		delete(e, k)
		if _, ps := LoadFromFor(envMap(e), BinaryJobs); !hasProblem(ps, k) {
			t.Errorf("jobs without %s should fail", k)
		}
	}
}

func TestShutdownDrainDelay(t *testing.T) {
	c, _ := LoadFrom(envMap(nil))
	if c.ShutdownDrainDelay != 2*time.Second {
		t.Errorf("default %s", c.ShutdownDrainDelay)
	}
	if _, ps := LoadFrom(envMap(map[string]string{"SHUTDOWN_DRAIN_DELAY": "6s"})); !hasProblem(ps, "SHUTDOWN_DRAIN_DELAY") {
		t.Error("above 5s must fail (shutdown budget)")
	}
}
