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

{{/*
The Secret holding the agent's control API token (key "token"). The
controller mints it (install.Names.AgentTokenSecret); the name is fixed.
*/}}
{{- define "patchbay.agentTokenSecretName" -}}
{{- printf "%s-agent-token" (include "patchbay.fullname" .) -}}
{{- end -}}

{{/*
The install's settings, as the controller reads them: compile.Settings in
JSON (internal/controller/settings parses it strictly; test/chart compares
the default with its fixture). Every number-plan entry is a string, whatever
YAML made of it. streamBridge stays unset until release B. The AMI Secret is
the one the controller mints, <fullname>-ami (install.CheckSettings).
*/}}
{{- define "patchbay.settingsJSON" -}}
{{- $p := .Values.pbx -}}
{{- $plan := .Values.numberPlan -}}
{{- $reserved := list -}}
{{- range $plan.reserved -}}{{- $reserved = append $reserved (toString .) -}}{{- end -}}
{{- $networks := list -}}
{{- range .Values.allowedNetworks -}}{{- $networks = append $networks (toString .) -}}{{- end -}}
{{- $settings := dict
  "pbxAddress" (include "patchbay.pbxAddress" .)
  "sipPort" (int $p.ports.sip)
  "rtpPorts" (dict "first" (int $p.ports.rtpStart) "last" (int $p.ports.rtpEnd))
  "deviceHTTPPort" (int $p.ports.deviceHTTP)
  "ntpServer" (toString .Values.ntpServer)
  "timezone" (toString .Values.timezone)
  "signalling" (toString $p.signalling)
  "numberPlan" (dict
    "devices" (toString $plan.devices)
    "people" (toString $plan.people)
    "streams" (toString $plan.streams)
    "echoTest" (toString $plan.echoTest)
    "emergency" (toString $plan.emergency)
    "reserved" $reserved)
  "allowedNetworks" $networks
  "amiSecret" (dict "name" (printf "%s-ami" (include "patchbay.fullname" .)) "key" "password")
-}}
{{- toPrettyJson $settings -}}
{{- end -}}

{{/* The PBX's SIP address, required: Asterisk binds it and advertises it in SDP. */}}
{{- define "patchbay.pbxAddress" -}}
{{- required "pbx.address is required: the IP of pbx.nodeName that phones reach (e.g. 192.168.0.240)" .Values.pbx.address -}}
{{- end -}}

{{/*
The Asterisk modules the PBX pod holds: exactly compile.Modules(), the list
the compiled modules.conf loads, in its order (test/chart keeps the two
equal). The asterisk-modules init container copies these, and nothing else,
from the image into the volume Asterisk mounts over its module directory,
so no other module (app_system, func_shell, res_agi, res_ari, ...) can be
loaded, not even over AMI or by a relative path.
*/}}
{{- define "patchbay.asteriskModules" -}}
- res_timing_timerfd.so
- res_pjproject.so
- res_sorcery_config.so
- res_sorcery_memory.so
- res_sorcery_astdb.so
- res_rtp_asterisk.so
- res_pjsip.so
- res_pjsip_session.so
- chan_pjsip.so
- res_pjsip_authenticator_digest.so
- res_pjsip_endpoint_identifier_user.so
- res_pjsip_registrar.so
- res_pjsip_sdp_rtp.so
- res_pjsip_nat.so
- res_pjsip_acl.so
- res_pjsip_caller_id.so
- res_pjsip_rfc3326.so
- res_pjsip_pubsub.so
- res_pjsip_notify.so
- res_pjsip_logger.so
- res_pjsip_refer.so
- codec_ulaw.so
- codec_alaw.so
- codec_g722.so
- format_pcm.so
- format_wav.so
- format_gsm.so
- pbx_config.so
- app_echo.so
- app_dial.so
- app_playback.so
- app_playtones.so
- func_callerid.so
- bridge_simple.so
- bridge_native_rtp.so
- res_audiosocket.so
- app_audiosocket.so
{{- end -}}

{{/* Asterisk's config through the live directory's current symlink (compile.AsteriskConfigDir). */}}
{{- define "patchbay.asteriskConf" -}}
/var/lib/patchbay/config/current/asterisk/asterisk.conf
{{- end -}}
