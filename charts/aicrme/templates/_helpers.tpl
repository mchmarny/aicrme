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

{{/*
Resolves the auth password once per render and caches it in .Values.global:
explicit .Values.auth.password, else the existing Secret's password (via
lookup, so `helm upgrade` doesn't rotate it), else a fresh random one.
secret.yaml and deployment.yaml's checksum/secret annotation both call this.
Without the cache, a fresh install would call randAlphaNum from each call
site independently, producing two different random strings in the same
render — the real Secret would get one, the checksum the other, so the
very next upgrade (which then deterministically reuses the real Secret's
password from both call sites) would compute a different checksum than
installation did, and needlessly roll the pod despite nothing changing.
.Values.global is a plain Go map shared by reference across every template
in the render, and is always present (Helm guarantees it), so the cache
works regardless of which of the two call sites happens to run first.
*/}}
{{- define "aicrme.authPassword" -}}
{{- if not .Values.global.aicrmeAuthPassword -}}
{{- $name := printf "%s-auth" (include "aicrme.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $name -}}
{{- $password := .Values.auth.password -}}
{{- if not $password -}}
{{- if $existing -}}
{{- $password = index $existing.data "password" | b64dec -}}
{{- else -}}
{{- $password = randAlphaNum 24 -}}
{{- end -}}
{{- end -}}
{{- $_ := set .Values.global "aicrmeAuthPassword" $password -}}
{{- end -}}
{{- .Values.global.aicrmeAuthPassword -}}
{{- end -}}
