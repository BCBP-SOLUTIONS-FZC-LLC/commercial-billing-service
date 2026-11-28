//go:build helm

// Package helm_test asserts the Helm chart's contract (LLD §13.1, §13.2,
// §10.2, §11.5, BIL-AUTH-3, BIL-AUTH-6) on `helm template` output. Run by
// .github/scripts/helm-check.sh (make helm-check).
package helm_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const chart = "../../deploy/helm"

var envFiles = []string{"", "values-dev.yaml", "values-staging.yaml", "values-prod.yaml"}

type obj = map[string]any

// pipelineValues stands in for the account-specific values release.yml
// sets; base (APP_ENV prod), staging and prod refuse to render without them.
const pipelineValues = "testdata/values-pipeline.yaml"

// helmTemplate runs helm template with valuesFile (relative to the chart, ""
// for the base values) and extra arguments; withPipeline adds pipelineValues.
func helmTemplate(valuesFile string, withPipeline bool, extra ...string) ([]byte, error) {
	args := []string{"template", "commercial-billing", chart, "--namespace", "billing"}
	if valuesFile != "" {
		args = append(args, "-f", filepath.Join(chart, valuesFile))
	}
	if withPipeline {
		args = append(args, "-f", pipelineValues)
	}
	args = append(args, extra...)
	return exec.Command("helm", args...).CombinedOutput()
}

// render renders valuesFile as the pipeline would (pipelineValues on top,
// except for dev) plus extra arguments.
func render(t *testing.T, valuesFile string, extra ...string) []obj {
	t.Helper()
	out, err := helmTemplate(valuesFile, valuesFile != "values-dev.yaml", extra...)
	if err != nil {
		t.Fatalf("helm template %s %v: %v\n%s", valuesFile, extra, err, out)
	}
	var docs []obj
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var d obj
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func get(o any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := o.(obj)
			if !ok {
				return nil
			}
			o = m[k]
		case int:
			s, ok := o.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			o = s[k]
		}
	}
	return o
}

func find(docs []obj, kind, name string) obj {
	for _, d := range docs {
		if d["kind"] == kind && get(d, "metadata", "name") == name {
			return d
		}
	}
	return nil
}

func envOf(container any) map[string]string {
	out := map[string]string{}
	for _, e := range get(container, "env").([]any) {
		m := e.(obj)
		if v, ok := m["value"].(string); ok {
			out[m["name"].(string)] = v
		}
	}
	return out
}

// Names, namespace and identity (CD-19, §13.1).
func TestBIL_OPS_4_NamesAndNamespace(t *testing.T) {
	for _, vf := range envFiles {
		docs := render(t, vf)
		for _, d := range docs {
			if ns := get(d, "metadata", "namespace"); ns != "billing" {
				t.Errorf("%s: %s %v namespace %v, want billing", vf, d["kind"], get(d, "metadata", "name"), ns)
			}
		}
		if find(docs, "ServiceAccount", "commercial-billing") == nil {
			t.Errorf("%s: ServiceAccount commercial-billing missing", vf)
		}
		dep := find(docs, "Deployment", "commercial-billing")
		if dep == nil {
			t.Fatalf("%s: Deployment commercial-billing missing", vf)
		}
		if get(dep, "spec", "template", "spec", "serviceAccountName") != "commercial-billing" ||
			get(dep, "spec", "template", "metadata", "labels", "app.kubernetes.io/name") != "commercial-billing" {
			t.Errorf("%s: deployment identity", vf)
		}
	}
}

