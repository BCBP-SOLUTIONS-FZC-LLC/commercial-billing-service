package http

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/swagger/openapi.json")

const goldenPath = "../../../docs/swagger/openapi.json"

// docs/swagger/openapi.json is the generated document, committed.
func TestOpenAPIGolden(t *testing.T) {
	got := OpenAPI()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("docs/swagger/openapi.json is stale; run go test ./internal/adapters/http -run Golden -update")
	}
}

// Every catalogue route is in the document with its operation id.
func TestOpenAPICoversCatalogue(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, item := range doc.Paths {
		for _, op := range item {
			ids[op.OperationID] = true
		}
	}
	for _, r := range Routes() {
		if !ids[r.ID] {
			t.Errorf("%s missing", r.ID)
		}
	}
}
