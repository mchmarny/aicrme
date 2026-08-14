{{/*
Chart name. Release.Name is always "aicrme" per the install command in
NOTES.txt and README, but this avoids a hardcoded literal in every template.
*/}}
{{- define "aicrme.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{/*
Fully qualified app name, collapsed to the release name when the release is
already named after the chart (the documented install command does this).
*/}}
{{- define "aicrme.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Chart name and version, for the helm.sh/chart label.
*/}}
{{- define "aicrme.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels applied to every object this chart installs.
*/}}
{{- define "aicrme.labels" -}}
helm.sh/chart: {{ include "aicrme.chart" . }}
{{ include "aicrme.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels: the stable subset used on both the Deployment's pod
template and the Service selector, so they always agree.
*/}}
{{- define "aicrme.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aicrme.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name. One ServiceAccount per release, bound to cluster-admin
by clusterrolebinding.yaml — no override knob, since a second name here
would just be a second identity for the same privilege.
*/}}
{{- define "aicrme.serviceAccountName" -}}
{{- include "aicrme.fullname" . -}}
{{- end -}}
