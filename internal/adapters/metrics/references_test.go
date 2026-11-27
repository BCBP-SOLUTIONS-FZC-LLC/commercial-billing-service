package metrics_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/obsregistry"
)

const root = "../../.."

// referenceFiles are everything that names a metric: alerting, recording
// and SLO rules, the dashboard, the HPA (chart values) and the runbooks.
func referenceFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"deploy/monitoring", "docs/runbooks"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".md")) &&
				!strings.HasSuffix(p, "platform-audit.rules.yml") { // vendored platform-audit rules
				files = append(files, p)
			}
			return nil
		})
	}
	return append(files, filepath.Join(root, "deploy/helm/values.yaml"))
}

// otherServices are Tier 3 prefixes of other Commercial services that
// Billing's rules may read (composed alerts).
var otherServices = []string{"commercial_usage_metering_", "commercial_entitlements_"}

var metricName = regexp.MustCompile(`\bcommercial_[a-z0-9_]+\b`)

// Every commercial_* name a rule, dashboard, HPA or runbook reads is one
// Billing exposes (Tier 3) or another Commercial service's Tier 3 — never an
// unratified Tier 2 name (standard rules 10–11).
func TestReferencesUseCompliantNames(t *testing.T) {
	exposed := scrapeBilling(t)
	for _, f := range referenceFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range metricName.FindAllStringIndex(string(b), -1) {
			name := string(b[loc[0]:loc[1]])
			// Not metric names: recording-rule levels (commercial_billing:…),
			// rule-group names (commercial_billing.slo), wildcards in prose
			// (commercial_billing_*).
			next := func(i int) byte {
				if loc[1]+i < len(b) {
					return b[loc[1]+i]
				}
				return 0
			}
			if next(0) == ':' || next(0) == '*' || (next(0) == '.' && next(1) >= 'a' && next(1) <= 'z') || strings.HasSuffix(name, "_") {
				continue
			}
			if foreign(name) {
				continue
			}
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_count"), "_sum")
			if !strings.HasPrefix(base, "commercial_billing_") {
				t.Errorf("%s reads %s: commercial_* domain metrics are not ratified (Tier 2); use commercial_billing_*", f, name)
				continue
			}
			if !strings.Contains(exposed, "# TYPE "+base+" ") {
				t.Errorf("%s reads %s, which Billing does not expose", f, name)
			}
		}
	}
}

func foreign(name string) bool {
	for _, p := range otherServices {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// platform_* references must be registry names (metricslint refs:
// registry-deprecated names, unregistered platform_*).
func TestReferencesPassMetricsLintRefs(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run")
	}
	args := append([]string{"run", "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/cmd/metricslint", "refs", "-deny-deprecated"}, referenceFiles(t)...)
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), " 0 errors") {
		t.Fatalf("metricslint refs:\n%s", out)
	}
}

var platformName = regexp.MustCompile(`\bplatform_[a-z0-9_]+\b`)

// Every platform_* name a rule, dashboard, HPA or runbook reads is in the
// Platform Observability Registry and neither deprecated nor removed
// (standard rules 10–11). metricslint refs checks only names with a series
// suffix (_total, _seconds, …), on the assumption that a scrape catches
// unregistered gauges; Billing's scrape holds only its own collectors, so
// a gauge emitted by a library (platform-events, pgcommon) is checked here.
func TestReferencedPlatformMetricsAreRegistered(t *testing.T) {
	reg := obsregistry.Default()
	for _, f := range referenceFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range platformName.FindAllStringIndex(string(b), -1) {
			name := string(b[loc[0]:loc[1]])
			if loc[1] < len(b) && (b[loc[1]] == ':' || b[loc[1]] == '*') || strings.HasSuffix(name, "_") {
				continue // recording-rule level or wildcard in prose
			}
			m, ok := reg.Lookup(name)
			if !ok {
				for _, s := range []string{"_bucket", "_count", "_sum"} {
					if base, cut := strings.CutSuffix(name, s); cut {
						if m, ok = reg.Lookup(base); ok {
							break
						}
					}
				}
			}
			switch {
			case !ok:
				t.Errorf("%s reads %s, which is not in the Platform Observability Registry (no library emits it)", f, name)
			case m.Status == obsregistry.StatusDeprecated || m.Status == obsregistry.StatusRemoved:
				t.Errorf("%s reads %s, which the registry marks %s", f, name, m.Status)
			}
		}
	}
}
