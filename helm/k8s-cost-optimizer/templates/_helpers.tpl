{{/* Standard name helpers. */}}

{{- define "k8s-cost-optimizer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "k8s-cost-optimizer.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "k8s-cost-optimizer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "k8s-cost-optimizer.labels" -}}
helm.sh/chart: {{ include "k8s-cost-optimizer.chart" . }}
{{ include "k8s-cost-optimizer.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: k8s-cost-optimizer
{{- end }}

{{- define "k8s-cost-optimizer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-cost-optimizer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "k8s-cost-optimizer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "k8s-cost-optimizer.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Fail fast on configurations that would be silently wrong or actively dangerous.

Rendering a chart that cannot work is worse than failing to render: the operator
discovers the problem from a CrashLoopBackOff or, worse, from a cluster mutation
they did not intend.
*/}}
{{- define "k8s-cost-optimizer.validate" -}}
{{- if not .Values.prometheus.address }}
{{- fail "prometheus.address is required: the optimizer cannot produce recommendations without usage history" }}
{{- end }}
{{- if and .Values.analysis.apply (not .Values.rbac.allowApply) }}
{{- fail "analysis.apply=true requires rbac.allowApply=true. Granting write permission is deliberately a separate decision from enabling mutation; set both only after reading docs/security.md." }}
{{- end }}
{{- if lt (float64 .Values.policy.cpuSafetyFactor) 1.0 }}
{{- fail (printf "policy.cpuSafetyFactor=%v is below 1.0, which would recommend less than the statistic identified as the safe envelope" .Values.policy.cpuSafetyFactor) }}
{{- end }}
{{- if lt (float64 .Values.policy.memorySafetyFactor) 1.0 }}
{{- fail (printf "policy.memorySafetyFactor=%v is below 1.0. For memory this is especially dangerous: the failure mode is an OOMKill." .Values.policy.memorySafetyFactor) }}
{{- end }}
{{- end }}
