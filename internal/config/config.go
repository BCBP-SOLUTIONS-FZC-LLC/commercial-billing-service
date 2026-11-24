// Package config loads and validates the Billing service settings of LLD §12.
//
// Every value has the default and range the LLD states. Load never panics:
// it returns the populated Config together with every problem found, so the
// caller can report them all at once and refuse to start (§12: "Startup
// validates every value against its range and refuses to start on a missing
// required value, an out-of-range value or a live/test key mismatch").
//
// Required values (topic ARN, queue URLs, base URLs, Stripe secrets) are
// enforced only outside developer environments (local, dev, test), so a
// developer can run the empty service before every dependency exists. In
// staging and prod a missing required value is an error.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environments accepted for APP_ENV (§12).
var validEnvs = map[string]bool{"local": true, "dev": true, "test": true, "staging": true, "prod": true}

// IsDevLike reports whether env is a developer environment.
func IsDevLike(env string) bool { return env == "local" || env == "dev" || env == "test" }

// Config is the typed form of LLD §12. Secrets are held only as long as the
// process needs them and are never logged (see Redacted).
type Config struct {
	AppName      string
	AppEnv       string
	BuildVersion string
	AppPort      int
	MetricsPort  int

	RequestTimeout time.Duration
	RunMigrations  bool
	MigrateTimeout time.Duration

	// Database. DSN assembly itself is pgcommon.ConfigFromEnv's job (PG_HOST,
	// PG_PORT, PG_USER, ...); these are the Billing-specific pieces.
	PGSystemUser            string
	PGSystemPassword        string
	PGSystemMaxConns        int
	PGAuditDrainerUser      string
	PGAuditDrainerPassword  string
	PGAuditDrainerMaxConns  int
	MigrationDatabaseURLSet bool

	// Events.
	SNSBillingEventsTopicARN  string
	GlueRegistryName          string
	SQSTenantBillingQueueURL  string
	SQSUsageBillingQueueURL   string
	SQSMaxReceiveCount        int
	SQSWaitTimeSeconds        int
	SQSVisibilityTimeout      time.Duration
	MaxLifecycleEventSkew     time.Duration
	OutboxPollInterval        time.Duration
	OutboxBatchSize           int
	OutboxMaxAttempts         int
	OutboxRetentionDays       int
	ProcessedEventsTTLDays    int
	SeatsAfterConversionDelay time.Duration
	EmissionPollInterval      time.Duration

	// Data lifecycle (§15.3).
	PIIMinimiseOffboardedDays int
	PIIMinimiseAbandonedDays  int

	// Payment providers (§3.4, §3.5).
	DefaultPaymentProvider   string
	EnabledPaymentProviders  []string
	ProviderCertSuiteVersion string
	ProviderRegistryRefresh  time.Duration
	DefaultCurrency          string

	// Stripe (§18.8).
	StripeAPIKey         string
	StripeWebhookSecrets []string
	StripeAPIVersion     string
	StripeTimeout        time.Duration
	StripeMaxRPS         int
	StripeMaxRetries     int

	// Webhook pipeline (§8.15).
	WebhookTolerance    time.Duration
	WebhookMaxBodyBytes int64
	WebhookPollInterval time.Duration
	WebhookMaxAttempts  int

	// Billing rules.
	DunningFinalAction            string
	CheckoutTTL                   time.Duration
	TenantAppBaseURL              string
	SeatPreviewTTL                time.Duration
	PurchaseAbandonAfter          time.Duration
	SCAAbandonAfter               time.Duration
	MaxSeatsPerPurchase           int
	SeatRenewalLead               time.Duration
	SeatResyncAfter               time.Duration
	DowngradeRecheckLead          time.Duration
	RepriceCutoff                 time.Duration
	SagaPollInterval              time.Duration
	SagaAlertAfter                time.Duration
	SagaMaxBackoff                time.Duration
	ProrationBelowMinimum         string
	MinChargeMinor                int64
	AutoPauseOnOperatorSuspension bool
	// OperatorPathLive opens the operator routes outside dev and test once
	// IAM's operator path is live end to end (BIL-AUTH-6; CD-52 OP-G1..G4).
	OperatorPathLive     bool
	AutoRevertOnEFW      bool
	ReviewThresholdMinor int64
	IdempotencyTTL       time.Duration

	// Outbound HTTP dependencies.
	CoreBaseURL         string
	CoreTimeout         time.Duration
	RPBaseURL           string
	RPTimeout           time.Duration
	EntitlementsBaseURL string
	// PushgatewayURL is where each CronJob pushes its run metric (§13.2);
	// empty → the outcome is only logged (rev 0.37).
	PushgatewayURL      string
	EntitlementsTimeout time.Duration

	// Identity.
	CommercialSystemPrincipal string
	BFFCallerPrincipal        string

	// Logs and tracing.
	LogLevel       string
	LogHashSalt    string
	OTELSamplerArg float64

	// Feature flags (§12). A disabled feature answers 404 (route not registered).
	FeatureDirectSignup bool
	FeatureTokenAddons  bool
	FeatureSeatPurchase bool
}

