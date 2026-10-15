{{/* Chart name, truncated to the 63-character DNS label limit. */}}
{{- define "patchbay.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. Component suffixes ("-controller", "-pbx", ...) are
appended to it, so it is truncated to leave room for them.
*/}}
{{- define "patchbay.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "patchbay.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Labels every object carries. */}}
{{- define "patchbay.labels" -}}
helm.sh/chart: {{ include "patchbay.chart" . }}
app.kubernetes.io/name: {{ include "patchbay.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: patchbay
{{- end -}}

{{/* Selector labels for one component: include with (dict "ctx" $ "component" "pbx"). */}}
{{- define "patchbay.selectorLabels" -}}
app.kubernetes.io/name: {{ include "patchbay.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* An image reference: repository[:tag][@digest]. Pass the image values map. */}}
{{- define "patchbay.imageRef" -}}
{{- $ref := .repository -}}
{{- if .tag -}}{{- $ref = printf "%s:%s" $ref .tag -}}{{- end -}}
{{- if .digest -}}{{- $ref = printf "%s@%s" $ref .digest -}}{{- end -}}
{{- $ref -}}
{{- end -}}

{{/* The patchbay image (controller and agent). The tag defaults to appVersion. */}}
{{- define "patchbay.image" -}}
{{- $img := deepCopy .Values.image -}}
{{- if not $img.tag -}}{{- $_ := set $img "tag" .Chart.AppVersion -}}{{- end -}}
{{- include "patchbay.imageRef" $img -}}
{{- end -}}

{{- define "patchbay.controllerServiceAccountName" -}}
{{- printf "%s-controller" (include "patchbay.fullname" .) -}}
{{- end -}}

{{- define "patchbay.agentServiceAccountName" -}}
{{- printf "%s-agent" (include "patchbay.fullname" .) -}}
{{- end -}}

{{/* The Secret holding the agent's control API token (key "token"). */}}
{{- define "patchbay.agentTokenSecretName" -}}
{{- default (printf "%s-agent-token" (include "patchbay.fullname" .)) .Values.pbx.agent.tokenSecretName -}}
{{- end -}}

{{/* The PBX's SIP address, required: Asterisk binds it and advertises it in SDP. */}}
{{- define "patchbay.pbxAddress" -}}
{{- required "pbx.address is required: the IP of pbx.nodeName that phones reach (e.g. 192.168.0.240)" .Values.pbx.address -}}
{{- end -}}
