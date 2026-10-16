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
phoneDefaults is there only when phoneDefaults.display is set
(patchbay.phoneDefaultsDisplay), so the default render is unchanged.
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
{{- $display := include "patchbay.phoneDefaultsDisplay" . -}}
{{- if $display -}}
{{- $_ := set $settings "phoneDefaults" (dict "display" ($display | fromJson)) -}}
{{- end -}}
{{- toPrettyJson $settings -}}
{{- end -}}

{{/*
The house default display (values phoneDefaults.display), as JSON for
settings.json's phoneDefaults.display: compile.Settings.PhoneDefaults.Display,
the shape of Phone.spec.display. Empty when it is unset or null, so the
settings carry no phoneDefaults and no phone gets display settings; an empty
map is a display ({}: no day has an on-window), and is kept. A field that is
null is unset. The chart checks the shape (the field names, a map, a
boolean, a list, start and duration present) and fails `helm template`
otherwise; the controller checks the values at start (whole minutes within
bounds, HH:MM, the day names: v1alpha1.PhoneDisplay.Validate) and refuses to
start, naming the field. Durations and the start are written as strings.
*/}}
{{- define "patchbay.phoneDefaultsDisplay" -}}
{{- $pd := .Values.phoneDefaults | default dict -}}
{{- if not (kindIs "map" $pd) -}}
{{- fail "phoneDefaults must be a map; its only key is display" -}}
{{- end -}}
{{- range $k, $_ := $pd -}}
{{- if ne $k "display" -}}
{{- fail (printf "phoneDefaults.%s is not a house default; the only one is phoneDefaults.display" $k) -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $pd "display") (not (kindIs "invalid" (index $pd "display"))) -}}
{{- $d := index $pd "display" -}}
{{- if not (kindIs "map" $d) -}}
{{- fail "phoneDefaults.display must be a map of idleTimeout, wakeOnCall and onWindow" -}}
{{- end -}}
{{- $out := dict -}}
{{- range $k, $v := $d -}}
{{- if kindIs "invalid" $v -}}
{{- else if eq $k "idleTimeout" -}}
{{- $_ := set $out "idleTimeout" (toString $v) -}}
{{- else if eq $k "wakeOnCall" -}}
{{- if not (kindIs "bool" $v) -}}
{{- fail (printf "phoneDefaults.display.wakeOnCall must be true or false, not %q" (toString $v)) -}}
{{- end -}}
{{- $_ := set $out "wakeOnCall" $v -}}
{{- else if eq $k "onWindow" -}}
{{- if not (kindIs "map" $v) -}}
{{- fail "phoneDefaults.display.onWindow must be a map of start, duration and days" -}}
{{- end -}}
{{- range $wk, $_ := $v -}}
{{- if not (has $wk (list "start" "duration" "days")) -}}
{{- fail (printf "phoneDefaults.display.onWindow.%s is not a field; the fields are start, duration and days" $wk) -}}
{{- end -}}
{{- end -}}
{{- $start := required "phoneDefaults.display.onWindow.start is required: HH:MM on a 24-hour clock, quoted (\"07:30\")" $v.start -}}
{{- $duration := required "phoneDefaults.display.onWindow.duration is required: whole minutes from 1m to 24h (\"10h30m\")" $v.duration -}}
{{- $raw := required "phoneDefaults.display.onWindow.days is required: Monday to Sunday, at least one" $v.days -}}
{{- if not (kindIs "slice" $raw) -}}
{{- fail "phoneDefaults.display.onWindow.days must be a list of days (Monday to Sunday)" -}}
{{- end -}}
{{- $days := list -}}
{{- range $raw -}}{{- $days = append $days (toString .) -}}{{- end -}}
{{- $_ := set $out "onWindow" (dict "start" (toString $start) "duration" (toString $duration) "days" $days) -}}
{{- else -}}
{{- fail (printf "phoneDefaults.display.%s is not a field; the fields are idleTimeout, wakeOnCall and onWindow" $k) -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}
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
- codec_resample.so
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
- chan_audiosocket.so
- func_timeout.so
{{- end -}}

{{/* Asterisk's config through the live directory's current symlink (compile.AsteriskConfigDir). */}}
{{- define "patchbay.asteriskConf" -}}
/var/lib/patchbay/config/current/asterisk/asterisk.conf
{{- end -}}

{{/*
The sound prompts' voice (sounds.voice), refused unless the image carries
it: the list is internal/agent/sounds.Voices (test/chart compares the two),
whose comment and docs/sounds.md give each voice's licence.
*/}}
{{- define "patchbay.soundsVoice" -}}
{{- $voices := list "espeak" "ljspeech" "kristin" "norman" "john" -}}
{{- $voice := toString .Values.sounds.voice -}}
{{- if not (has $voice $voices) -}}
{{- fail (printf "sounds.voice %q is not a voice the image carries; the voices are: %s (docs/sounds.md)" $voice (join ", " $voices)) -}}
{{- end -}}
{{- $voice -}}
{{- end -}}
