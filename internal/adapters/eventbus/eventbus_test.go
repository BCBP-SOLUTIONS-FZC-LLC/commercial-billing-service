package eventbus

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
)

const tenant = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

func TestSchemasLoadAndValidate(t *testing.T) {
	s, err := LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	ok := `{"tenant_id":"` + tenant + `","cancelled_at":"2026-11-20T09:14:03Z","reason":"customer","extra":1}`
	if err := s.Validate("TenantSubscriptionCancelled", []byte(ok)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"missing tenant": `{"cancelled_at":"2026-11-20T09:14:03Z","reason":"customer"}`,
		"bad enum":       `{"tenant_id":"` + tenant + `","cancelled_at":"2026-11-20T09:14:03Z","reason":"bored"}`,
		"not json":       `{`,
	} {
		if err := s.Validate("TenantSubscriptionCancelled", []byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := s.Validate("TenantConverted", []byte(`{}`)); !errors.Is(err, ErrNoSchema) {
		t.Errorf("unknown type: %v", err)
	}
}

func TestLoadSchemasFailsClosed(t *testing.T) {
	stray := fstest.MapFS{"schemas/other.json": {Data: []byte(`{}`)}}
	if _, err := loadSchemas(stray); err == nil || !strings.Contains(err.Error(), "schemaFileNames") {
		t.Errorf("unmapped file: %v", err)
	}
	one := fstest.MapFS{"schemas/tenant_reactivated.json": {Data: []byte(`{"type":"object"}`)}}
	if _, err := loadSchemas(one); err == nil || !strings.Contains(err.Error(), "no embedded schema") {
		t.Errorf("missing produced type: %v", err)
	}
}

func TestValidatingCodec(t *testing.T) {
	s, _ := LoadSchemas()
	c := NewValidatingCodec(NoopCodec{}, s)
	if _, _, err := c.Encode(context.Background(), "TenantReactivated", []byte(`{"tenant_id":"x"}`)); err == nil {
		t.Error("invalid payload encoded")
	}
	in := `{"tenant_id":"` + tenant + `","source":"billing","reactivated_at":"2026-11-20T09:14:03Z"}`
	out, id, err := c.Encode(context.Background(), "TenantReactivated", []byte(in))
	if err != nil || string(out) != in || id != "" {
		t.Fatalf("encode: %s %q %v", out, id, err)
	}
}

type fakeGlue struct {
	status gluetypes.SchemaVersionStatus
	err    error
	defs   map[string]string
}

func (f *fakeGlue) GetSchemaByDefinition(_ context.Context, in *glue.GetSchemaByDefinitionInput, _ ...func(*glue.Options)) (*glue.GetSchemaByDefinitionOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.defs[*in.SchemaId.SchemaName] = *in.SchemaDefinition
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(*in.SchemaId.SchemaName)).String()
	return &glue.GetSchemaByDefinitionOutput{SchemaVersionId: &id, Status: f.status}, nil
}

func TestGlueCodec(t *testing.T) {
	ctx := context.Background()
	g := &fakeGlue{status: gluetypes.SchemaVersionStatusAvailable, defs: map[string]string{}}
	c, err := NewGlueCodec(ctx, g, "billing-events")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.defs) != 6 {
		t.Fatalf("resolved %d schemas", len(g.defs))
	}
	payload := []byte(`{"a":1}`)
	enc, id, err := c.Encode(ctx, "TenantPlanChanged", payload)
	if err != nil {
		t.Fatal(err)
	}
	want := uuid.NewSHA1(uuid.NameSpaceOID, []byte("TenantPlanChanged"))
	if id != want.String() || enc[0] != 0x03 || enc[1] != 0x00 || string(enc[2:18]) != string(want[:]) {
		t.Fatalf("header: id=%s % x", id, enc[:18])
	}
	dec, err := c.Decode(ctx, id, enc)
	if err != nil || string(dec) != string(payload) {
		t.Fatalf("decode: %s %v", dec, err)
	}
	if _, err := c.Decode(ctx, "", []byte{0x01}); err == nil {
		t.Error("short payload decoded")
	}
	if _, _, err := c.Encode(ctx, "TenantConverted", payload); !errors.Is(err, ErrNoSchema) {
		t.Errorf("unknown type: %v", err)
	}

	// Startup fails when a definition is not registered or not AVAILABLE.
	if _, err := NewGlueCodec(ctx, &fakeGlue{err: errors.New("EntityNotFoundException")}, "billing-events"); err == nil {
		t.Error("unregistered definition accepted")
	}
	if _, err := NewGlueCodec(ctx, &fakeGlue{status: gluetypes.SchemaVersionStatusPending, defs: map[string]string{}}, "billing-events"); err == nil {
		t.Error("PENDING version accepted")
	}
}

// The definition sent to Glue is exactly what schema-gov register uploads:
// Python json.dumps(schema, separators=(",", ":")).
func TestDefinitionMatchesSchemaGov(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	for file, name := range schemaFileNames {
		got, err := definition(schemasFS, name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(py, "-c", `import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")), end="")`,
			"schemas/"+file).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got != string(out) {
			t.Errorf("%s: definition differs from json.dumps\n got %s\nwant %s", file, got, out)
		}
	}
	if got := asciiEscape([]byte("\"\u00e9\U0001F600\"")); got != `"\u00e9\ud83d\ude00"` {
		t.Errorf("asciiEscape: %q", got)
	}
}
