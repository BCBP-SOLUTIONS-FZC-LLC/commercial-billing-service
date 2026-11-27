{{/* NetworkPolicy egress rules shared by the server, CronJob and migrate policies (§10.2). */}}
{{- define "commercial-billing.np.dns" -}}
# DNS.
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
  ports:
    - { port: 53, protocol: UDP }
    - { port: 53, protocol: TCP }
{{- end }}
{{- define "commercial-billing.np.istiod" -}}
# Istio sidecar: xDS and certificates from istiod.
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.networkPolicy.istioNamespace }}
      podSelector:
        matchLabels:
          app: istiod
  ports:
    - { port: 15012, protocol: TCP }
{{- end }}
{{- define "commercial-billing.np.postgresTo" -}}
{{- with .Values.networkPolicy.postgresCIDRs -}}
to:
{{- range . }}
  - ipBlock: { cidr: {{ . | quote }} }
{{- end }}
{{- end }}
{{- end }}
{{- define "commercial-billing.np.observability" -}}
# OTel collector and Pushgateway.
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.networkPolicy.observabilityNamespace }}
  ports:
    - { port: 4317, protocol: TCP }
    - { port: 9091, protocol: TCP }
{{- end }}
{{- define "commercial-billing.np.https" -}}
# HTTPS out to public addresses: Stripe API (and the public AWS SNS, SQS,
# Glue, STS endpoints for the server). Restricted further to the mesh egress
# gateway / ServiceEntry allow-list by the platform.
- to:
    - ipBlock:
        cidr: 0.0.0.0/0
        except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16]
  ports:
    - { port: 443, protocol: TCP }
{{- with .Values.networkPolicy.egressGateway }}
{{- if .namespace }}
# Mesh egress gateway (go-live check: networkPolicy.egressGateway).
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .namespace }}
      {{- with .podLabels }}
      podSelector:
        matchLabels:
          {{- toYaml . | nindent 10 }}
      {{- end }}
  ports:
    - { port: 443, protocol: TCP }
{{- end }}
{{- end }}
{{- end }}
{{- define "commercial-billing.np.awsEndpoints" -}}
{{- with .Values.networkPolicy.awsEndpointCIDRs -}}
# AWS VPC interface endpoints (SNS, SQS, Glue, STS) on private addresses,
# which the public HTTPS rule excludes. Go-live check:
# networkPolicy.awsEndpointCIDRs.
- to:
  {{- range . }}
    - ipBlock: { cidr: {{ . | quote }} }
  {{- end }}
  ports:
    - { port: 443, protocol: TCP }
{{- end }}
{{- end }}
