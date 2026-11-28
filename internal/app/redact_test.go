package app

import (
	"encoding/json"
	"strings"
	"testing"
)

type captureLogger struct{ lines []string }

func (c *captureLogger) put(level, msg string, f map[string]any) {
	b, _ := json.Marshal(map[string]any{"level": level, "msg": msg, "fields": f})
	c.lines = append(c.lines, string(b))
}
func (c *captureLogger) Debug(m string, f map[string]any) { c.put("debug", m, f) }
func (c *captureLogger) Info(m string, f map[string]any)  { c.put("info", m, f) }
func (c *captureLogger) Warn(m string, f map[string]any)  { c.put("warn", m, f) }
func (c *captureLogger) Error(m string, f map[string]any) { c.put("error", m, f) }

// Golden log lines (LLD §10.1, §14.1 "Redaction"): whatever a caller passes,
// the written line holds no secret, client_secret, e-mail address,
// export_s3_key or TenantReactivated.reason, and a payment-method id keeps
// only its last 4 characters. Operational fields pass unchanged.
func TestRedactingLoggerGoldenLines(t *testing.T) {
	c := &captureLogger{}
	log := RedactingLogger(c, "env-salt")
	log.Info("webhook received", map[string]any{
		"stripe_signature": "t=1,v1=abcdef", "Stripe-Signature": "t=1,v1=abcdef", "webhook_secret": "whsec_123",
		"provider_key": "sk_live_51Hxyz", "note": "rk_test_abc", "client_secret": "pi_3Ab_secret_Xy",
		"action": map[string]any{"type": "3ds", "secret": "pi_3Ab_secret_Xy"},
	})
	log.Warn("payment method", map[string]any{"payment_method_ref": "pm_1NabcDEFGhijk", "default": "pm_9ZZZZwxyz", "card": "card_12345678"})
	log.Error("contact", map[string]any{"billing_contact_email": "Jane.Doe@Example.com", "contact": "jane.doe@example.com"})
	log.Info("lifecycle", map[string]any{"export_s3_key": "s3://bucket/tenant/export.zip", "reactivation_reason": "Customer called support about …"})
	log.Info("operational", map[string]any{"tenant_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6", "reason": "paid_after_end",
		"alert": "BillingPaymentParked", "amount_minor": int64(20700), "tokens_total": 5000000, "key": "seatpur:abc:void"})

	all := strings.Join(c.lines, "\n")
	for _, leak := range []string{"abcdef", "whsec_123", "sk_live_51Hxyz", "rk_test_abc", "_secret_", "pm_1NabcDEFGhijk", "pm_9ZZZZwxyz",
		"card_12345678", "Jane.Doe", "jane.doe@example.com", "export.zip", "Customer called"} {
		if strings.Contains(all, leak) {
			t.Errorf("leaked %q in:\n%s", leak, all)
		}
	}
	for _, kept := range []string{"…hijk", "…wxyz", "…5678", `"reason":"paid_after_end"`, `"alert":"BillingPaymentParked"`,
		`"amount_minor":20700`, `"tokens_total":5000000`, `"key":"seatpur:abc:void"`, "3fa85f64-5717-4562-b3fc-2c963f66afa6"} {
		if !strings.Contains(all, kept) {
			t.Errorf("missing %q in:\n%s", kept, all)
		}
	}
	// The same address hashes to the same value (correlation within an
	// environment), and differently with another salt.
	a, b := RedactingLogger(c, "env-salt").(redactingLogger).hash("Jane.Doe@Example.com"), RedactingLogger(c, "env-salt").(redactingLogger).hash(" jane.doe@example.com ")
	other := RedactingLogger(c, "other-salt").(redactingLogger).hash("jane.doe@example.com")
	if a != b || a == other || !strings.HasPrefix(a, "sha256:") {
		t.Errorf("hash: %s %s %s", a, b, other)
	}
}