// BIL-AUTH-3: B-I1 reads the last XFCC element, which the sidecar appends
// from the verified client certificate (APPEND_FORWARD, the sidecar default).
// PeerAuthentication STRICT on every Billing workload keeps a plaintext
// caller from supplying it; the metrics port alone may be PERMISSIVE. The
// former proxy.istio.io/config forwardClientCertDetails annotation is
// gateway-only in Istio and must not come back as if it did something.
func TestPeerAuthenticationStrict(t *testing.T) {
	for _, vf := range envFiles {
		docs := render(t, vf)
		pa := find(docs, "PeerAuthentication", "commercial-billing")
		if pa == nil {
			t.Fatalf("%s: PeerAuthentication missing", vf)
		}
		if get(pa, "spec", "mtls", "mode") != "STRICT" {
			t.Errorf("%s: PeerAuthentication mode %v, want STRICT", vf, get(pa, "spec", "mtls", "mode"))
		}
		sel, _ := get(pa, "spec", "selector", "matchLabels").(obj)
		if sel["app.kubernetes.io/name"] != "commercial-billing" || sel["app.kubernetes.io/component"] != nil {
			t.Errorf("%s: PeerAuthentication must select every Billing workload: %v", vf, sel)
		}
		levels, _ := get(pa, "spec", "portLevelMtls").(obj)
		for port, m := range levels {
			if port != "9090" || get(m, "mode") != "PERMISSIVE" {
				t.Errorf("%s: portLevelMtls %s %v: only the metrics port may be relaxed", vf, port, m)
			}
		}
		if len(levels) != 1 {
			t.Errorf("%s: default metrics.scrapeMode plaintext must relax the metrics port: %v", vf, levels)
		}
		dep := find(docs, "Deployment", "commercial-billing")
		if _, ok := get(dep, "spec", "template", "metadata", "annotations", "proxy.istio.io/config").(string); ok {
			t.Errorf("%s: proxy.istio.io/config annotation is back (forwardClientCertDetails is gateway-only)", vf)
		}
	}
}

// metricsRule returns the AuthorizationPolicy rule that admits the metrics
// port, failing if there is not exactly one.
func metricsRule(t *testing.T, docs []obj) obj {
	t.Helper()
	ap := find(docs, "AuthorizationPolicy", "commercial-billing")
	var found []obj
	for _, r := range get(ap, "spec", "rules").([]any) {
		if fmt.Sprint(get(r, "to", 0, "operation", "ports")) == "[9090]" {
			found = append(found, r.(obj))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one metrics-port rule, got %d: %v", len(found), get(ap, "spec", "rules"))
	}
	return found[0]
}

// §13.1: metrics.scrapeMode decides how Prometheus reaches /metrics. A
// plaintext scrape (Prometheus outside the mesh, the default) has no peer
// identity, so the metrics rule must not depend on one (a namespaces match
// would refuse every scrape and silence every alert and the HPA metric): it
// admits by source address, or any source with the NetworkPolicy limiting
// the port to the monitoring namespace. mesh: STRICT everywhere, admitted by
// namespace. Only the metrics port is ever relaxed.
func TestMetricsScrapeModes(t *testing.T) {
	docs := render(t, "values-prod.yaml")
	rule := metricsRule(t, docs)
	if get(rule, "from") != nil {
		t.Errorf("plaintext without monitoringCIDRs: metrics rule must not need a peer identity or address: %v", rule)
	}
	if get(rule, "to", 0, "operation", "paths") != nil || fmt.Sprint(get(rule, "to", 0, "operation", "methods")) != "[GET]" {
		t.Errorf("metrics rule %v", rule)
	}
	np := find(docs, "NetworkPolicy", "commercial-billing")
	var metricsIngress []any
	for _, in := range get(np, "spec", "ingress").([]any) {
		if strings.Contains(fmt.Sprint(get(in, "ports")), "port:9090") {
			metricsIngress = append(metricsIngress, in)
		}
	}
	if len(metricsIngress) != 1 || get(metricsIngress[0], "from", 0, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "monitoring" {
		t.Errorf("NetworkPolicy must admit the metrics port from the monitoring namespace only: %v", metricsIngress)
	}

	docs = render(t, "values-prod.yaml", "--set", "metrics.monitoringCIDRs={10.0.32.0/20}")
	if got := fmt.Sprint(get(metricsRule(t, docs), "from", 0, "source", "ipBlocks")); got != "[10.0.32.0/20]" {
		t.Errorf("plaintext with monitoringCIDRs: ipBlocks %s", got)
	}
	if strings.Contains(fmt.Sprint(metricsRule(t, docs)), "namespaces") {
		t.Error("plaintext: the metrics rule must not match on namespaces")
	}

	docs = render(t, "values-prod.yaml", "--set", "metrics.scrapeMode=mesh")
	pa := find(docs, "PeerAuthentication", "commercial-billing")
	if get(pa, "spec", "mtls", "mode") != "STRICT" || get(pa, "spec", "portLevelMtls") != nil {
		t.Errorf("mesh: STRICT on every port, got %v", get(pa, "spec"))
	}
	if got := fmt.Sprint(get(metricsRule(t, docs), "from", 0, "source", "namespaces")); got != "[monitoring]" {
		t.Errorf("mesh: metrics rule namespaces %s, want [monitoring]", got)
	}

	if out, err := helmTemplate("values-prod.yaml", true, "--set", "metrics.scrapeMode=open"); err == nil || !strings.Contains(string(out), "scrapeMode") {
		t.Errorf("an unknown scrapeMode must fail the render: %v\n%s", err, firstLines(out))
	}
}

// BIL-AUTH-6 and §12: gates stay off in every environment's defaults.
func TestGatesOff(t *testing.T) {
	for _, vf := range envFiles {
		dep := find(render(t, vf), "Deployment", "commercial-billing")
		env := envOf(get(dep, "spec", "template", "spec", "containers", 0))
		for k, want := range map[string]string{"OPERATOR_PATH_LIVE": "false", "FEATURE_TOKEN_ADDONS": "false", "FEATURE_TRIAL_CAP_OVERRIDE": "false",
			"FEATURE_DIRECT_SIGNUP": "false", "RUN_MIGRATIONS": "false", "APP_NAME": "commercial-billing"} {
			if env[k] != want {
				t.Errorf("%s: %s = %q, want %q", vf, k, env[k], want)
			}
		}
		for _, k := range []string{"STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRETS", "DATABASE_URL", "LOG_HASH_SALT"} {
			if _, plain := env[k]; plain {
				t.Errorf("%s: secret %s set as a plain value (§10.5)", vf, k)
			}
		}
	}
}

// §13.2: ten CronJobs whose names and schedules are cmd/jobs'.
func TestCronJobsMatchJobsBinary(t *testing.T) {
	src, err := os.ReadFile("../../cmd/jobs/main.go")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z-]+)":\s+"([0-9*/, ]+)",`).FindAllStringSubmatch(string(src), -1) {
		want[m[1]] = m[2]
	}
	if len(want) != 10 {
		t.Fatalf("parsed %d jobs from cmd/jobs", len(want))
	}
	docs := render(t, "values-prod.yaml")
	got := 0
	for _, d := range docs {
		if d["kind"] != "CronJob" {
			continue
		}
		got++
		name := strings.TrimPrefix(get(d, "metadata", "name").(string), "commercial-billing-")
		if want[name] != get(d, "spec", "schedule") {
			t.Errorf("CronJob %s schedule %v, want %q", name, get(d, "spec", "schedule"), want[name])
		}
		if get(d, "spec", "concurrencyPolicy") != "Forbid" || get(d, "spec", "startingDeadlineSeconds") != 120 {
			t.Errorf("CronJob %s policy", name)
		}
		cmd := get(d, "spec", "jobTemplate", "spec", "template", "spec", "containers", 0, "command").([]any)
		if fmt.Sprint(cmd) != "[/jobs --job="+name+"]" {
			t.Errorf("CronJob %s command %v", name, cmd)
		}
	}
	if got != len(want) {
		t.Errorf("%d CronJobs, want %d", got, len(want))
	}
}

