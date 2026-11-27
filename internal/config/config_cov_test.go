package config

import (
	"testing"
	"time"
)

func TestProblemError(t *testing.T) {
	if got := (Problem{Key: "APP_PORT", Reason: "must be an integer"}).Error(); got != "APP_PORT: must be an integer" {
		t.Errorf("Error() = %q", got)
	}
}

// Load reads the process environment.
func TestLoadReadsProcessEnvironment(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	t.Setenv("APP_PORT", "8181")
	t.Setenv("FEATURE_TOKEN_ADDONS", "true")
	c, ps := Load()
	if len(ps) != 0 {
		t.Fatalf("problems: %v", ps)
	}
	if c.AppPort != 8181 || !c.FeatureTokenAddons {
		t.Errorf("port %d, token add-ons %v", c.AppPort, c.FeatureTokenAddons)
	}
}

func TestMalformedValuesRejected(t *testing.T) {
	for key, val := range map[string]string{
		"APP_PORT":                     "eighty",
		"OTEL_TRACES_SAMPLER_ARG":      "lots",
		"CHECKOUT_TTL":                 "an hour",
		"RUN_MIGRATIONS":               "maybe",
		"SNS_BILLING_EVENTS_TOPIC_ARN": "billing-events",
		"DEFAULT_CURRENCY":             "DOLLAR",
		"PUSHGATEWAY_URL":              "pushgateway:9091",
		"CORE_BASE_URL":                "not a url",
		"TENANT_APP_BASE_URL":          "http://app.example.com",
		"STRIPE_WEBHOOK_SECRETS":       "whsec_a,whsec_b,whsec_c",
	} {
		env := validProd()
		env[key] = val
		_, ps := LoadFrom(envMap(env))
		if !hasProblem(ps, key) {
			t.Errorf("%s=%q accepted (problems %v)", key, val, ps)
		}
	}
	env := validProd()
	env["STRIPE_WEBHOOK_SECRETS"] = "secret_a"
	if _, ps := LoadFrom(envMap(env)); !hasProblem(ps, "STRIPE_WEBHOOK_SECRETS") {
		t.Error("webhook secret without whsec_ accepted")
	}
	env = validProd()
	env["APP_PORT"], env["METRICS_PORT"] = "9000", "9000"
	if _, ps := LoadFrom(envMap(env)); !hasProblem(ps, "METRICS_PORT") {
		t.Error("equal ports accepted")
	}
}

// In dev an http tenant URL and a valid pushgateway are fine.
func TestDevAcceptsHTTPAndOptionalURL(t *testing.T) {
	c, ps := LoadFrom(envMap(map[string]string{
		"APP_ENV": "dev", "TENANT_APP_BASE_URL": "http://localhost:3000", "PUSHGATEWAY_URL": "http://pushgateway:9091",
		"SAGA_POLL_INTERVAL": "3s",
	}))
	if len(ps) != 0 {
		t.Fatalf("problems: %v", ps)
	}
	if c.PushgatewayURL != "http://pushgateway:9091" || c.SagaPollInterval != 3*time.Second {
		t.Errorf("pushgateway %q, saga poll %s", c.PushgatewayURL, c.SagaPollInterval)
	}
}

func TestRedactedMasksStripeKeyAndSalt(t *testing.T) {
	c, _ := LoadFrom(envMap(validProd()))
	r := c.Redacted()
	if r.StripeAPIKey != "[redacted]" || r.LogHashSalt != "[redacted]" || r.PGSystemPassword != "[redacted]" {
		t.Errorf("not redacted: %+v", r)
	}
	// An unset secret stays empty (nothing to hide, and "unset" stays visible).
	c.StripeAPIKey = ""
	if r := c.Redacted(); r.StripeAPIKey != "" {
		t.Errorf("unset key rendered as %q", r.StripeAPIKey)
	}
}
