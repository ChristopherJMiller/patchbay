# Operating patchbay

What to look at, and what to do, when something is wrong with an
install. Every entry is symptom, cause, action. The design's failure
table is [design §7](design.md); what release A accepts as a known
limitation is [plan §11](plans/release-a.md#11-known-limitations-accepted-for-a).
`<ns>` below is the install's namespace (`telephony` at home), `<fullname>`
its Helm fullname (`patchbay` when the release is called `patchbay`).

## Where to look

1. **`patchbay status -n <ns>`**, top to bottom:
   - `Agent:` the agent's version, whether Asterisk (its AMI session) is
     up, the config version it applied, and its last failed apply.
   - `Controller <fullname>:` who holds the install's leader-election Lease
     and when it renewed it. "may be down" (no renewal for 30 s) or "nobody
     holds the lease" means nothing decides or compiles, and every status
     below is as the controller last wrote it.
   - `Config:` the version in `pbx-config` beside the one the agent
     applied, and "compiling has failed since …" when the latest
     CompileFailed Event is newer than the latest version written.
   - every Phone, Softphone, Person and Stream: its state, each condition
     that is not healthy, and its **human action** (the one thing a person
     must do). "(its spec changed since; not decided again yet)" means the
     controller has not seen your last edit.
   - the unknown phones: MACs that asked for a config no Phone names.
2. **`kubectl -n <ns> get phones,softphones`**: State, Ready and its
   Reason. In an outage State keeps the last state (design §8.3); Ready and
   Reason say why it is Unknown.
3. **Events** (`kubectl -n <ns> get events`): StateChanged, ActionSent,
   ActionFailed, RequestIgnored, ApplyFailed on the devices; UnknownPhone,
   ConfigWritten, CompileFailed on `<fullname>-pbx-config` (or on
   `<fullname>-settings` before the first bundle). A repeated Event is a
   count on one Event; a new note is a new Event.
4. **Metrics** (the chart's PodMonitor, `podMonitor.enabled`):
   `patchbay_agent_reachable` (0: the controller cannot poll the agent),
   `patchbay_asterisk_up` (0: Asterisk's AMI session is down; absent while
   the agent cannot be reached), `patchbay_observation_age_seconds`,
   `patchbay_compile_failures_total`, `patchbay_apply_failures_total`,
   `patchbay_devices{kind,state}`, `patchbay_registered_endpoints`, and the
   agent's own `patchbay_agent_*`.
5. **Logs**: `kubectl -n <ns> logs deploy/<fullname>-controller`, and the
   PBX pod's containers `agent`, `asterisk`, `fileserver`
   (`kubectl -n <ns> logs deploy/<fullname>-pbx -c agent`).

Restarting the PBX pod, the remedy several entries below name, is
`kubectl -n <ns> delete pod -l app.kubernetes.io/component=pbx,app.kubernetes.io/instance=<release>`.
**Calls in progress drop**, and calls to a phone fail until it registers
again (at most its 600 s expiry; Asterisk's contacts live on an emptyDir).

## The install

| Symptom | Cause | Action |
|---|---|---|
| The PBX ReplicaSet reports `FailedCreate` ("violates PodSecurity") | The namespace does not admit host-network pods. | Label it `pod-security.kubernetes.io/enforce=privileged` (at home: Argo's `managedNamespaceMetadata`). |
| The PBX pod stays in `ContainerCreating` | Its `<fullname>-agent-token` Secret does not exist yet: the controller mints it, and is not running (or not yet). | Look at the controller pod. |
| The controller crash-loops; its log names `settings.json` | A chart value it refuses: a time zone the 79x5 has no name for (the message lists the ones it takes), a number plan that clashes, an `amiSecret` that is not this install's. | Fix the value. The old controller pod keeps running meanwhile (rolling update). |
| The PBX pod is not Ready; the agent's startup probe fails, `/readyz` says no config is live | The agent waits for `pbx-config` and `pbx-secrets`, or refused what it found (`Agent:` and the agent log say why). After a change of `pbx.address` or `pbx.ports.sip`, or a new release, the new PBX pod refuses a version compiled for its old SIP bind or modules until the new controller has compiled ("refused: compiled for SIP on …"): Asterisk would keep the transports it booted with, and no reload moves them. | Normally seconds. If it lasts, the controller is not compiling: see the `Controller:` and `Config:` lines. The startup probe allows 300 s, then the kubelet restarts the agent. |
| `/readyz` names a sound prompt | The image lacks a prompt (a broken image). | Use a released image. |

## Every device at once

| Symptom | Cause | Action |
|---|---|---|
| Ready=Unknown, reason `AgentUnreachable`; `patchbay_agent_reachable` 0 | The controller cannot poll the agent: the agent container is down or restarting, the node is down, or something holds every connection of the control API (:9440, on every address of the node; plan §11). | `kubectl get pods`; the agent log ("connections over the cap are refused" for the last). Calls keep working. |
| Registered=False, reason `PBXDown`; `patchbay_asterisk_up` 0 | Asterisk is down (its container restarting or crash-looping: `kubectl get pods`, its log), or the agent cannot log in to AMI while Asterisk runs (agent log "AMI session not established … Authentication failed": an AMI password change during an Asterisk outage, plan §11). | For a crash, its log. For the login, restart the PBX pod. |
| ConfigCurrent=False `ApplyFailed`, an ApplyFailed Event, `Agent:` "last apply … failed" | The agent refused or rolled back the compiled version; the message is its error. Calls are unaffected: the previous version stays live. | "refused: compiled for SIP on …": see the install table. A verify error on a version the controller compiled is a bug: report it with the message. After an agent restart, a version that failed verification can leave the agent without AMI (`PBXDown` while calls work, plan §11): fix or revert the change, or `patchbay softphone qr <name> --reissue -n <ns>` to make a new version. |
| ConfigCurrent=False `ApplyPending` that does not clear | The agent has not applied the compiled version yet: it is busy verifying another (30 s), or down. | Wait a minute; then the agent log. |
| `Config:` "compiling has failed since …"; CompileFailed Events | Nothing compiles: the previous bundle stays, and no change (a label, a new softphone, a reissue) takes effect. The note says why. A hand-edited device Secret (`patchbay-<id>`) with a value the controller cannot use is the usual one. | Fix the cause. For a Secret, delete it: the controller writes a new one (a new SIP password; a 7945 is resynced to it, a softphone needs `qr --reissue`). |
| `Controller …: may be down` / `nobody holds the lease` | No controller is running: statuses are frozen, new resources wait, nothing compiles. Calls keep working on the applied config. | `kubectl -n <ns> get pods -l app.kubernetes.io/component=controller` and its log. |

## One Phone (cisco79x5)

The human action in `patchbay status` is the instruction; this is the why.

| State (Ready reason) | Cause | Action |
|---|---|---|
| `Unseen` | The phone never asked for its config. | Connect it to a voice-VLAN switch port; it asks for `SEP<MAC>.cnf.xml` from the PBX address (DHCP option 66). Check the MAC in the Phone. |
| `Invalid` (`NumberConflict`, `MACConflict`, `UnknownFirmware`) | An older resource holds its number or MAC, or its model's load is not in the manifest (a 7975 in release A). | Change the number or MAC; the older resource keeps it. |
| `NeedsFirmware` | Its load is not staged, or the manifest has no SHA-256 for it yet (plan §11: staging Cisco firmware needs a patchbay release). | `patchbay firmware load <cisco zip> -n <ns>`; for pending digests, the release first. |
| `NeedsTrustReset` | It trusts another call manager (old trust list): it asks only for the signed `.sgn` config. | Factory reset (hold # while powering on, then 123456789*0#), after the default load is staged. |
| `Upgrading` | It is downloading firmware. | Do not unplug it. "Upgrade stuck" after 30 min: look at its screen. |
| `Registering` | It fetched its config but has not registered; the human action quotes its own log. | Usually a wrong time, VLAN or network; after a renumbering it registers under the old number and needs a power-cycle (plan §11). |
| `ConfigStale` | Its config changed; the controller sends a resync NOTIFY (twice, 5 min apart), never during a call ("waiting for the call to end"). Ready says when a resync did not go out, and it is tried again in 30 s. | Nothing, unless the human action asks: press Settings then **#**, or power-cycle it. |
| `Unreachable` | Its registration lapsed: at most 600 s plus the 10 min grace (plan §11). | Power-cycle it, check its cable. |
| `LabelShortened` True | The 79x5 shows at most 12 characters without spaces on its screen and 30 on its line; runs of `@` become one. | The message says what it shows; shorten the label if you mind. |

`patchbay phone resync <name> -n <ns>` and `patchbay phone restart <name>`
ask for a resync or a reboot (request annotations, acted on once).

## One Softphone

| State | Cause | Action |
|---|---|---|
| `Issued` | A one-time provisioning URL waits to be scanned. | `patchbay softphone qr <name> -n <ns>`, scan it in Linphone. It works once. |
| `Provisioned` | Linphone fetched its settings; the controller has replaced the spent token. | Wait for it to register. |
| `Offline` | Its registration lapsed (the laptop sleeps). | Nothing; it registers when it wakes. |
| Linphone says provisioning failed | The URL was spent (it re-fetches at start), or the softphone was reissued. | `patchbay softphone qr <name> --reissue` and scan the new one. |

## Changes and upgrades

- **A label, number or new device**: compiled and applied by reload, no
  call drops. Phones are resynced when idle.
- **A chart or image upgrade** recreates the PBX pod: calls drop, phones
  register again within 600 s.
- **`pbx.address`, `pbx.ports.sip`, or a release that changes the module
  list** restart the PBX pod and the controller together. The new PBX pod
  waits, not ready, until the new controller has compiled for the new
  settings, then Asterisk starts on them. A new address or SIP port also
  needs the router's voice-VLAN rules and DHCP option 66 to follow, and
  every phone re-fetches its config on its resync.
- **A firmware digest filled in** needs a patchbay release; then
  `patchbay firmware load` the zip.

## Unknown phones

An UnknownPhone Event on `pbx-config` and a line of `patchbay status`
("unknown phone <MAC> requested config from <IP>"): a phone on the voice
VLAN with no Phone resource. Create a Phone with that MAC. A flood of
random MACs delays real ones (plan §11).

## Uninstall

`helm uninstall` keeps the CRDs (and so every Phone and Softphone) and the
firmware volume (`firmware.persistence.keep`). The controller's own objects
are not Helm's; once the controller is gone, delete them by name:

```sh
kubectl -n <ns> delete configmap <fullname>-pbx-config
kubectl -n <ns> delete secret <fullname>-pbx-secrets <fullname>-ami <fullname>-agent-token
```

Not by the `app.kubernetes.io/managed-by=patchbay-controller` label: each
device Secret (`patchbay-<id>`, its SIP password and a softphone's token)
carries it too, and goes with its Phone or Softphone. Delete those as well
and, after a reinstall, every 7945 needs a power-cycle and every softphone
a `qr --reissue`.
