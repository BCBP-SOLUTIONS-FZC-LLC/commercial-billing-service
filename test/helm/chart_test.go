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

	"gopkg.in/yaml.v3"
)

const chart = "../../deploy/helm"

var envFiles = []string{"", "values-dev.yaml", "values-staging.yaml", "values-prod.yaml"}

type obj = map[string]any

func render(t *testing.T, valuesFile string) []obj {
	t.Helper()
	args := []string{"template", "commercial-billing", chart, "--namespace", "billing"}
	if valuesFile != "" {
		args = append(args, "-f", filepath.Join(chart, valuesFile))
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", valuesFile, err, out)
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
func TestNamesAndNamespace(t *testing.T) {
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
		if get(dep, "spec", "template", "spec", "terminationGracePeriodSeconds") != 45 {
			t.Errorf("%s: terminationGracePeriodSeconds", vf)
		}
	}
}

// BIL-AUTH-3: the sidecar forwards the client certificate as XFCC.
func TestXFCCAnnotation(t *testing.T) {
	for _, vf := range envFiles {
		dep := find(render(t, vf), "Deployment", "commercial-billing")
		a, _ := get(dep, "spec", "template", "metadata", "annotations", "proxy.istio.io/config").(string)
		if !strings.Contains(a, "forwardClientCertDetails: SANITIZE_SET") {
			t.Errorf("%s: proxy.istio.io/config = %q", vf, a)
		}
	}
}

// BIL-AUTH-6 and §12: gates stay off in every environment's defaults.
func TestGatesOff(t *testing.T) {
	for _, vf := range envFiles {
		dep := find(render(t, vf), "Deployment", "commercial-billing")
		env := envOf(get(dep, "spec", "template", "spec", "containers", 0))
		for k, want := range map[string]string{"OPERATOR_PATH_LIVE": "false", "FEATURE_TOKEN_ADDONS": "false",
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
	if len(matches) != 17 {
		t.Fatalf("operator matches %d, want 17 (B-O1 to B-O17)", len(matches))
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
	if get(job, "metadata", "annotations", "helm.sh/hook") != "pre-install,pre-upgrade" || get(job, "spec", "activeDeadlineSeconds") != 900 {
		t.Error("migrate hook")
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
