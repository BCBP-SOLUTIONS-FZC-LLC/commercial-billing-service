//go:build helm

package helm_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// podTemplates returns each workload's pod template by component: the
// Deployment, every CronJob and the migrate Job.
func podTemplates(t *testing.T, docs []obj) map[string][]obj {
	t.Helper()
	out := map[string][]obj{}
	for _, d := range docs {
		var tpl any
		switch d["kind"] {
		case "Deployment":
			tpl = get(d, "spec", "template")
		case "CronJob":
			tpl = get(d, "spec", "jobTemplate", "spec", "template")
		case "Job":
			tpl = get(d, "spec", "template")
		default:
			continue
		}
		m, ok := tpl.(obj)
		if !ok {
			t.Fatalf("%s %v: no pod template", d["kind"], get(d, "metadata", "name"))
		}
		c, _ := get(m, "metadata", "labels", "app.kubernetes.io/component").(string)
		out[c] = append(out[c], m)
	}
	return out
}

// matches reports whether every selector label is on the pod.
func matches(selector any, labels any) bool {
	sel, _ := selector.(obj)
	lab, _ := labels.(obj)
	if len(sel) == 0 {
		return false
	}
	for k, v := range sel {
		if lab[k] != v {
			return false
		}
	}
	return true
}

// secretRefs returns the container's secret-backed env vars: name → Secret.
func secretRefs(container any) map[string]string {
	out := map[string]string{}
	for _, e := range get(container, "env").([]any) {
		if s, ok := get(e, "valueFrom", "secretKeyRef", "name").(string); ok {
			out[get(e, "name").(string)] = s
		}
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// §13.1: workloads carry distinct component labels, all keep
// app.kubernetes.io/name commercial-billing (BIL-OPS-4), and every server
// selector (Service, Deployment, PDB, ServiceMonitor, AuthorizationPolicy,
// server NetworkPolicy, topology spread) selects the server pods only, so a
// CronJob or migrate pod never becomes a Service endpoint.
func TestWorkloadLabelsAndSelectors(t *testing.T) {
	for _, vf := range envFiles {
		docs := render(t, vf)
		pods := podTemplates(t, docs)
		if len(pods["server"]) != 1 || len(pods["cronjob"]) != 10 || len(pods["migrate"]) != 1 {
			t.Fatalf("%s: pod templates by component: server %d, cronjob %d, migrate %d", vf, len(pods["server"]), len(pods["cronjob"]), len(pods["migrate"]))
		}
		for c, list := range pods {
			for _, p := range list {
				if get(p, "metadata", "labels", "app.kubernetes.io/name") != "commercial-billing" {
					t.Errorf("%s: %s pod lacks app.kubernetes.io/name=commercial-billing", vf, c)
				}
			}
		}
		selectors := map[string]any{
			"Service":             get(find(docs, "Service", "commercial-billing"), "spec", "selector"),
			"Deployment":          get(find(docs, "Deployment", "commercial-billing"), "spec", "selector", "matchLabels"),
			"PodDisruptionBudget": get(find(docs, "PodDisruptionBudget", "commercial-billing"), "spec", "selector", "matchLabels"),
			"ServiceMonitor":      get(find(docs, "ServiceMonitor", "commercial-billing"), "spec", "selector", "matchLabels"),
			"AuthorizationPolicy": get(find(docs, "AuthorizationPolicy", "commercial-billing"), "spec", "selector", "matchLabels"),
			"NetworkPolicy":       get(find(docs, "NetworkPolicy", "commercial-billing"), "spec", "podSelector", "matchLabels"),
		}
		if vf != "values-dev.yaml" {
			selectors["topologySpread"] = get(find(docs, "Deployment", "commercial-billing"), "spec", "template", "spec",
				"topologySpreadConstraints", 0, "labelSelector", "matchLabels")
		}
		for what, sel := range selectors {
			if !matches(sel, get(pods["server"][0], "metadata", "labels")) {
				t.Errorf("%s: %s selector %v does not select the server pods", vf, what, sel)
			}
			for _, c := range []string{"cronjob", "migrate"} {
				for _, p := range pods[c] {
					if matches(sel, get(p, "metadata", "labels")) {
						t.Errorf("%s: %s selector %v selects %s pods", vf, what, sel, c)
					}
				}
			}
		}
		// The ServiceMonitor selects Services by label.
		if !matches(selectors["ServiceMonitor"], get(find(docs, "Service", "commercial-billing"), "metadata", "labels")) {
			t.Errorf("%s: ServiceMonitor does not select the Service", vf)
		}
	}
}

// §10.2: CronJob and migrate pods have their own NetworkPolicies with the
// egress each needs; neither admits ingress.
func TestWorkloadNetworkPolicies(t *testing.T) {
	docs := render(t, "values-prod.yaml")
	pods := podTemplates(t, docs)
	cases := map[string]struct {
		component string
		want      []string
		forbid    []string
	}{
		"commercial-billing-cronjob": {"cronjob",
			[]string{"kube-system", "istiod", "port:5432", "port:6432", "iam-org-membership", "iam-realm-provisioner", "port:9091", "port:4317", "port:443", "cidr:10.20.0.0/24"},
			[]string{"iam-audit-log", "commercial-entitlements"}},
		"commercial-billing-migrate": {"migrate",
			[]string{"kube-system", "port:5432", "cidr:10.20.0.0/24"},
			[]string{"port:6432", "port:443", "iam-", "port:9091"}},
	}
	for name, c := range cases {
		np := find(docs, "NetworkPolicy", name)
		if np == nil {
			t.Fatalf("NetworkPolicy %s missing", name)
		}
		for _, p := range pods[c.component] {
			if !matches(get(np, "spec", "podSelector", "matchLabels"), get(p, "metadata", "labels")) {
				t.Errorf("%s does not select the %s pods", name, c.component)
			}
		}
		if get(np, "spec", "ingress") != nil || !slices.Contains(get(np, "spec", "policyTypes").([]any), any("Ingress")) {
			t.Errorf("%s must deny all ingress", name)
		}
		text := fmt.Sprint(get(np, "spec", "egress"))
		for _, s := range c.want {
			if !strings.Contains(text, s) {
				t.Errorf("%s egress lacks %s", name, s)
			}
		}
		for _, s := range c.forbid {
			if strings.Contains(text, s) {
				t.Errorf("%s egress must not include %s", name, s)
			}
		}
	}
	// The server's sidecar needs istiod too.
	if !strings.Contains(fmt.Sprint(find(docs, "NetworkPolicy", "commercial-billing")), "istiod") {
		t.Error("server NetworkPolicy lacks istiod egress")
	}

	// HTTPS: public addresses by default; private AWS interface endpoints
	// (server only: the jobs never call AWS) and an egress gateway (server
	// and jobs) only when configured.
	for _, name := range []string{"commercial-billing", "commercial-billing-cronjob"} {
		text := fmt.Sprint(get(find(docs, "NetworkPolicy", name), "spec", "egress"))
		if strings.Contains(text, "10.0.8.0/24") || strings.Contains(text, "istio-egress") {
			t.Errorf("%s: no AWS endpoint or egress gateway rule by default", name)
		}
	}
	docs = render(t, "values-prod.yaml",
		"--set", "networkPolicy.awsEndpointCIDRs={10.0.8.0/24,10.0.9.0/24}",
		"--set", "networkPolicy.egressGateway.namespace=istio-egress",
		"--set", "networkPolicy.egressGateway.podLabels.istio=egressgateway")
	awsRule := func(name string) any {
		for _, r := range get(find(docs, "NetworkPolicy", name), "spec", "egress").([]any) {
			if strings.Contains(fmt.Sprint(get(r, "to")), "10.0.8.0/24") {
				return r
			}
		}
		return nil
	}
	if r := awsRule("commercial-billing"); fmt.Sprint(get(r, "ports")) != "[map[port:443 protocol:TCP]]" ||
		fmt.Sprint(get(r, "to")) != "[map[ipBlock:map[cidr:10.0.8.0/24]] map[ipBlock:map[cidr:10.0.9.0/24]]]" {
		t.Errorf("server AWS endpoint egress %v", r)
	}
	for _, name := range []string{"commercial-billing-cronjob", "commercial-billing-migrate"} {
		if awsRule(name) != nil {
			t.Errorf("%s must not reach the AWS endpoints", name)
		}
	}
	for _, name := range []string{"commercial-billing", "commercial-billing-cronjob"} {
		text := fmt.Sprint(get(find(docs, "NetworkPolicy", name), "spec", "egress"))
		if !strings.Contains(text, "kubernetes.io/metadata.name:istio-egress") || !strings.Contains(text, "istio:egressgateway") {
			t.Errorf("%s lacks the egress gateway rule: %s", name, text)
		}
	}
	if strings.Contains(fmt.Sprint(find(docs, "NetworkPolicy", "commercial-billing-migrate")), "istio-egress") {
		t.Error("the migrate Job must not reach the egress gateway")
	}
}

// CD-19: CronJobs call Core and RP over mTLS, so they run the sidecar, as a
// native sidecar that exits with the job container; the migrate Job (Postgres
// only, runs before the mesh policies of a first install) does not.
func TestIstioSidecars(t *testing.T) {
	pods := podTemplates(t, render(t, "values-prod.yaml"))
	for _, c := range []string{"server", "cronjob"} {
		for _, p := range pods[c] {
			a := get(p, "metadata", "annotations")
			if get(a, "sidecar.istio.io/inject") != "true" || get(a, "sidecar.istio.io/nativeSidecar") != "true" {
				t.Errorf("%s pod: sidecar annotations %v, want inject and nativeSidecar true", c, a)
			}
			if get(p, "spec", "serviceAccountName") != "commercial-billing" {
				t.Errorf("%s pod: serviceAccountName %v, want commercial-billing (CD-19 SPIFFE id)", c, get(p, "spec", "serviceAccountName"))
			}
		}
	}
	if get(pods["migrate"][0], "metadata", "annotations", "sidecar.istio.io/inject") != "false" {
		t.Error("migrate pod must not get a sidecar")
	}
}

// §10.5: each workload gets only the secrets its binary reads; the
// schema-owner MIGRATION_DATABASE_URL only in the migrate Job (from its own
// Secret), or in the server when RUN_MIGRATIONS=true.
func TestSecretsPerWorkload(t *testing.T) {
	want := map[string][]string{
		"server":  {"DATABASE_URL", "LOG_HASH_SALT", "PG_AUDIT_DRAINER_PASSWORD", "PG_SYSTEM_PASSWORD", "STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRETS"},
		"cronjob": {"DATABASE_URL", "LOG_HASH_SALT", "PG_SYSTEM_PASSWORD", "STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRETS"},
		"migrate": {"MIGRATION_DATABASE_URL"},
	}
	wantSecret := map[string]string{"server": "commercial-billing-secrets", "cronjob": "commercial-billing-secrets", "migrate": "commercial-billing-migrate-secrets"}
	for _, vf := range envFiles {
		docs := render(t, vf)
		for c, list := range podTemplates(t, docs) {
			for _, p := range list {
				refs := secretRefs(get(p, "spec", "containers", 0))
				if got := keys(refs); !slices.Equal(got, want[c]) {
					t.Errorf("%s: %s secrets %v, want %v", vf, c, got, want[c])
				}
				for k, s := range refs {
					if s != wantSecret[c] {
						t.Errorf("%s: %s reads %s from %s, want %s", vf, c, k, s, wantSecret[c])
					}
				}
			}
		}
		es := find(docs, "ExternalSecret", "commercial-billing")
		var got []string
		for _, d := range get(es, "spec", "data").([]any) {
			got = append(got, get(d, "secretKey").(string))
		}
		if slices.Contains(got, "MIGRATION_DATABASE_URL") {
			t.Errorf("%s: service ExternalSecret holds MIGRATION_DATABASE_URL", vf)
		}
		mes := find(docs, "ExternalSecret", "commercial-billing-migrate")
		if get(mes, "spec", "target", "name") != "commercial-billing-migrate-secrets" ||
			fmt.Sprint(get(mes, "spec", "data", 0, "secretKey")) != "MIGRATION_DATABASE_URL" || len(get(mes, "spec", "data").([]any)) != 1 {
			t.Errorf("%s: migrate ExternalSecret %v", vf, get(mes, "spec"))
		}
	}
	docs := render(t, "values-prod.yaml", "--set-string", "env.RUN_MIGRATIONS=true")
	refs := secretRefs(get(find(docs, "Deployment", "commercial-billing"), "spec", "template", "spec", "containers", 0))
	if _, ok := refs["MIGRATION_DATABASE_URL"]; !ok {
		t.Error("RUN_MIGRATIONS=true: server lacks MIGRATION_DATABASE_URL")
	}
}

// §19.1: the migrate hook brings its own ServiceAccount and Secret, created
// before it (the release's do not exist on a first install) and removed
// after it succeeds.
func TestMigrateHookPrerequisites(t *testing.T) {
	docs := render(t, "values-prod.yaml")
	job := find(docs, "Job", "commercial-billing-migrate")
	if get(job, "spec", "template", "spec", "serviceAccountName") != "commercial-billing-migrate" {
		t.Errorf("migrate serviceAccountName %v", get(job, "spec", "template", "spec", "serviceAccountName"))
	}
	for _, kind := range []string{"ServiceAccount", "ExternalSecret"} {
		d := find(docs, kind, "commercial-billing-migrate")
		a := get(d, "metadata", "annotations")
		if get(a, "helm.sh/hook") != "pre-install,pre-upgrade" || get(a, "helm.sh/hook-weight") != "-10" ||
			get(a, "helm.sh/hook-delete-policy") != "before-hook-creation,hook-succeeded,hook-failed" {
			t.Errorf("%s commercial-billing-migrate hook annotations %v (the migration credential must not outlive a failed hook)", kind, a)
		}
	}
	if get(find(docs, "ServiceAccount", "commercial-billing-migrate"), "metadata", "annotations", "eks.amazonaws.com/role-arn") != nil {
		t.Error("the migrate ServiceAccount must not carry the IRSA role")
	}
}

// Staging and prod refuse to render with the placeholder account, without
// the IRSA role, or without Postgres CIDRs (§10.2); dev still renders.
func TestPlaceholderGuards(t *testing.T) {
	cases := []struct {
		name  string
		vf    string
		pipe  bool
		extra []string
		want  string
	}{
		{"prod without pipeline values", "values-prod.yaml", false, nil, "is required"},
		{"staging without pipeline values", "values-staging.yaml", false, nil, "is required"},
		{"base (APP_ENV prod) without pipeline values", "", false, nil, "is required"},
		{"placeholder account in env", "values-prod.yaml", true,
			[]string{"--set-string", "env.SNS_BILLING_EVENTS_TOPIC_ARN=arn:aws:sns:ap-south-1:000000000000:billing-events"}, "000000000000"},
		{"placeholder account in role", "values-staging.yaml", true,
			[]string{"--set-string", `serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::000000000000:role/commercial-billing`}, "000000000000"},
		{"no IRSA role", "values-prod.yaml", true,
			[]string{"--set-string", `serviceAccount.annotations.eks\.amazonaws\.com/role-arn=`}, "role-arn"},
		{"no Postgres CIDRs", "values-prod.yaml", true,
			[]string{"--set", "networkPolicy.postgresCIDRs=null"}, "postgresCIDRs"},
	}
	for _, vf := range []string{"values-staging.yaml", "values-prod.yaml"} {
		for _, key := range []string{"istio.peerAuthentication.enabled", "istio.authorizationPolicy.enabled", "networkPolicy.enabled"} {
			cases = append(cases, struct {
				name  string
				vf    string
				pipe  bool
				extra []string
				want  string
			}{vf + " with " + key + "=false", vf, true, []string{"--set", key + "=false"}, key + " must be true"})
		}
	}
	for _, c := range cases {
		out, err := helmTemplate(c.vf, c.pipe, c.extra...)
		if err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: want a render failure mentioning %q, got err=%v\n%s", c.name, c.want, err, firstLines(out))
		}
	}
	if out, err := helmTemplate("values-dev.yaml", false); err != nil {
		t.Errorf("dev must render without pipeline values: %v\n%s", err, out)
	}
	if out, err := helmTemplate("values-dev.yaml", false, "--set", "istio.peerAuthentication.enabled=false",
		"--set", "istio.authorizationPolicy.enabled=false", "--set", "networkPolicy.enabled=false"); err != nil {
		t.Errorf("dev may turn the mesh and network policies off: %v\n%s", err, out)
	}
	for _, vf := range []string{"values-staging.yaml", "values-prod.yaml"} {
		for _, d := range render(t, vf) {
			if strings.Contains(fmt.Sprint(d), "000000000000") {
				t.Errorf("%s: %s %v carries 000000000000", vf, d["kind"], get(d, "metadata", "name"))
			}
		}
	}
}

func firstLines(b []byte) string {
	lines := strings.SplitN(string(b), "\n", 4)
	return strings.Join(lines[:min(3, len(lines))], "\n")
}

// §13.1: preStop sleep (native action, distroless), grace period 60 s,
// probe timeouts above the app's 5 s dependency-check bound.
func TestShutdownAndProbes(t *testing.T) {
	for _, vf := range envFiles {
		spec := get(find(render(t, vf), "Deployment", "commercial-billing"), "spec", "template", "spec")
		if get(spec, "terminationGracePeriodSeconds") != 60 {
			t.Errorf("%s: terminationGracePeriodSeconds %v, want 60", vf, get(spec, "terminationGracePeriodSeconds"))
		}
		ctr := get(spec, "containers", 0)
		sleep, _ := get(ctr, "lifecycle", "preStop", "sleep", "seconds").(int)
		if sleep < 5 || sleep >= 60 {
			t.Errorf("%s: preStop sleep %v", vf, get(ctr, "lifecycle", "preStop"))
		}
		if get(ctr, "lifecycle", "preStop", "exec") != nil {
			t.Errorf("%s: preStop exec needs a shell; the image is distroless", vf)
		}
		for _, p := range []string{"startupProbe", "livenessProbe", "readinessProbe"} {
			if to, _ := get(ctr, p, "timeoutSeconds").(int); to < 6 {
				t.Errorf("%s: %s timeoutSeconds %v, want >= 6", vf, p, get(ctr, p, "timeoutSeconds"))
			}
		}
	}
}

// No Kubernetes API token in any Billing pod: IRSA and Istio mount their
// own projected tokens.
func TestNoServiceAccountToken(t *testing.T) {
	for c, list := range podTemplates(t, render(t, "values-prod.yaml")) {
		for _, p := range list {
			if get(p, "spec", "automountServiceAccountToken") != false {
				t.Errorf("%s pod: automountServiceAccountToken %v, want false", c, get(p, "spec", "automountServiceAccountToken"))
			}
		}
	}
}

// §10.3: the webhook route is limited per client IP, not by one shared
// bucket, keeps its no-merge security policy (no JWT, no ext_authz), and
// admits only the configured source ranges when they are set.
func TestWebhookRateLimitAndSources(t *testing.T) {
	docs := render(t, "values-prod.yaml")
	btp := find(docs, "BackendTrafficPolicy", "commercial-billing-webhook")
	rules, _ := get(btp, "spec", "rateLimit", "global", "rules").([]any)
	if len(rules) != 2 {
		t.Fatalf("webhook rate limit: want one IPv4 and one IPv6 rule, got %v", rules)
	}
	for i, cidr := range []string{"0.0.0.0/0", "::/0"} {
		rule := rules[i]
		if get(rule, "clientSelectors", 0, "sourceCIDR", "type") != "Distinct" || get(rule, "clientSelectors", 0, "sourceCIDR", "value") != cidr {
			t.Errorf("webhook rate limit must be per client IP (%s): %v", cidr, rule)
		}
		if get(rule, "limit", "requests") != 50 {
			t.Errorf("webhook limit %v, want 50", get(rule, "limit"))
		}
	}
	sp := find(docs, "SecurityPolicy", "commercial-billing-webhook")
	if get(sp, "spec", "authorization") != nil {
		t.Error("empty allowedSourceCIDRs must admit any source")
	}
	docs = render(t, "values-prod.yaml", "--set", "gateway.webhook.allowedSourceCIDRs={3.18.12.63/32,3.130.192.231/32}")
	sp = find(docs, "SecurityPolicy", "commercial-billing-webhook")
	spec := get(sp, "spec").(obj)
	if _, merged := spec["mergeType"]; merged || spec["jwt"] != nil || spec["extAuth"] != nil {
		t.Errorf("webhook SecurityPolicy must replace the gateway policy: %v", spec)
	}
	if get(sp, "spec", "authorization", "defaultAction") != "Deny" ||
		fmt.Sprint(get(sp, "spec", "authorization", "rules", 0, "principal", "clientCIDRs")) != "[3.18.12.63/32 3.130.192.231/32]" {
		t.Errorf("webhook source allow-list %v", get(sp, "spec", "authorization"))
	}
}

// CronJobs run one job at a time per pod: small pools.
func TestCronJobPools(t *testing.T) {
	pods := podTemplates(t, render(t, "values-prod.yaml"))
	server := envOf(get(pods["server"][0], "spec", "containers", 0))
	for _, p := range pods["cronjob"] {
		env := envOf(get(p, "spec", "containers", 0))
		if env["PG_MIN_CONNS"] != "0" {
			t.Errorf("CronJob PG_MIN_CONNS=%q, want 0 (pgcommon defaults to 2)", env["PG_MIN_CONNS"])
		}
		for _, k := range []string{"PG_MAX_CONNS", "PG_SYSTEM_MAX_CONNS"} {
			j, errJ := strconv.Atoi(env[k])
			s, errS := strconv.Atoi(server[k])
			if errJ != nil || errS != nil || j < 1 || j >= s {
				t.Errorf("CronJob %s=%q, want 1..%d (below the server's)", k, env[k], s-1)
			}
		}
	}
}