// §13.1, BL-OQ-9: the operator HTTPRoute lists every operator route; the
// tenant and webhook routes exist.
func TestRoutes(t *testing.T) {
	docs := render(t, "values-prod.yaml")
	op := find(docs, "HTTPRoute", "commercial-billing-operator")
	if op == nil {
		t.Fatal("operator HTTPRoute missing")
	}
	matches := get(op, "spec", "rules", 0, "matches").([]any)
	if len(matches) != 18 {
		t.Fatalf("operator matches %d, want 18 (B-O1 to B-O18)", len(matches))
	}
	wh := find(docs, "HTTPRoute", "commercial-billing-webhook")
	if get(wh, "spec", "rules", 0, "matches", 0, "path", "value") != "/api/v1/webhooks/stripe" {
		t.Error("webhook route path")
	}
	sp := find(docs, "SecurityPolicy", "commercial-billing-webhook")
	if _, merged := get(sp, "spec").(obj)["mergeType"]; merged || get(sp, "spec", "jwt") != nil {
		t.Error("webhook SecurityPolicy must replace the gateway policy (no mergeType, no JWT)")
	}
	if get(find(docs, "SecurityPolicy", "commercial-billing-tenant"), "spec", "mergeType") != "StrategicMerge" {
		t.Error("tenant SecurityPolicy must merge with the gateway policy")
	}
	if find(docs, "HTTPRoute", "commercial-billing-tenant") == nil {
		t.Error("tenant HTTPRoute missing")
	}
}

