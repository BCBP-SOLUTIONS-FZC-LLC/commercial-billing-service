package metrics_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
)

const inventoryPath = "docs/observability/metric-registry.md"

var constLabels = map[string]bool{"domain": true, "service": true, "environment": true}

func families(t *testing.T, legacy bool) []*dto.MetricFamily {
	t.Helper()
	registered(t)
	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg, metrics.Options{Legacy: legacy})
	if err != nil {
		t.Fatal(err)
	}
	record(t, m, reg)
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return fams
}

func labelsOf(f *dto.MetricFamily) string {
	seen := map[string]bool{}
	for _, m := range f.Metric {
		for _, l := range m.Label {
			if !constLabels[l.GetName()] {
				seen[l.GetName()] = true
			}
		}
	}
	var out []string
	for l := range seen {
		out = append(out, "`"+l+"`")
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "—"
	}
	return strings.Join(out, ", ")
}

func tier(name string) string {
	switch {
	case strings.HasPrefix(name, "platform_"):
		return "1 platform (registry)"
	case strings.HasPrefix(name, "commercial_billing_"):
		return "3 service"
	}
	return "legacy"
}

// inventory renders the observability inventory from what the code
// registers (LLD §11.2, `make metrics-inventory`).
func inventory(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(`# commercial-billing metric inventory

Generated from the code (internal/adapters/metrics) by ` + "`make metrics-inventory`" + `; CI fails when it drifts.
Enterprise Platform Observability Standard (LLD §11.2, rev 0.43): Tier 1 ` + "`platform_*`" + ` only from the
Platform Observability Registry, no Tier 2 ` + "`commercial_*`" + ` until ratified (proposals in
[registry-proposals.md](registry-proposals.md)), Billing's own series as Tier 3 ` + "`commercial_billing_*`" + `.
Every series carries the central const labels ` + "`domain=\"commercial\"`, `service=\"commercial-billing\"`, `environment`" + `
(platform-gincommon); no tenant, user, request or event id is ever a label.

Series emitted by the platform libraries (HTTP, messaging, outbox, DB pools, platform-audit) are
listed in their registries, not here.

| Metric | Type | Tier | Labels | Meaning |
|---|---|---|---|---|
`)
	for _, f := range families(t, false) {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", f.GetName(), strings.ToLower(f.GetType().String()), tier(f.GetName()), labelsOf(f),
			strings.ReplaceAll(f.GetHelp(), "|", "\\|"))
	}
	b.WriteString(`
## Deprecated (compatibility period)

Emitted in parallel with their replacements while ` + "`OBSERVABILITY_LEGACY_METRICS=true`" + ` (default during the
compatibility period). No alert, recording rule, SLO, dashboard, HPA or runbook reads them (CI). Removed after
the approved sunset.

| Deprecated name | Replacement |
|---|---|
`)
	var old []string
	for k := range metrics.Replacements {
		old = append(old, k)
	}
	sort.Strings(old)
	for _, k := range old {
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", k, metrics.Replacements[k])
	}
	return b.String()
}

// The checked-in inventory is what the code registers. UPDATE_INVENTORY=1
// rewrites it (make metrics-inventory).
func TestInventoryUpToDate(t *testing.T) {
	want := inventory(t)
	p := filepath.Join(root, inventoryPath)
	if os.Getenv("UPDATE_INVENTORY") == "1" {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != want {
		t.Fatalf("%s is out of date: run make metrics-inventory", inventoryPath)
	}
}
