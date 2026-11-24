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

{{/* Environment shared by the server, the CronJobs and the migrate Job:
     plain values from .Values.env, secrets from the one Secret (§10.5). */}}
{{- define "commercial-billing.env" -}}
{{- range $k, $v := .Values.env }}
- name: {{ $k }}
  value: {{ $v | toString | quote }}
{{- end }}
{{- range .Values.secretKeys }}
- name: {{ . }}
  valueFrom:
    secretKeyRef:
      name: {{ include "commercial-billing.secretName" $ }}
      key: {{ . }}
{{- end }}
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
