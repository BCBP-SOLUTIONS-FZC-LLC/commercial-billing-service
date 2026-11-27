package contract

// Billing → Entitlements contract for TenantQuotaAddOnChanged (BL-OQ-4,
// BL-OQ-20; LLD §7.3.6, §14.4). Billing publishes the canonical schema
// (internal/adapters/eventbus/schemas, extracted from api/asyncapi.yaml);
// Entitlements consumes it on billing-entitlements-q with its own schema
// (testdata/consumers/commercial-entitlements) and decodes it into the
// structs mirrored below.
//
// Three checks, so CI fails on any divergence:
//  1. schema compatibility: everything Entitlements' schema requires or
//     constrains, Billing's canonical schema (or Billing's domain rules)
//     guarantees;
//  2. decode: every payload Billing produces decodes into Entitlements'
//     inbound structs and meets the rules its consumer applies before
//     acting (billing_consumer.go, service/addon.go);
//  3. producer guards: Billing's domain rejects the payloads Entitlements'
//     consumer would mishandle (no purchased_at, no or non-UUID purchase_ref).
//
// The produced payloads are also validated against Entitlements' schema by
// TestProducedPayloadsPassOwnAndConsumerSchemas (consumerFiles).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

const (
	billingAddonSchema     = "../../internal/adapters/eventbus/schemas/tenant_quota_add_on_changed.json"
	entitlementsAddonFile  = "commercial-entitlements/inbound_tenant_quota_add_on_changed.json"
	entitlementsSourceNote = "commercial-entitlements-service internal/core/domain/event.go, entitlement.go"
)

// Mirrors of Entitlements' inbound payloads (source: entitlementsSourceNote,
// commit recorded in testdata/consumers/README.md). Field names and Go types
// must match theirs: a type change on either side fails the decode test.
type entInboundAddonPurchase struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	Resource    string    `json:"resource"`
	Delta       int64     `json:"delta"`
	Reason      string    `json:"reason"`
	BillingRef  string    `json:"billing_ref"`
	PurchasedAt time.Time `json:"purchased_at"`
}

type entInboundAddonRefund struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	Resource    string    `json:"resource"`
	Delta       int64     `json:"delta"`
	Reason      string    `json:"reason"`
	BillingRef  string    `json:"billing_ref"`
	PurchaseRef uuid.UUID `json:"purchase_ref"`
}

// entResourceKeys mirrors Entitlements' billingResourceKeys: the resource
// names its consumer maps to a limit; any other is rejected.
var entResourceKeys = []string{"llm_tokens", "api_requests"}

// producerGuarantees lists consumer schema constraints that Billing's
// canonical schema does not state but Billing's domain rules enforce
// (proved by TestBillingRejectsWhatEntitlementsCannotApply).
var producerGuarantees = map[string]string{
	"purchase_ref.format": "domain.TenantQuotaAddOnChanged.Validate: refund purchase_ref is a canonical UUID",
}

type jsonSchema struct {
	Required   []string                  `json:"required"`
	Properties map[string]schemaProperty `json:"properties"`
}

type schemaProperty struct {
	Type   string          `json:"type"`
	Format string          `json:"format"`
	Enum   []string        `json:"enum"`
	Not    json.RawMessage `json:"not"`
}

func loadSchemaFile(t *testing.T, path string) jsonSchema {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return s
}

