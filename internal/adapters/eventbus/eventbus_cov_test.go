package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// isolatedAWS points the SDK at static test credentials and no shared files.
func isolatedAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgFile, []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// NewGlueCodecFor resolves every produced schema through the Glue API at
// the configured endpoint, in the registry named.
func TestNewGlueCodecForUsesEndpointAndRegistry(t *testing.T) {
	isolatedAWS(t)
	var registries, names []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			SchemaID struct{ SchemaName, RegistryName string } `json:"SchemaId"`
		}
		_ = json.Unmarshal(body, &in)
		registries = append(registries, in.SchemaID.RegistryName)
		names = append(names, in.SchemaID.SchemaName)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"SchemaVersionId":"` + uuid.NewSHA1(uuid.NameSpaceOID, []byte(in.SchemaID.SchemaName)).String() + `","Status":"AVAILABLE"}`))
	}))
	defer srv.Close()
	c, err := NewGlueCodecFor(context.Background(), "ap-south-1", srv.URL, "billing-events")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(ProducedSchemaNames()) || registries[0] != "billing-events" {
		t.Fatalf("resolved %v in %v", names, registries)
	}
	if _, id, err := c.Encode(context.Background(), names[0], []byte(`{}`)); err != nil || id == "" {
		t.Errorf("encode after resolve: %q %v", id, err)
	}
}

// An unknown AWS profile fails startup with the SDK's error.
func TestNewGlueCodecForBadAWSConfig(t *testing.T) {
	isolatedAWS(t)
	t.Setenv("AWS_PROFILE", "no-such-profile")
	if _, err := NewGlueCodecFor(context.Background(), "", "", "billing-events"); err == nil || !strings.Contains(err.Error(), "aws config") {
		t.Fatalf("err = %v", err)
	}
}

type answer struct {
	id     *string
	status gluetypes.SchemaVersionStatus
}

func (a answer) GetSchemaByDefinition(context.Context, *glue.GetSchemaByDefinitionInput, ...func(*glue.Options)) (*glue.GetSchemaByDefinitionOutput, error) {
	return &glue.GetSchemaByDefinitionOutput{SchemaVersionId: a.id, Status: a.status}, nil
}

func TestGlueCodecRejectsBadAnswers(t *testing.T) {
	ctx := context.Background()
	notUUID := "v1"
	if _, err := NewGlueCodec(ctx, answer{status: gluetypes.SchemaVersionStatusAvailable}, "r"); err == nil || !strings.Contains(err.Error(), "no schema version id") {
		t.Errorf("nil id: %v", err)
	}
	if _, err := NewGlueCodec(ctx, answer{id: &notUUID, status: gluetypes.SchemaVersionStatusAvailable}, "r"); err == nil || !strings.Contains(err.Error(), "version id") {
		t.Errorf("non-UUID id: %v", err)
	}
	// A produced schema missing from the files fails before any Glue call.
	if _, err := newGlueCodec(ctx, answer{}, "r", fstest.MapFS{}); err == nil {
		t.Error("missing schema file accepted")
	}
}

func TestGlueDecodeRejectsHeaderVersion(t *testing.T) {
	c := &GlueCodec{versions: map[string]string{}}
	enc := make([]byte, glueHeaderSize+2)
	enc[0] = 0x02
	if _, err := c.Decode(context.Background(), "", enc); err == nil || !strings.Contains(err.Error(), "header version") {
		t.Errorf("err = %v", err)
	}
}

func TestDefinitionErrors(t *testing.T) {
	if _, err := definition(schemasFS, "NoSuchEvent"); !errors.Is(err, ErrNoSchema) {
		t.Errorf("unknown name: %v", err)
	}
	bad := fstest.MapFS{"schemas/tenant_reactivated.json": {Data: []byte(`{not json`)}}
	if _, err := definition(bad, "TenantReactivated"); err == nil || !strings.Contains(err.Error(), "compact schema") {
		t.Errorf("malformed: %v", err)
	}
}

// unreadable lists its files but fails to read them.
type unreadable struct{ fstest.MapFS }

func (unreadable) ReadFile(string) ([]byte, error) { return nil, fs.ErrPermission }

func TestLoadSchemasFileErrors(t *testing.T) {
	if _, err := loadSchemas(fstest.MapFS{}); err == nil || !strings.Contains(err.Error(), "read embedded schemas") {
		t.Errorf("no dir: %v", err)
	}
	for name, fsys := range map[string]fs.FS{
		"unreadable": unreadable{fstest.MapFS{"schemas/tenant_reactivated.json": {Data: []byte(`{}`)}}},
		"parse":      fstest.MapFS{"schemas/tenant_reactivated.json": {Data: []byte(`{`)}},
		"compile":    fstest.MapFS{"schemas/tenant_reactivated.json": {Data: []byte(`{"type":5}`)}},
	} {
		if _, err := loadSchemas(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Subdirectories and non-JSON files are skipped (then the produced set is incomplete).
	skip := fstest.MapFS{"schemas/README.md": {Data: []byte("x")}, "schemas/sub/a.json": {Data: []byte(`{}`)}}
	if _, err := loadSchemas(skip); err == nil || !strings.Contains(err.Error(), "no embedded schema") {
		t.Errorf("skip: %v", err)
	}
}

func TestValidatingCodecDecodeDelegates(t *testing.T) {
	s, err := LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewValidatingCodec(events.NoopCodec{}, s).Decode(context.Background(), "", []byte(`{"a":1}`))
	if err != nil || string(out) != `{"a":1}` {
		t.Errorf("decode: %s %v", out, err)
	}
}