// Problem is one invalid or missing setting.
type Problem struct {
	Key    string
	Reason string
}

func (p Problem) Error() string { return p.Key + ": " + p.Reason }

// loader accumulates problems while reading the environment.
type loader struct {
	getenv   func(string) string
	problems []Problem
	strict   bool // required values enforced
}

// Load reads the process environment.
func Load() (Config, []Problem) { return LoadFrom(os.Getenv) }

// LoadFrom reads settings through getenv (tests pass a map lookup).
func LoadFrom(getenv func(string) string) (Config, []Problem) {
	l := &loader{getenv: getenv}
	var c Config

	c.AppEnv = resolveEnv(getenv)
	if !validEnvs[c.AppEnv] {
		l.fail("APP_ENV", fmt.Sprintf("%q is not one of local|dev|test|staging|prod", c.AppEnv))
	}
	l.strict = !IsDevLike(c.AppEnv)

	c.AppName = l.str("APP_NAME", "commercial-billing")
	if c.AppName != "commercial-billing" {
		l.fail("APP_NAME", "fixed value commercial-billing (CD-17)")
	}
	c.BuildVersion = l.str("BUILD_VERSION", "")
	c.AppPort = l.intIn("APP_PORT", 8080, 1024, 65535)
	c.MetricsPort = l.intIn("METRICS_PORT", 9090, 1024, 65535)
	if c.AppPort == c.MetricsPort {
		l.fail("METRICS_PORT", "must differ from APP_PORT")
	}
	c.RequestTimeout = l.durIn("REQUEST_TIMEOUT", 30*time.Second, 5*time.Second, 60*time.Second)
	c.RunMigrations = l.boolean("RUN_MIGRATIONS", false)
	c.MigrateTimeout = l.durIn("MIGRATE_TIMEOUT", 10*time.Minute, time.Minute, 30*time.Minute)

	c.PGSystemUser = l.str("PG_SYSTEM_USER", "billing_system")
	c.PGSystemPassword = l.secret("PG_SYSTEM_PASSWORD")
	c.PGSystemMaxConns = l.intIn("PG_SYSTEM_MAX_CONNS", 4, 1, 10)
	c.PGAuditDrainerUser = l.str("PG_AUDIT_DRAINER_USER", "billing_audit_drainer")
	c.PGAuditDrainerPassword = l.secret("PG_AUDIT_DRAINER_PASSWORD")
	c.PGAuditDrainerMaxConns = l.intIn("PG_AUDIT_DRAINER_MAX_CONNS", 2, 1, 5)
	c.MigrationDatabaseURLSet = strings.TrimSpace(getenv("MIGRATION_DATABASE_URL")) != ""

	c.SNSBillingEventsTopicARN = l.required("SNS_BILLING_EVENTS_TOPIC_ARN")
	if c.SNSBillingEventsTopicARN != "" && !strings.HasPrefix(c.SNSBillingEventsTopicARN, "arn:") {
		l.fail("SNS_BILLING_EVENTS_TOPIC_ARN", "must be an ARN")
	}
	c.GlueRegistryName = l.str("GLUE_REGISTRY_NAME", "billing-events")
	c.SQSTenantBillingQueueURL = l.requiredURL("SQS_TENANT_BILLING_QUEUE_URL", false)
	c.SQSUsageBillingQueueURL = l.requiredURL("SQS_USAGE_BILLING_QUEUE_URL", false)
	c.SQSMaxReceiveCount = l.intIn("SQS_MAX_RECEIVE_COUNT", 4, 1, 4)
	c.SQSWaitTimeSeconds = l.intIn("SQS_WAIT_TIME_SECONDS", 20, 0, 20)
	c.SQSVisibilityTimeout = l.durIn("SQS_VISIBILITY_TIMEOUT", 60*time.Second, 30*time.Second, 300*time.Second)
	c.MaxLifecycleEventSkew = time.Duration(l.intIn("MAX_LIFECYCLE_EVENT_SKEW_SECONDS", 300, 60, 900)) * time.Second
	c.OutboxPollInterval = l.durIn("OUTBOX_POLL_INTERVAL", 500*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	c.OutboxBatchSize = l.intIn("OUTBOX_BATCH_SIZE", 50, 1, 500)
	c.OutboxMaxAttempts = l.intIn("OUTBOX_MAX_ATTEMPTS", 5, 1, 10)
	c.OutboxRetentionDays = l.intIn("OUTBOX_RETENTION_DAYS", 8, 7, 30)
	// PE-1: must exceed the 4-day SQS retention.
	c.ProcessedEventsTTLDays = l.intIn("PROCESSED_EVENTS_TTL_DAYS", 8, 5, 365)
	c.SeatsAfterConversionDelay = l.durIn("SEATS_AFTER_CONVERSION_DELAY", 120*time.Second, 30*time.Second, 600*time.Second)
	c.EmissionPollInterval = l.durIn("EMISSION_POLL_INTERVAL", 5*time.Second, time.Second, 30*time.Second)

	c.PIIMinimiseOffboardedDays = l.intIn("PII_MINIMISE_OFFBOARDED_DAYS", 90, 30, 365)
	c.PIIMinimiseAbandonedDays = l.intIn("PII_MINIMISE_ABANDONED_DAYS", 30, 7, 90)

	c.EnabledPaymentProviders = l.list("ENABLED_PAYMENT_PROVIDERS", []string{"stripe", "manual"})
	for _, p := range c.EnabledPaymentProviders {
		if p != "stripe" && p != "manual" {
			// Startup fails only if a listed name has no compiled adapter (§12).
			l.fail("ENABLED_PAYMENT_PROVIDERS", fmt.Sprintf("%q has no compiled adapter (stripe, manual)", p))
		}
	}
	c.DefaultPaymentProvider = l.str("DEFAULT_PAYMENT_PROVIDER", "stripe")
	if !contains(c.EnabledPaymentProviders, c.DefaultPaymentProvider) {
		l.fail("DEFAULT_PAYMENT_PROVIDER", "must be in ENABLED_PAYMENT_PROVIDERS")
	}
	c.ProviderCertSuiteVersion = l.str("PROVIDER_CERT_SUITE_VERSION", "cert-1.0")
	c.ProviderRegistryRefresh = l.durIn("PROVIDER_REGISTRY_REFRESH", 60*time.Second, 10*time.Second, 10*time.Minute)
	c.DefaultCurrency = strings.ToUpper(l.str("DEFAULT_CURRENCY", "USD"))
	if len(c.DefaultCurrency) != 3 {
		l.fail("DEFAULT_CURRENCY", "must be an ISO 4217 code")
	}

	stripeEnabled := contains(c.EnabledPaymentProviders, "stripe")
	c.StripeAPIKey = l.secret("STRIPE_API_KEY")
	if stripeEnabled {
		l.checkStripeKey(c.AppEnv, c.StripeAPIKey)
	}
	c.StripeWebhookSecrets = l.list("STRIPE_WEBHOOK_SECRETS", nil)
	if stripeEnabled {
		if len(c.StripeWebhookSecrets) == 0 && l.strict {
			l.fail("STRIPE_WEBHOOK_SECRETS", "required")
		}
		if len(c.StripeWebhookSecrets) > 2 {
			l.fail("STRIPE_WEBHOOK_SECRETS", "at most 2 (rotation overlap)")
		}
		for _, s := range c.StripeWebhookSecrets {
			if !strings.HasPrefix(s, "whsec_") {
				l.fail("STRIPE_WEBHOOK_SECRETS", "each value must start with whsec_")
				break
			}
		}
	}
	c.StripeAPIVersion = l.str("STRIPE_API_VERSION", "")
	c.StripeTimeout = time.Duration(l.intIn("STRIPE_TIMEOUT_MS", 8000, 1000, 20000)) * time.Millisecond
	c.StripeMaxRPS = l.intIn("STRIPE_MAX_RPS", 25, 1, 80)
	c.StripeMaxRetries = l.intIn("STRIPE_MAX_RETRIES", 2, 0, 3)

	c.WebhookTolerance = l.durIn("WEBHOOK_TOLERANCE", 300*time.Second, 60*time.Second, 600*time.Second)
	c.WebhookMaxBodyBytes = int64(l.intIn("WEBHOOK_MAX_BODY_BYTES", 262144, 1024, 4<<20))
	c.WebhookPollInterval = l.durIn("WEBHOOK_POLL_INTERVAL", 500*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	c.WebhookMaxAttempts = l.intIn("WEBHOOK_MAX_ATTEMPTS", 10, 3, 20)

	c.DunningFinalAction = l.oneOf("DUNNING_FINAL_ACTION", "cancel", "cancel")
	c.CheckoutTTL = l.durIn("CHECKOUT_TTL", time.Hour, 30*time.Minute, 24*time.Hour)
	c.TenantAppBaseURL = l.requiredURL("TENANT_APP_BASE_URL", true)
	c.SeatPreviewTTL = l.durIn("SEAT_PREVIEW_TTL", 10*time.Minute, 5*time.Minute, 30*time.Minute)
	c.PurchaseAbandonAfter = l.durIn("PURCHASE_ABANDON_AFTER", 30*time.Minute, 10*time.Minute, 24*time.Hour)
	c.SCAAbandonAfter = l.durIn("SCA_ABANDON_AFTER", 30*time.Minute, 10*time.Minute, 24*time.Hour)
	c.MaxSeatsPerPurchase = l.intIn("MAX_SEATS_PER_PURCHASE", 1000, 1, 10000)
	c.SeatRenewalLead = l.durIn("SEAT_RENEWAL_LEAD", 24*time.Hour, time.Hour, 72*time.Hour)
	c.SeatResyncAfter = l.durIn("SEAT_RESYNC_AFTER", 10*time.Minute, 5*time.Minute, 60*time.Minute)
	c.DowngradeRecheckLead = l.durIn("DOWNGRADE_RECHECK_LEAD", 2*time.Hour, 30*time.Minute, 24*time.Hour)
	c.RepriceCutoff = l.durIn("REPRICE_CUTOFF", time.Hour, 15*time.Minute, 24*time.Hour)
	c.SagaPollInterval = l.durIn("SAGA_POLL_INTERVAL", 2*time.Second, time.Second, 10*time.Second)
	c.SagaAlertAfter = l.durIn("SAGA_ALERT_AFTER", 30*time.Minute, 30*time.Minute, 30*time.Minute) // fixed (BIL-OPS-5)
	c.SagaMaxBackoff = l.durIn("SAGA_MAX_BACKOFF", 5*time.Minute, time.Minute, 15*time.Minute)
	c.ProrationBelowMinimum = l.oneOf("PRORATION_BELOW_MINIMUM", "free_until_renewal", "free_until_renewal", "charge_minimum")
	c.MinChargeMinor = int64(l.intIn("MIN_CHARGE_MINOR", 200, 0, 1<<31-1))
	c.AutoPauseOnOperatorSuspension = l.boolean("AUTO_PAUSE_ON_OPERATOR_SUSPENSION", true)
	c.OperatorPathLive = l.boolean("OPERATOR_PATH_LIVE", false)
	c.AutoRevertOnEFW = l.boolean("AUTO_REVERT_ON_EFW", false)
	c.ReviewThresholdMinor = int64(l.intIn("REVIEW_THRESHOLD_MINOR", 100000, 0, 1<<31-1))
	c.IdempotencyTTL = time.Duration(l.intIn("IDEMPOTENCY_TTL_HOURS", 72, 24, 168)) * time.Hour

	c.CoreBaseURL = l.requiredURL("CORE_BASE_URL", false)
	c.CoreTimeout = time.Duration(l.intIn("CORE_TIMEOUT_MS", 3000, 1000, 10000)) * time.Millisecond
	c.RPBaseURL = l.requiredURL("RP_BASE_URL", false)
	c.RPTimeout = time.Duration(l.intIn("RP_TIMEOUT_MS", 30000, 5000, 60000)) * time.Millisecond
	c.EntitlementsBaseURL = l.requiredURL("ENTITLEMENTS_BASE_URL", false)
	c.PushgatewayURL = l.optionalURL("PUSHGATEWAY_URL")
	c.EntitlementsTimeout = time.Duration(l.intIn("ENTITLEMENTS_TIMEOUT_MS", 1000, 300, 5000)) * time.Millisecond

	c.CommercialSystemPrincipal = l.str("COMMERCIAL_SYSTEM_PRINCIPAL", "00000000-0000-0000-0000-0000000000c1")
	if c.CommercialSystemPrincipal != "00000000-0000-0000-0000-0000000000c1" {
		l.fail("COMMERCIAL_SYSTEM_PRINCIPAL", "fixed value 00000000-0000-0000-0000-0000000000c1")
	}
	c.BFFCallerPrincipal = l.str("BFF_CALLER_PRINCIPAL", "cluster.local/ns/bff/sa/bff-signup")

	c.LogLevel = l.oneOf("LOG_LEVEL", "info", "debug", "info", "warn", "error")
	c.LogHashSalt = l.secret("LOG_HASH_SALT")
	if c.LogHashSalt == "" && l.strict {
		l.fail("LOG_HASH_SALT", "required")
	}
	c.OTELSamplerArg = l.floatIn("OTEL_TRACES_SAMPLER_ARG", 0.2, 0, 1)

	c.FeatureDirectSignup = l.boolean("FEATURE_DIRECT_SIGNUP", false)
	c.FeatureTokenAddons = l.boolean("FEATURE_TOKEN_ADDONS", false)
	c.FeatureSeatPurchase = l.boolean("FEATURE_SEAT_PURCHASE", true)

	return c, l.problems
}

// Redacted returns a copy safe to log: every secret replaced.
func (c Config) Redacted() Config {
	mask := func(s string) string {
		if s == "" {
			return ""
		}
		return "[redacted]"
	}
	r := c
	r.PGSystemPassword = mask(c.PGSystemPassword)
	r.PGAuditDrainerPassword = mask(c.PGAuditDrainerPassword)
	r.StripeAPIKey = mask(c.StripeAPIKey)
	r.LogHashSalt = mask(c.LogHashSalt)
	if len(c.StripeWebhookSecrets) > 0 {
		r.StripeWebhookSecrets = []string{"[redacted]"}
	}
	return r
}

// checkStripeKey enforces BIL-OPS-7: live and test keys never coexist in one
// environment; rk_live_ only in prod, rk_test_ elsewhere (§12).
func (l *loader) checkStripeKey(env, key string) {
	if key == "" {
		if l.strict {
			l.fail("STRIPE_API_KEY", "required when stripe is enabled")
		}
		return
	}
	switch {
	case env == "prod" && !strings.HasPrefix(key, "rk_live_"):
		l.fail("STRIPE_API_KEY", "prod requires a restricted live key (rk_live_) (BIL-OPS-7)")
	case env != "prod" && !strings.HasPrefix(key, "rk_test_"):
		l.fail("STRIPE_API_KEY", "non-prod environments require a restricted test key (rk_test_) (BIL-OPS-7)")
	}
}

func resolveEnv(getenv func(string) string) string {
	for _, key := range []string{"APP_ENV", "ENVIRONMENT"} {
		if v := strings.ToLower(strings.TrimSpace(getenv(key))); v != "" {
			return v
		}
	}
	return "dev"
}

func (l *loader) fail(key, reason string) {
	l.problems = append(l.problems, Problem{Key: key, Reason: reason})
}

func (l *loader) raw(key string) (string, bool) {
	v := strings.TrimSpace(l.getenv(key))
	return v, v != ""
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) secret(key string) string {
	v, _ := l.raw(key)
	return v
}

func (l *loader) required(key string) string {
	v, ok := l.raw(key)
	if !ok && l.strict {
		l.fail(key, "required")
	}
	return v
}

// optionalURL is an absolute URL or empty.
func (l *loader) optionalURL(key string) string {
	v, ok := l.raw(key)
	if !ok || v == "" {
		return ""
	}
	if u, err := url.Parse(v); err != nil || u.Scheme == "" || u.Host == "" {
		l.fail(key, "must be an absolute URL")
	}
	return v
}

func (l *loader) requiredURL(key string, httpsOnly bool) string {
	v := l.required(key)
	if v == "" {
		return v
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		l.fail(key, "must be an absolute URL")
		return v
	}
	if httpsOnly && l.strict && u.Scheme != "https" {
		l.fail(key, "must be an https URL")
	}
	return v
}

func (l *loader) intIn(key string, def, lo, hi int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, "must be an integer")
		return def
	}
	if n < lo || n > hi {
		l.fail(key, fmt.Sprintf("%d outside %d–%d", n, lo, hi))
	}
	return n
}

func (l *loader) floatIn(key string, def, lo, hi float64) float64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail(key, "must be a number")
		return def
	}
	if f < lo || f > hi {
		l.fail(key, fmt.Sprintf("%g outside %g–%g", f, lo, hi))
	}
	return f
}

func (l *loader) durIn(key string, def, lo, hi time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, "must be a duration such as 30s or 5m")
		return def
	}
	if d < lo || d > hi {
		l.fail(key, fmt.Sprintf("%s outside %s–%s", d, lo, hi))
	}
	return d
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, "must be true or false")
		return def
	}
	return b
}

func (l *loader) oneOf(key, def string, allowed ...string) string {
	v := l.str(key, def)
	if !contains(allowed, v) {
		l.fail(key, fmt.Sprintf("%q is not one of %s", v, strings.Join(allowed, "|")))
	}
	return v
}

func (l *loader) list(key string, def []string) []string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
