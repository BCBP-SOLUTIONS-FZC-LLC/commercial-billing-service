package eventbus

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// NoopCodec passes payloads through as plain JSON: the wire codec in
// developer environments without a Glue registry.
type NoopCodec struct{}

var _ events.Codec = NoopCodec{}

// Encode returns payload unchanged and no schema id.
func (NoopCodec) Encode(_ context.Context, _ string, payload json.RawMessage) ([]byte, string, error) {
	return payload, "", nil
}

// Decode returns encoded unchanged.
func (NoopCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return encoded, nil
}

// ValidatingCodec validates a payload against its embedded schema before
// the inner codec encodes it. Fails closed: a type without a schema is an
// error.
type ValidatingCodec struct {
	inner   events.Codec
	schemas *Schemas
}

var _ events.Codec = (*ValidatingCodec)(nil)

// NewValidatingCodec wraps inner.
func NewValidatingCodec(inner events.Codec, schemas *Schemas) *ValidatingCodec {
	return &ValidatingCodec{inner: inner, schemas: schemas}
}

// Encode validates, then delegates.
func (c *ValidatingCodec) Encode(ctx context.Context, eventType string, payload json.RawMessage) ([]byte, string, error) {
	if err := c.schemas.Validate(eventType, payload); err != nil {
		return nil, "", err
	}
	return c.inner.Encode(ctx, eventType, payload)
}

// Decode delegates.
func (c *ValidatingCodec) Decode(ctx context.Context, schemaID string, encoded []byte) (json.RawMessage, error) {
	return c.inner.Decode(ctx, schemaID, encoded)
}
