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
{{- if .Values.auditLog }}
{{- if .Values.auditLog.backend }}
- name: OBSERVE_X_AUDIT_LOG_BACKEND
  value: {{ .Values.auditLog.backend | quote }}
{{- if eq .Values.auditLog.backend "file" }}
- name: OBSERVE_X_AUDIT_LOG_FILE_PATH
  value: {{ .Values.auditLog.file.path | quote }}
{{- end }}
{{- if eq .Values.auditLog.backend "s3" }}
- name: OBSERVE_X_AUDIT_LOG_S3_BUCKET
  value: {{ .Values.auditLog.s3.bucket | quote }}
- name: OBSERVE_X_AUDIT_LOG_S3_PREFIX
  value: {{ .Values.auditLog.s3.prefix | quote }}
{{- if .Values.auditLog.s3.region }}
- name: OBSERVE_X_AUDIT_LOG_S3_REGION
  value: {{ .Values.auditLog.s3.region | quote }}
{{- end }}
{{- if .Values.auditLog.s3.endpoint }}
- name: OBSERVE_X_AUDIT_LOG_S3_ENDPOINT
  value: {{ .Values.auditLog.s3.endpoint | quote }}
{{- end }}
{{- if .Values.auditLog.s3.insecure }}
- name: OBSERVE_X_AUDIT_LOG_S3_INSECURE
  value: {{ .Values.auditLog.s3.insecure | quote }}
{{- end }}
{{- if .Values.auditLog.s3.lock }}
- name: OBSERVE_X_AUDIT_LOG_S3_LOCK
  value: {{ .Values.auditLog.s3.lock | quote }}
- name: OBSERVE_X_AUDIT_LOG_S3_RETAIN
  value: {{ .Values.auditLog.s3.retain | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}
