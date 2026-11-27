package contract

// Billing → Audit Log AL-5 contract (LLD §10.6, §18.3): every entry type
// Billing writes through platform-audit must be a direct-write type in Audit
// Log's taxonomy, or Audit Log answers 422 UNKNOWN_ENTRY_TYPE and the entry
// waits in pending_audit_entries as version skew. The taxonomy is vendored
// from iam-audit-log api/taxonomy.json (commit in testdata/consumers/README.md).

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// auditTaxonomyFile is the vendored taxonomy; not an event schema, so
// TestEveryConsumerSchemaIsUsed skips it.
const auditTaxonomyFile = "iam-audit-log/taxonomy.json"

// nonEventContractFiles are vendored consumer files checked by a test other
// than the event-schema one.
var nonEventContractFiles = map[string]bool{auditTaxonomyFile: true}

type auditTaxonomy struct {
	EntryTypes []struct {
		EntryType   string `json:"entry_type"`
		DirectWrite bool   `json:"direct_write"`
	} `json:"entry_types"`
}

var entryTypeLiteral = regexp.MustCompile(`EntryType:\s*"([a-z0-9_.]+)"`)

// billingEntryTypes collects every EntryType literal in Billing's production
// code under internal/.
func billingEntryTypes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	root := filepath.Join("..", "..", "internal")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range entryTypeLiteral.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAuditEntryTypesAreDirectWriteInAuditLogTaxonomy(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(consumersDir(), filepath.FromSlash(auditTaxonomyFile)))
	if err != nil {
		t.Fatal(err)
	}
	var tax auditTaxonomy
	if err := json.Unmarshal(b, &tax); err != nil {
		t.Fatal(err)
	}
	direct := map[string]bool{}
	for _, e := range tax.EntryTypes {
		direct[e.EntryType] = e.DirectWrite
	}
	types := billingEntryTypes(t)
	if len(types) < 12 {
		t.Fatalf("found only %d AL-5 entry types in internal/: %v", len(types), types)
	}
	for _, et := range types {
		ok, known := direct[et]
		switch {
		case !known:
			t.Errorf("%s: not in Audit Log's taxonomy (AL-5 would answer 422 UNKNOWN_ENTRY_TYPE)", et)
		case !ok:
			t.Errorf("%s: in Audit Log's taxonomy but not a direct-write type", et)
		}
	}
}
