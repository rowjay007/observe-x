{{/* vim: set filetype=mustache: */}}

{{/*
Expand the chart name.
*/}}
{{- define "observex.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. Truncated at 63 characters per DNS-1035.
*/}}
{{- define "observex.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Common labels applied to every resource.
*/}}
{{- define "observex.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "observex.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: observex
{{- end -}}

{{/*
Service-specific selector labels. Pass `service` and `dict` in.
*/}}
{{- define "observex.selectorLabels" -}}
app.kubernetes.io/name: {{ include "observex.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
Image reference: {registry}/{service}:{tag}
*/}}
{{- define "observex.image" -}}
{{- printf "%s/%s:%s" .Values.global.imageRegistry .component .Values.global.imageTag -}}
{{- end -}}

{{/*
Common env block: self-observability + Postgres URL from secret.
*/}}
{{- define "observex.commonEnv" -}}
- name: OBSERVE_X_POSTGRES_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.existingSecret }}
      key: postgres-url
{{- if .Values.selfObservability.enabled }}
- name: OBSERVE_X_OTLP_ENDPOINT
  value: {{ .Values.selfObservability.endpoint | quote }}
- name: OBSERVE_X_OTLP_SAMPLING
  value: {{ .Values.selfObservability.samplingFraction | quote }}
- name: OBSERVE_X_OTLP_INSECURE
  value: {{ .Values.selfObservability.insecure | quote }}
{{- end }}
{{- end -}}
