// Package eventbus is Billing's billing.events wire layer (LLD §7.3): the
// embedded JSON Schemas extracted from api/asyncapi.yaml by schema-gov, the
// validating codec used at enqueue time, and the Glue codec applied by the
// SNS publisher.
package eventbus

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// schemasFS holds schema-gov extract's output (`make schemas`): one
// snake_case file per AsyncAPI *Payload.
//
//go:embed schemas/*.json
var schemasFS embed.FS

// Source is the envelope source of every Billing event (CD-17).
const Source = "commercial-billing"

// SpecVersion is the envelope specversion (§7.3.1).
const SpecVersion = "1"

// schemaFileNames maps every embedded file to its envelope type / Glue
// schema name. A new schema file needs one entry here; TestSchemaFiles
// fails on a file without one.
var schemaFileNames = map[string]string{
	"tenant_plan_changed.json":           string(domain.EventTenantPlanChanged),
	"tenant_seats_changed.json":          string(domain.EventTenantSeatsChanged),
	"tenant_payment_past_due.json":       string(domain.EventTenantPaymentPastDue),
	"tenant_subscription_cancelled.json": string(domain.EventTenantSubscriptionCancelled),
	"tenant_reactivated.json":            string(domain.EventTenantReactivated),
	"tenant_quota_add_on_changed.json":   string(domain.EventTenantQuotaAddOnChanged),
}

// ErrNoSchema is returned (wrapped) by Validate for a type without a schema.
var ErrNoSchema = errors.New("no schema for event type")

// Schemas is the compiled set of embedded schemas, keyed by event type.
type Schemas struct {
	byType map[string]*jsonschema.Schema
}

// LoadSchemas compiles every embedded schema. A file missing from
// schemaFileNames, or a produced type without a file, is an error: an event
// can never reach the outbox unvalidated (BIL-EVT-2).
func LoadSchemas() (*Schemas, error) { return loadSchemas(schemasFS) }

func loadSchemas(fsys fs.FS) (*Schemas, error) {
	entries, err := fs.ReadDir(fsys, "schemas")
	if err != nil {
		return nil, fmt.Errorf("read embedded schemas: %w", err)
	}
	s := &Schemas{byType: map[string]*jsonschema.Schema{}}
	compiler := jsonschema.NewCompiler()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name, ok := schemaFileNames[e.Name()]
		if !ok {
			return nil, fmt.Errorf("schema file %s has no entry in schemaFileNames", e.Name())
		}
		raw, err := fs.ReadFile(fsys, path.Join("schemas", e.Name()))
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("parse schema %s: %w", e.Name(), err)
		}
		if err := compiler.AddResource(name, doc); err != nil {
			return nil, fmt.Errorf("register schema %s: %w", e.Name(), err)
		}
		sch, err := compiler.Compile(name)
		if err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", e.Name(), err)
		}
		s.byType[name] = sch
	}
	for _, t := range domain.ProducedEventTypes {
		if _, ok := s.byType[string(t)]; !ok {
			return nil, fmt.Errorf("produced event type %s has no embedded schema", t)
		}
	}
	return s, nil
}

// Validate checks payload against eventType's schema.
func (s *Schemas) Validate(eventType string, payload []byte) error {
	sch, ok := s.byType[eventType]
	if !ok {
		return fmt.Errorf("validate %s: %w", eventType, ErrNoSchema)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("validate %s: payload is not JSON: %w", eventType, err)
	}
	if err := sch.Validate(doc); err != nil {
		return fmt.Errorf("validate %s: %w", eventType, err)
	}
	return nil
}

// ProducedSchemaNames returns the Glue schema names Billing registers in
// GLUE_REGISTRY_NAME (one per produced type), sorted.
func ProducedSchemaNames() []string {
	out := make([]string, 0, len(domain.ProducedEventTypes))
	for _, t := range domain.ProducedEventTypes {
		out = append(out, string(t))
	}
	slices.Sort(out)
	return out
}

// definition returns schemaName's embedded file as schema-gov register
// uploads it (Python json.dumps(separators=(",", ":")), ensure_ascii), so
// GetSchemaByDefinition matches byte for byte.
func definition(fsys fs.FS, schemaName string) (string, error) {
	for file, name := range schemaFileNames {
		if name != schemaName {
			continue
		}
		raw, err := fs.ReadFile(fsys, path.Join("schemas", file))
		if err != nil {
			return "", err
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return "", fmt.Errorf("compact schema %s: %w", file, err)
		}
		return asciiEscape(buf.Bytes()), nil
	}
	return "", fmt.Errorf("%w: %s", ErrNoSchema, schemaName)
}
