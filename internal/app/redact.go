package app

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// RedactingLogger applies the LLD §10.1 redaction list to every log line,
// whatever the caller passed (defence in depth; callers still never log card
// data). cmd/server and cmd/jobs wrap their root logger with it, so every
// layer's lines pass through it. salt is LOG_HASH_SALT (per environment).
//
// Redacted:
//   - provider secrets and signatures: keys naming a secret, password,
//     signature, API key or authorization header; values that look like a
//     Stripe secret key (sk_/rk_), a webhook secret (whsec_) or a client
//     secret (pi_…_secret_…, seti_…_secret_…);
//   - client_secret (by key and by value);
//   - payment-method ids beyond the last 4 characters (pm_…, card_…, src_…);
//   - e-mail addresses, by key (…email…) or by value: SHA-256 with the salt,
//     so the same address correlates within an environment;
//   - TenantOffboarded.export_s3_key and TenantReactivated.reason, by their
//     dedicated keys (export_s3_key, reactivation_reason).
func RedactingLogger(next port.Logger, salt string) port.Logger {
	if next == nil {
		return nopLogger{}
	}
	return redactingLogger{next: next, salt: salt}
}

type redactingLogger struct {
	next port.Logger
	salt string
}

func (l redactingLogger) Debug(msg string, f map[string]any) { l.next.Debug(msg, l.fields(f)) }
func (l redactingLogger) Info(msg string, f map[string]any)  { l.next.Info(msg, l.fields(f)) }
func (l redactingLogger) Warn(msg string, f map[string]any)  { l.next.Warn(msg, l.fields(f)) }
func (l redactingLogger) Error(msg string, f map[string]any) { l.next.Error(msg, l.fields(f)) }

const redacted = "[REDACTED]"

var (
	// Values that are a secret whatever key they sit under.
	secretValue = regexp.MustCompile(`^(sk|rk)_(live|test)_|^whsec_|_secret_`)
	// Provider payment-method ids: only the last 4 characters are kept.
	paymentMethodValue = regexp.MustCompile(`^(pm|card|src)_[A-Za-z0-9]+$`)
	emailValue         = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

func (l redactingLogger) fields(f map[string]any) map[string]any {
	if len(f) == 0 {
		return f
	}
	out := make(map[string]any, len(f))
	for k, v := range f {
		out[k] = l.value(k, v)
	}
	return out
}

func (l redactingLogger) value(key string, v any) any {
	k := strings.ToLower(key)
	switch {
	case secretKey(k), k == "export_s3_key", k == "reactivation_reason":
		return redacted
	case strings.Contains(k, "email"):
		if s, ok := v.(string); ok && s != "" {
			return l.hash(s)
		}
	case strings.Contains(k, "payment_method") || k == "pm":
		if s, ok := v.(string); ok {
			return lastFour(s)
		}
	}
	switch x := v.(type) {
	case string:
		switch {
		case secretValue.MatchString(x):
			return redacted
		case paymentMethodValue.MatchString(x):
			return lastFour(x)
		case emailValue.MatchString(x):
			return l.hash(x)
		}
	case map[string]any:
		return l.fields(x)
	}
	return v
}

func secretKey(k string) bool {
	for _, s := range []string{"secret", "password", "signature", "api_key", "apikey", "authorization", "private_key"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func (l redactingLogger) hash(s string) string {
	sum := sha256.Sum256([]byte(l.salt + strings.ToLower(strings.TrimSpace(s))))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func lastFour(s string) string {
	if len(s) <= 4 {
		return redacted
	}
	return "…" + s[len(s)-4:]
}