func TestEntitlementsAddonSchemaCompatible(t *testing.T) {
	producer := loadSchemaFile(t, billingAddonSchema)
	consumer := loadSchemaFile(t, filepath.Join(consumersDir(), entitlementsAddonFile))

	for _, f := range consumer.Required {
		if !slices.Contains(producer.Required, f) {
			t.Errorf("Entitlements requires %q; Billing's schema does not", f)
		}
	}
	for name, c := range consumer.Properties {
		p, ok := producer.Properties[name]
		if !ok {
			t.Errorf("Entitlements reads %q; Billing's schema does not define it", name)
			continue
		}
		if c.Type != "" && c.Type != p.Type {
			t.Errorf("%s: type %q in Billing, %q in Entitlements", name, p.Type, c.Type)
		}
		if c.Format != "" && c.Format != p.Format && producerGuarantees[name+".format"] == "" {
			t.Errorf("%s: Entitlements requires format %q; Billing's schema has %q and no domain guarantee", name, c.Format, p.Format)
		}
		if len(c.Enum) > 0 {
			if len(p.Enum) == 0 {
				t.Errorf("%s: Entitlements accepts only %v; Billing's schema allows any value", name, c.Enum)
			}
			for _, v := range p.Enum {
				if !slices.Contains(c.Enum, v) {
					t.Errorf("%s: Billing may send %q; Entitlements' schema rejects it", name, v)
				}
			}
		}
		if len(c.Not) > 0 && string(c.Not) != string(p.Not) {
			var cn, pn any
			_ = json.Unmarshal(c.Not, &cn)
			_ = json.Unmarshal(p.Not, &pn)
			if !jsonEqual(cn, pn) {
				t.Errorf("%s: Entitlements excludes %s; Billing's schema excludes %s", name, c.Not, p.Not)
			}
		}
	}
	// The resource values Billing may send must all be ones Entitlements maps.
	for _, v := range producer.Properties["resource"].Enum {
		if !slices.Contains(entResourceKeys, v) {
			t.Errorf("resource %q: Billing's schema allows it; Entitlements' consumer has no limit for it", v)
		}
	}
	// A guarantee for a constraint that is no longer a gap is stale.
	for k := range producerGuarantees {
		var name, field string
		for i := range k {
			if k[i] == '.' {
				name, field = k[:i], k[i+1:]
			}
		}
		c, p := consumer.Properties[name], producer.Properties[name]
		if field == "format" && (c.Format == "" || c.Format == p.Format) {
			t.Errorf("producerGuarantees[%q] is stale: no format gap remains; remove it", k)
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestEntitlementsDecodesProducedAddonEvents(t *testing.T) {
	n := 0
	for name, p := range samples() {
		ev, ok := p.(domain.TenantQuotaAddOnChanged)
		if !ok {
			continue
		}
		n++
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		// billing_consumer.go peeks at reason, then decodes the whole payload.
		switch ev.Reason {
		case domain.AddonPurchase:
			var got entInboundAddonPurchase
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("%s: Entitlements purchase decode: %v\n%s", name, err, data)
			}
			checkCommon(t, name, ev, got.TenantID, got.Resource, got.Delta, got.Reason, got.BillingRef)
			if got.Delta <= 0 {
				t.Errorf("%s: purchase delta %d; Entitlements skips delta ≤ 0", name, got.Delta)
			}
			// Expiry = purchased_at + 12 months (service/addon.go): a zero time
			// would expire the grant in year 2.
			if got.PurchasedAt.IsZero() || !got.PurchasedAt.Equal(*ev.PurchasedAt) {
				t.Errorf("%s: purchased_at decoded as %v, want %v", name, got.PurchasedAt, ev.PurchasedAt)
			}
		case domain.AddonRefund:
			var got entInboundAddonRefund
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("%s: Entitlements refund decode: %v\n%s", name, err, data)
			}
			checkCommon(t, name, ev, got.TenantID, got.Resource, got.Delta, got.Reason, got.BillingRef)
			if got.Delta >= 0 {
				t.Errorf("%s: refund delta %d; Entitlements skips delta ≥ 0", name, got.Delta)
			}
			if got.PurchaseRef == uuid.Nil || got.PurchaseRef.String() != ev.PurchaseRef {
				t.Errorf("%s: purchase_ref decoded as %v, want %s", name, got.PurchaseRef, ev.PurchaseRef)
			}
		default:
			t.Errorf("%s: reason %q; Entitlements acknowledges and drops it", name, ev.Reason)
		}
	}
	if n < 2 {
		t.Fatalf("want a purchase and a refund sample, got %d add-on samples", n)
	}
}

func checkCommon(t *testing.T, name string, ev domain.TenantQuotaAddOnChanged, tenantID uuid.UUID, resource string, delta int64, reason, billingRef string) {
	t.Helper()
	if tenantID.String() != ev.TenantID {
		t.Errorf("%s: tenant_id decoded as %s, want %s", name, tenantID, ev.TenantID)
	}
	if !slices.Contains(entResourceKeys, resource) {
		t.Errorf("%s: resource %q has no Entitlements limit", name, resource)
	}
	if delta != ev.Delta || reason != string(ev.Reason) {
		t.Errorf("%s: delta/reason decoded as %d/%s", name, delta, reason)
	}
	// Idempotency key on Entitlements' side (uq_quota_addons_billing_ref).
	if billingRef == "" || billingRef != ev.BillingRef {
		t.Errorf("%s: billing_ref decoded as %q, want %q", name, billingRef, ev.BillingRef)
	}
}

func TestBillingRejectsWhatEntitlementsCannotApply(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 14, 3, 0, time.UTC)
	ref := "0193a8f0-1c2d-7e3f-9a4b-5c6d7e8f9a0b"
	cases := map[string]domain.TenantQuotaAddOnChanged{
		"purchase without purchased_at": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: 5_000_000,
			Reason: domain.AddonPurchase, BillingRef: ref},
		"purchase with delta 0": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: 0,
			Reason: domain.AddonPurchase, BillingRef: ref, PurchasedAt: &at},
		"refund without purchase_ref": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: -1,
			Reason: domain.AddonRefund, BillingRef: ref},
		"refund with non-UUID purchase_ref": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: -1,
			Reason: domain.AddonRefund, BillingRef: ref, PurchaseRef: "purchase-42"},
		"refund with braced purchase_ref": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: -1,
			Reason: domain.AddonRefund, BillingRef: ref, PurchaseRef: "{" + ref + "}"},
		"refund with positive delta": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: 1,
			Reason: domain.AddonRefund, BillingRef: ref, PurchaseRef: ref},
		"unknown reason": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: 1,
			Reason: "operator_correction", BillingRef: ref},
		"no billing_ref": {TenantID: tenant, Resource: domain.ResourceLLMTokens, Delta: 1,
			Reason: domain.AddonPurchase, PurchasedAt: &at},
	}
	for name, ev := range cases {
		if err := ev.Validate(); err == nil {
			t.Errorf("%s: Billing would publish it; Entitlements cannot apply it", name)
		}
	}
}