// §10.2: ingress from gateway, bff-signup and monitoring; egress includes
// Entitlements (the one GET), IAM and the observability namespace.
func TestNetworkPolicy(t *testing.T) {
	np := find(render(t, "values-prod.yaml"), "NetworkPolicy", "commercial-billing")
	if np == nil {
		t.Fatal("NetworkPolicy missing")
	}
	text := fmt.Sprint(np)
	for _, s := range []string{"commercial-entitlements", "iam-org-membership", "iam-realm-provisioner", "iam-audit-log", "bff-signup", "monitoring", "observability"} {
		if !strings.Contains(text, s) {
			t.Errorf("NetworkPolicy lacks %s", s)
		}
	}
}

// §13.1: HPA 2–4 with the webhook backlog; PDB minAvailable 1; migrate hook.
func TestScalingAndMigrate(t *testing.T) {
	docs := render(t, "values-prod.yaml")
	hpa := find(docs, "HorizontalPodAutoscaler", "commercial-billing")
	if get(hpa, "spec", "minReplicas") != 2 || get(hpa, "spec", "maxReplicas") != 4 ||
		get(hpa, "spec", "metrics", 1, "external", "metric", "name") != "commercial_billing_webhook_backlog" {
		t.Errorf("HPA %v", get(hpa, "spec"))
	}
	if get(find(docs, "PodDisruptionBudget", "commercial-billing"), "spec", "minAvailable") != 1 {
		t.Error("PDB")
	}
	job := find(docs, "Job", "commercial-billing-migrate")
	if get(job, "metadata", "annotations", "helm.sh/hook") != "pre-install,pre-upgrade" {
		t.Error("migrate hook")
	}
	// The Job ends (MIGRATE_TIMEOUT, then activeDeadlineSeconds for both
	// attempts) before release.yml's helm --timeout=10m gives up on it.
	const helmTimeout = 10 * time.Minute
	deadline, _ := get(job, "spec", "activeDeadlineSeconds").(int)
	mt, err := time.ParseDuration(envOf(get(job, "spec", "template", "spec", "containers", 0))["MIGRATE_TIMEOUT"])
	if err != nil || mt < time.Minute || mt >= time.Duration(deadline)*time.Second || time.Duration(deadline)*time.Second >= helmTimeout {
		t.Errorf("migrate MIGRATE_TIMEOUT %v (%v) < activeDeadlineSeconds %d < helm timeout %v", mt, err, deadline, helmTimeout)
	}
}

// §11.5: the chart's rules are deploy/monitoring/app-alerts.yml, and every
// alert has a runbook that exists.
func TestAlertsAndRunbooks(t *testing.T) {
	a, _ := os.ReadFile("../../deploy/monitoring/app-alerts.yml")
	b, _ := os.ReadFile(chart + "/files/app-alerts.yml")
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Fatal("deploy/helm/files/app-alerts.yml differs from deploy/monitoring/app-alerts.yml")
	}
	slo, _ := os.ReadFile("../../deploy/monitoring/slo-rules.yml")
	var rules struct {
		Groups []struct {
			Rules []struct {
				Alert       string
				Annotations map[string]string
			}
		}
	}
	for _, doc := range [][]byte{a, slo} {
		var part struct {
			Groups []struct {
				Rules []struct {
					Alert       string
					Annotations map[string]string
				}
			}
		}
		if err := yaml.Unmarshal(doc, &part); err != nil {
			t.Fatal(err)
		}
		rules.Groups = append(rules.Groups, part.Groups...)
	}
	var names []string
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" { // recording rule
				continue
			}
			names = append(names, r.Alert)
			u := r.Annotations["runbook_url"]
			i := strings.Index(u, "/blob/main/")
			if i < 0 {
				t.Errorf("%s: runbook_url %q", r.Alert, u)
				continue
			}
			if _, err := os.Stat("../../" + u[i+len("/blob/main/"):]); err != nil {
				t.Errorf("%s: runbook %s missing", r.Alert, u)
			}
		}
	}
	for _, want := range []string{"BillingConversionSagaOverdue", "BillingSagaFailed", "CommercialPurchaseNotActive", "BillingWebhookSignatureFailures",
		"BillingWebhookBacklog", "BillingOutboxDeadLetter", "BillingDLQNotEmpty", "BillingI18Unavailable", "BillingSuspensionDraftsAwaitingDecision",
		"BillingReconciliationDrift", "BillingSeatProjectionDrift", "BillingProviderUnavailable", "BillingProviderNotCertified"} {
		if !slices.Contains(names, want) {
			t.Errorf("alert %s missing (§11.5)", want)
		}
	}
}
