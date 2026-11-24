package config

import (
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func hasProblem(ps []Problem, key string) bool {
	for _, p := range ps {
		if p.Key == key {
			return true
		}
	}
	return false
}

// validProd is a complete production environment.
func validProd() map[string]string {
	return map[string]string{
		"APP_ENV":                      "prod",
		"SNS_BILLING_EVENTS_TOPIC_ARN": "arn:aws:sns:ap-south-1:000000000000:billing-events",
		"SQS_TENANT_BILLING_QUEUE_URL": "https://sqs.ap-south-1.amazonaws.com/000000000000/tenant-billing-q",
		"SQS_USAGE_BILLING_QUEUE_URL":  "https://sqs.ap-south-1.amazonaws.com/000000000000/usage-billing-q",
		"TENANT_APP_BASE_URL":          "https://app.example.com",
		"CORE_BASE_URL":                "http://iam-org-membership.iam.svc.cluster.local:8080",
		"RP_BASE_URL":                  "http://iam-realm-provisioner.iam.svc.cluster.local:8080",
		"ENTITLEMENTS_BASE_URL":        "http://commercial-entitlements.commercial.svc.cluster.local:8080",
		"PG_SYSTEM_PASSWORD":           "system-secret",
		"PG_AUDIT_DRAINER_PASSWORD":    "drainer-secret",
		"STRIPE_API_KEY":               "rk_live_abc",
		"STRIPE_WEBHOOK_SECRETS":       "whsec_a,whsec_b",
		"LOG_HASH_SALT":                "salt",
	}
}

func TestDefaultsMatchLLD(t *testing.T) {
	c, ps := LoadFrom(envMap(map[string]string{}))
	if len(ps) != 0 {
		t.Fatalf("dev defaults should load cleanly, got %v", ps)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"AppName", c.AppName, "commercial-billing"},
		{"AppEnv", c.AppEnv, "dev"},
		{"AppPort", c.AppPort, 8080},
		{"MetricsPort", c.MetricsPort, 9090},
		{"RequestTimeout", c.RequestTimeout, 30 * time.Second},
		{"GlueRegistryName", c.GlueRegistryName, "billing-events"},
		{"SQSMaxReceiveCount", c.SQSMaxReceiveCount, 4},
		{"OutboxRetentionDays", c.OutboxRetentionDays, 8},
		{"PIIMinimiseOffboardedDays", c.PIIMinimiseOffboardedDays, 90},
		{"PIIMinimiseAbandonedDays", c.PIIMinimiseAbandonedDays, 30},
		{"DefaultPaymentProvider", c.DefaultPaymentProvider, "stripe"},
		{"ProviderCertSuiteVersion", c.ProviderCertSuiteVersion, "cert-1.0"},
		{"DefaultCurrency", c.DefaultCurrency, "USD"},
		{"RepriceCutoff", c.RepriceCutoff, time.Hour},
		{"ProrationBelowMinimum", c.ProrationBelowMinimum, "free_until_renewal"},
		{"AutoPauseOnOperatorSuspension", c.AutoPauseOnOperatorSuspension, true},
		{"AutoRevertOnEFW", c.AutoRevertOnEFW, false},
		{"EntitlementsTimeout", c.EntitlementsTimeout, time.Second},
		{"IdempotencyTTL", c.IdempotencyTTL, 72 * time.Hour},
		{"FeatureDirectSignup", c.FeatureDirectSignup, false},
		{"FeatureTokenAddons", c.FeatureTokenAddons, false},
		{"FeatureSeatPurchase", c.FeatureSeatPurchase, true},
	}
	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestProdRequiresDependencies(t *testing.T) {
	_, ps := LoadFrom(envMap(map[string]string{"APP_ENV": "prod"}))
	for _, key := range []string{
		"SNS_BILLING_EVENTS_TOPIC_ARN", "SQS_TENANT_BILLING_QUEUE_URL", "SQS_USAGE_BILLING_QUEUE_URL",
		"TENANT_APP_BASE_URL", "CORE_BASE_URL", "RP_BASE_URL", "ENTITLEMENTS_BASE_URL",
		"STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRETS", "LOG_HASH_SALT",
	} {
		if !hasProblem(ps, key) {
			t.Errorf("prod without %s should fail", key)
		}
	}
}

func TestValidProdLoads(t *testing.T) {
	_, ps := LoadFrom(envMap(validProd()))
	if len(ps) != 0 {
		t.Fatalf("valid prod config failed: %v", ps)
	}
}

func TestBIL_OPS_7_LiveAndTestKeysNeverMix(t *testing.T) {
	prod := validProd()
	prod["STRIPE_API_KEY"] = "rk_test_abc"
	if _, ps := LoadFrom(envMap(prod)); !hasProblem(ps, "STRIPE_API_KEY") {
		t.Error("prod with a test key must fail")
	}
	staging := validProd()
	staging["APP_ENV"] = "staging"
	staging["STRIPE_API_KEY"] = "rk_live_abc"
	if _, ps := LoadFrom(envMap(staging)); !hasProblem(ps, "STRIPE_API_KEY") {
		t.Error("staging with a live key must fail")
	}
	prod["STRIPE_API_KEY"] = "sk_live_abc" // unrestricted key
	if _, ps := LoadFrom(envMap(prod)); !hasProblem(ps, "STRIPE_API_KEY") {
		t.Error("an unrestricted key must fail")
	}
}

func TestRangesEnforced(t *testing.T) {
	cases := map[string]string{
		"APP_PORT":                    "80",
		"REQUEST_TIMEOUT":             "2s",
		"SQS_MAX_RECEIVE_COUNT":       "5",
		"OUTBOX_BATCH_SIZE":           "0",
		"PII_MINIMISE_ABANDONED_DAYS": "3",
		"REPRICE_CUTOFF":              "5m",
		"STRIPE_MAX_RPS":              "100",
		"IDEMPOTENCY_TTL_HOURS":       "12",
		"PRORATION_BELOW_MINIMUM":     "round_up",
		"DUNNING_FINAL_ACTION":        "mark_unpaid",
		"OTEL_TRACES_SAMPLER_ARG":     "1.5",
		"FEATURE_TOKEN_ADDONS":        "maybe",
		"SAGA_ALERT_AFTER":            "20m",
		"APP_ENV":                     "production",
		"ENABLED_PAYMENT_PROVIDERS":   "stripe,adyen",
	}
	for key, val := range cases {
		_, ps := LoadFrom(envMap(map[string]string{key: val}))
		if !hasProblem(ps, key) {
			t.Errorf("%s=%s should be rejected", key, val)
		}
	}
}

func TestDefaultProviderMustBeEnabled(t *testing.T) {
	_, ps := LoadFrom(envMap(map[string]string{"ENABLED_PAYMENT_PROVIDERS": "manual"}))
	if !hasProblem(ps, "DEFAULT_PAYMENT_PROVIDER") {
		t.Error("default provider stripe is not enabled; should fail")
	}
}

func TestFixedValues(t *testing.T) {
	for key, val := range map[string]string{
		"APP_NAME":                    "billing",
		"COMMERCIAL_SYSTEM_PRINCIPAL": "00000000-0000-0000-0000-000000000001",
	} {
		if _, ps := LoadFrom(envMap(map[string]string{key: val})); !hasProblem(ps, key) {
			t.Errorf("%s is fixed; %q should fail", key, val)
		}
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	c, _ := LoadFrom(envMap(validProd()))
	c.PGSystemPassword = "p1"
	r := c.Redacted()
	s := strings.Join([]string{r.StripeAPIKey, r.LogHashSalt, r.PGSystemPassword, strings.Join(r.StripeWebhookSecrets, ",")}, " ")
	for _, secret := range []string{"rk_live_abc", "whsec_a", "salt", "p1"} {
		if strings.Contains(s, secret) {
			t.Errorf("redacted config still contains %q", secret)
		}
	}
	if c.StripeAPIKey != "rk_live_abc" {
		t.Error("Redacted must not modify the original")
	}
}

// Outside dev the system and drainer pools must have their own role.
func TestProdRequiresRolePasswords(t *testing.T) {
	for _, key := range []string{"PG_SYSTEM_PASSWORD", "PG_AUDIT_DRAINER_PASSWORD"} {
		env := validProd()
		delete(env, key)
		if _, problems := LoadFrom(envMap(env)); !hasProblem(problems, key) {
			t.Errorf("%s missing in prod not refused", key)
		}
	}
}
