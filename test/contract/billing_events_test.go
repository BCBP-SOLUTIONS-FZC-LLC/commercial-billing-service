// Package contract checks Billing's produced events against its own schemas
// and the schemas its consumers validate with (LLD §7.3, §14.4).
package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

const tenant = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

func ptr[T any](v T) *T { return &v }

// samples covers every type with optional fields both present and absent.
func samples() map[string]domain.EventPayload {
	at := time.Date(2026, 11, 20, 9, 14, 3, 0, time.UTC)
	ref := "0193a8f0-1c2d-7e3f-9a4b-5c6d7e8f9a0b"
	sub := "0193a8f0-1c2d-7e3f-9a4b-000000000001"
	return map[string]domain.EventPayload{
		"plan upgrade": domain.TenantPlanChanged{TenantID: tenant, Plan: domain.PlanPro, PreviousPlan: domain.PlanStarter,
			Direction: domain.DirectionUpgrade, EffectiveAt: at, BillingRef: ref, BillingSubscriptionID: sub},
		"plan minimal": domain.TenantPlanChanged{TenantID: tenant, Plan: domain.PlanStarter, PreviousPlan: domain.PlanPro, EffectiveAt: at, BillingRef: ref},
		"seats conversion": domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 12, Reason: domain.SeatsConversion,
			BillingSubscriptionID: sub},
		"seats purchase deferred": domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 13, PreviousLicensedSeats: ptr(12),
			Reason: domain.SeatsPurchase, BillingRef: ptr(ref), DeferredAmountMinor: ptr(int64(12)), DeferralReason: domain.DeferralBelowProviderMinimum},
		"seats resync": domain.TenantSeatsChanged{TenantID: tenant, LicensedSeats: 5, Reason: domain.SeatsResync},
		"past due": domain.TenantPaymentPastDue{TenantID: tenant, InvoiceRef: ref, PastDueSince: at,
			AmountDue: &domain.Money{AmountMinor: 4900, Currency: "USD"}, NextRetryAt: ptr(at.Add(72 * time.Hour))},
		"past due minimal": domain.TenantPaymentPastDue{TenantID: tenant, InvoiceRef: ref, PastDueSince: at},
		"cancelled":        domain.TenantSubscriptionCancelled{TenantID: tenant, CancelledAt: at, Reason: domain.CancelDunningExhausted},
		"reactivated": domain.TenantReactivated{TenantID: tenant, Source: domain.ReactivationSource, ReactivatedAt: at,
			PreviousStatus: domain.StatusCancelled},
		"reactivated minimal": domain.TenantReactivated{TenantID: tenant, Source: domain.ReactivationSource, ReactivatedAt: at},
		"addon purchase": domain.TenantQuotaAddOnChanged{TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: 10_000_000,
			Reason: domain.AddonPurchase, BillingRef: ref, PurchasedAt: ptr(at), PackCode: "tokens_5m", PackQuantity: 2},
		"addon refund": domain.TenantQuotaAddOnChanged{TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: -5_000_000,
			Reason: domain.AddonRefund, BillingRef: ref, PurchaseRef: sub},
	}
}

// consumerFiles maps each produced type to the consumer schema files that
// validate it.
var consumerFiles = map[domain.EventType][]string{
	domain.EventTenantPlanChanged:  {"iam-org-membership/tenant_plan_changed.json", "iam-audit-log/billing_audit_tenant_plan_changed.json"},
	domain.EventTenantSeatsChanged: {"iam-org-membership/tenant_seats_changed.json", "iam-audit-log/billing_audit_tenant_seats_changed.json"},
	domain.EventTenantPaymentPastDue: {"iam-org-membership/tenant_payment_past_due.json",
		"iam-audit-log/billing_audit_tenant_payment_past_due.json"},
	domain.EventTenantSubscriptionCancelled: {"iam-org-membership/tenant_subscription_cancelled.json",
		"iam-audit-log/billing_audit_tenant_subscription_cancelled.json"},
	domain.EventTenantReactivated: {"iam-org-membership/tenant_reactivated.json", "iam-audit-log/billing_audit_tenant_reactivated.json",
		"iam-realm-provisioner/billing_tenant_reactivated.json"},
	domain.EventTenantQuotaAddOnChanged: {"iam-audit-log/billing_audit_tenant_quota_add_on_changed.json"},
}

func compile(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "consumers", file))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(file, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(file)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProducedPayloadsPassOwnAndConsumerSchemas(t *testing.T) {
	own, err := eventbus.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[domain.EventType]bool{}
	for name, p := range samples() {
		seen[p.EventType()] = true
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: domain validation: %v", name, err)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := own.Validate(string(p.EventType()), data); err != nil {
			t.Errorf("%s: Billing schema: %v\n%s", name, err, data)
		}
		var doc map[string]any
		_ = json.Unmarshal(data, &doc)
		if doc["tenant_id"] != tenant { // D-1: tenant_id in data as well
			t.Errorf("%s: data.tenant_id %v", name, doc["tenant_id"])
		}
		for _, f := range consumerFiles[p.EventType()] {
			v, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
			if err := compile(t, f).Validate(v); err != nil {
				t.Errorf("%s: consumer schema %s: %v", name, f, err)
			}
		}
	}
	for _, typ := range domain.ProducedEventTypes {
		if !seen[typ] {
			t.Errorf("no sample for %s", typ)
		}
	}
}

// Every consumer schema file in testdata is checked by some type.
func TestEveryConsumerSchemaIsUsed(t *testing.T) {
	used := map[string]bool{}
	for _, fs := range consumerFiles {
		for _, f := range fs {
			used[f] = true
		}
	}
	files, _ := filepath.Glob("testdata/consumers/*/*.json")
	for _, f := range files {
		rel, _ := filepath.Rel("testdata/consumers", f)
		if !used[filepath.ToSlash(rel)] {
			t.Errorf("%s is not mapped in consumerFiles", rel)
		}
	}
}
