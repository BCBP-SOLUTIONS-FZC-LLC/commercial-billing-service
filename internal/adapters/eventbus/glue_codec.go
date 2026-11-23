package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// Glue Schema Registry wire format: version byte, compression byte, 16-byte
// schema-version UUID, then the JSON payload.
const (
	glueHeaderVersion byte = 0x03
	glueNoCompression byte = 0x00
	glueHeaderSize         = 18
)

// GlueAPI is the one Glue call the codec makes.
type GlueAPI interface {
	GetSchemaByDefinition(ctx context.Context, in *glue.GetSchemaByDefinitionInput, opts ...func(*glue.Options)) (*glue.GetSchemaByDefinitionOutput, error)
}

// GlueCodec stamps each published event with the Glue schema version whose
// definition equals this binary's embedded schema (resolved once, at
// startup, by definition rather than "latest": the version describes what
// this code produces). It is applied by the SNS publisher
// (events.WithCodec); the outbox always stores plain JSON.
type GlueCodec struct {
	versions map[string]string // schema name → version UUID
}

var _ events.Codec = (*GlueCodec)(nil)

// NewGlueCodec resolves every produced schema in registry. A definition not
// yet registered (and AVAILABLE) fails startup, so a deploy that outruns
// schema registration restarts until it lands.
func NewGlueCodec(ctx context.Context, client GlueAPI, registry string) (*GlueCodec, error) {
	return newGlueCodec(ctx, client, registry, schemasFS)
}

func newGlueCodec(ctx context.Context, client GlueAPI, registry string, fsys fs.FS) (*GlueCodec, error) {
	c := &GlueCodec{versions: map[string]string{}}
	for _, name := range ProducedSchemaNames() {
		def, err := definition(fsys, name)
		if err != nil {
			return nil, err
		}
		out, err := client.GetSchemaByDefinition(ctx, &glue.GetSchemaByDefinitionInput{
			SchemaId:         &gluetypes.SchemaId{SchemaName: &name, RegistryName: &registry},
			SchemaDefinition: &def,
		})
		if err != nil {
			return nil, fmt.Errorf("glue schema %q in registry %q is not registered with this definition: %w", name, registry, err)
		}
		if out.SchemaVersionId == nil {
			return nil, fmt.Errorf("glue schema %q: no schema version id", name)
		}
		if out.Status != gluetypes.SchemaVersionStatusAvailable {
			return nil, fmt.Errorf("glue schema %q version %s is %s, not AVAILABLE", name, *out.SchemaVersionId, out.Status)
		}
		if _, err := uuid.Parse(*out.SchemaVersionId); err != nil {
			return nil, fmt.Errorf("glue schema %q: version id %q: %w", name, *out.SchemaVersionId, err)
		}
		c.versions[name] = *out.SchemaVersionId
	}
	return c, nil
}

// Encode prepends the Glue header and returns the version id for the
// envelope's dataschema.
func (c *GlueCodec) Encode(_ context.Context, eventType string, payload json.RawMessage) ([]byte, string, error) {
	id, ok := c.versions[eventType]
	if !ok {
		return nil, "", fmt.Errorf("glue encode %s: %w", eventType, ErrNoSchema)
	}
	u := uuid.MustParse(id)
	out := make([]byte, glueHeaderSize+len(payload))
	out[0], out[1] = glueHeaderVersion, glueNoCompression
	copy(out[2:glueHeaderSize], u[:])
	copy(out[glueHeaderSize:], payload)
	return out, id, nil
}

// Decode strips the Glue header.
func (c *GlueCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	if len(encoded) < glueHeaderSize {
		return nil, errors.New("glue decode: payload shorter than the 18-byte header")
	}
	if encoded[0] != glueHeaderVersion {
		return nil, fmt.Errorf("glue decode: header version 0x%02x, want 0x%02x", encoded[0], glueHeaderVersion)
	}
	return json.RawMessage(encoded[glueHeaderSize:]), nil
}

// asciiEscape writes every non-ASCII rune as \uXXXX (surrogate pairs above
// U+FFFF), as Python's ensure_ascii does. Non-ASCII occurs only inside
// JSON strings, so this is always valid JSON.
func asciiEscape(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		switch {
		case r < utf8.RuneSelf:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			_, _ = fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			_, _ = fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.String()
}
