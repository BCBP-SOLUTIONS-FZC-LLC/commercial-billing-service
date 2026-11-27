//go:build integration

package bus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
)

var producedFiles = map[string]string{
	"TenantPlanChanged":           "tenant_plan_changed.json",
	"TenantSeatsChanged":          "tenant_seats_changed.json",
	"TenantPaymentPastDue":        "tenant_payment_past_due.json",
	"TenantSubscriptionCancelled": "tenant_subscription_cancelled.json",
	"TenantReactivated":           "tenant_reactivated.json",
	"TenantQuotaAddOnChanged":     "tenant_quota_add_on_changed.json",
}

// registry creates a Glue registry holding the produced schemas in the form
// schema-gov register uploads them (compact JSON), leaving out skip.
func registry(t *testing.T, skip string) string {
	t.Helper()
	ctx := context.Background()
	g := glueClient()
	name := unique("billing-events")
	if _, err := g.CreateRegistry(ctx, &glue.CreateRegistryInput{RegistryName: &name}); err != nil {
		t.Fatal(err)
	}
	for schema, file := range producedFiles {
		if schema == skip {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "eventbus", "schemas", file))
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := g.CreateSchema(ctx, &glue.CreateSchemaInput{
			RegistryId: &gluetypes.RegistryId{RegistryName: &name}, SchemaName: aws.String(schema),
			DataFormat: gluetypes.DataFormatJson, Compatibility: gluetypes.CompatibilityBackward,
			SchemaDefinition: aws.String(compact.String()),
		}); err != nil {
			t.Fatalf("register %s: %v", schema, err)
		}
	}
	return name
}

// The codec resolves each produced schema by definition at startup, and
// refuses to start when one is not registered (§7.2).
func TestGlueCodecResolvesByDefinition(t *testing.T) {
	ctx := context.Background()
	c, err := eventbus.NewGlueCodec(ctx, glueClient(), registry(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	enc, id, err := c.Encode(ctx, "TenantSeatsChanged", []byte(`{}`))
	if err != nil || id == "" || enc[0] != 0x03 {
		t.Fatalf("encode: %v %q", err, id)
	}
	if _, err := eventbus.NewGlueCodec(ctx, glueClient(), registry(t, "TenantQuotaAddOnChanged")); err == nil {
		t.Fatal("codec started with a produced schema missing from the registry")
	}
}
