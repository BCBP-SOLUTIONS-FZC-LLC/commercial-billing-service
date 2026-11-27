{{/* Chart name. */}}
{{- define "commercial-billing.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified name: commercial-billing (CD-19: ServiceAccount, label and
     SPIFFE identity are all commercial-billing). */}}
{{- define "commercial-billing.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "commercial-billing.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "commercial-billing.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: commercial
{{- end }}

{{- define "commercial-billing.selectorLabels" -}}
app.kubernetes.io/name: {{ include "commercial-billing.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "commercial-billing.serviceAccountName" -}}
{{- default (include "commercial-billing.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{- define "commercial-billing.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "commercial-billing.secretName" -}}
{{- if .Values.externalSecret.enabled -}}
{{ .Values.externalSecret.targetSecretName | default (printf "%s-secrets" (include "commercial-billing.fullname" .)) }}
{{- else -}}
{{ .Values.existingSecret | default (printf "%s-secrets" (include "commercial-billing.fullname" .)) }}
{{- end -}}
{{- end }}

{{/* Selector labels of one workload: the shared name/instance plus
     app.kubernetes.io/component (server, cronjob, migrate), so the Service,
     PDB, ServiceMonitor, NetworkPolicy and AuthorizationPolicy of the server
     never match CronJob or migrate pods.
     Call with (dict "ctx" $ "component" "server"). */}}
{{- define "commercial-billing.componentSelectorLabels" -}}
{{ include "commercial-billing.selectorLabels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* Secret of the migrate hook Job: its own Secret holding only the
     schema-owner MIGRATION_DATABASE_URL (§10.5). */}}
{{- define "commercial-billing.migrateSecretName" -}}
{{- if .Values.externalSecret.enabled -}}
{{ printf "%s-migrate-secrets" (include "commercial-billing.fullname" .) }}
{{- else -}}
{{ .Values.migrate.existingSecret | default (include "commercial-billing.secretName" .) }}
{{- end -}}
{{- end }}

{{/* Secret keys the server reads (JSON list). MIGRATION_DATABASE_URL only
     when the server runs migrations itself (RUN_MIGRATIONS=true); in
     Kubernetes the migrate hook Job owns migrations. */}}
{{- define "commercial-billing.serverSecretKeys" -}}
{{- $keys := .Values.secretKeys.server -}}
{{- if eq (toString .Values.env.RUN_MIGRATIONS) "true" -}}
{{- $keys = append $keys "MIGRATION_DATABASE_URL" -}}
{{- end -}}
{{- toJson (uniq $keys) -}}
{{- end }}

{{/* Keys the service Secret (server and CronJobs) holds (JSON list). */}}
{{- define "commercial-billing.serviceSecretKeys" -}}
{{- $server := include "commercial-billing.serverSecretKeys" . | fromJsonArray -}}
{{- toJson (concat $server .Values.secretKeys.jobs | uniq | sortAlpha) -}}
{{- end }}

{{/* Container environment of one workload (§10.5, §12): plain values from
     .Values.env with the workload's overrides on top, and only the secret
     keys that workload reads. Call with
     (dict "ctx" $ "overrides" <map> "secretKeys" <list> "secretName" <name>). */}}
{{- define "commercial-billing.env" -}}
{{- $env := merge (deepCopy (.overrides | default dict)) .ctx.Values.env }}
{{- range $k, $v := $env }}
- name: {{ $k }}
  value: {{ $v | toString | quote }}
{{- end }}
{{- range .secretKeys }}
- name: {{ . }}
  valueFrom:
    secretKeyRef:
      name: {{ $.secretName }}
      key: {{ . }}
{{- end }}
{{- end }}

{{/* Render-time guards. Every environment: a known metrics.scrapeMode.
     Staging and prod (APP_ENV): the mesh and network policies on
     (BIL-AUTH-3, §10.2), no placeholder account id, the IRSA role on the
     ServiceAccount, the bus endpoints and the Postgres CIDRs set. The deploy
     pipeline supplies the account values (release.yml, deploy-gate). */}}
{{- define "commercial-billing.validate" -}}
{{- if not (has (toString .Values.metrics.scrapeMode) (list "plaintext" "mesh")) -}}
{{- fail (printf "metrics.scrapeMode must be plaintext or mesh; got %q" (toString .Values.metrics.scrapeMode)) }}
{{- end -}}
{{- if has (toString .Values.env.APP_ENV) (list "staging" "prod") -}}
{{- $env := .Values.env.APP_ENV -}}
{{- range $name, $on := dict "istio.peerAuthentication.enabled" .Values.istio.peerAuthentication.enabled "istio.authorizationPolicy.enabled" .Values.istio.authorizationPolicy.enabled "networkPolicy.enabled" .Values.networkPolicy.enabled }}
{{- if not $on }}
{{- fail (printf "APP_ENV=%s: %s must be true (BIL-AUTH-3, LLD §10.2); only dev may turn it off" $env $name) }}
{{- end }}
{{- end }}
{{- range $k, $v := .Values.env }}
{{- if contains "000000000000" (toString $v) }}
{{- fail (printf "APP_ENV=%s: env.%s still holds the placeholder account 000000000000; the deploy pipeline must set it" $env $k) }}
{{- end }}
{{- end }}
{{- $required := list "SNS_BILLING_EVENTS_TOPIC_ARN" "SQS_TENANT_BILLING_QUEUE_URL" }}
{{- if eq (toString .Values.env.USAGE_BILLING_CONSUMER_ENABLED) "true" }}{{- $required = append $required "SQS_USAGE_BILLING_QUEUE_URL" }}{{- end }}
{{- range $k := $required }}
{{- if not (get $.Values.env $k) }}
{{- fail (printf "APP_ENV=%s: env.%s is required; the deploy pipeline must set it" $env $k) }}
{{- end }}
{{- end }}
{{- $role := toString (get (.Values.serviceAccount.annotations | default dict) "eks.amazonaws.com/role-arn") }}
{{- if not (regexMatch "^arn:aws:iam::[0-9]{12}:role/.+" $role) }}
{{- fail (printf "APP_ENV=%s: serviceAccount.annotations[eks.amazonaws.com/role-arn] must be the IRSA role ARN (deploy/iam/README.md); got %q" $env $role) }}
{{- end }}
{{- if contains "000000000000" $role }}
{{- fail (printf "APP_ENV=%s: the IRSA role ARN still holds the placeholder account 000000000000" $env) }}
{{- end }}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.postgresCIDRs) }}
{{- fail (printf "APP_ENV=%s: networkPolicy.postgresCIDRs is empty, which would admit any destination on the Postgres ports" $env) }}
{{- end }}
{{- end -}}
{{- end }}

{{/* Istio sidecar of the server and the CronJobs, injected as a Kubernetes
     native sidecar (an init container with restartPolicy Always): it is
     ready before the app starts, stops only after the app has exited (so
     shutdown-time calls to Stripe, Core and the database still work), and
     does not keep a Job pod running once the job container exits. Needs
     Kubernetes >= 1.29 (SidecarContainers) and Istio >= 1.24 (per-pod
     sidecar.istio.io/nativeSidecar). */}}
{{- define "commercial-billing.istioAnnotations" -}}
sidecar.istio.io/inject: "true"
sidecar.istio.io/nativeSidecar: "true"
{{- end }}

{{- define "commercial-billing.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "commercial-billing.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end }}
