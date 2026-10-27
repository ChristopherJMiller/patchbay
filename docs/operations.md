# Operating patchbay

What to look at, and what to do, when something is wrong with an
install. Every entry is symptom, cause, action. The design's failure
table is [design §7](design.md); what release A accepts as a known
limitation is [plan §11](plans/release-a.md#11-known-limitations-accepted-for-a),
and release B [its plan §11](plans/release-b.md#11-known-limitations-accepted-for-b).
Streams (a number that plays TV or radio) have their own guide,
[docs/streams.md](streams.md), whose troubleshooting table goes from a
Stream's state and message to the cause.
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
   - a Stream also shows its listeners and tuner pool, and its last failure
     while it is not Ready (`Stream kiro7 (707): Busy (pool hdhomerun)`).
   - the unknown phones: MACs that asked for a config no Phone names.
2. **`kubectl -n <ns> get phones,softphones`** (and `streams -o wide`):
   State, Ready and its Reason. In an outage State keeps the last state
   (design §8.3); Ready and Reason say why it is Unknown.
3. **Events** (`kubectl -n <ns> get events`): StateChanged, ActionSent,
   ActionFailed, RequestIgnored, ApplyFailed on the devices; UnknownPhone,
   ConfigWritten, CompileFailed on `<fullname>-pbx-config` (or on
   `<fullname>-settings` before the first bundle); StateChanged,
   SourceFailed and CallRefused on the Streams; MessageShown, MessageHeld,
   PushFailed and the rest on the Messages ([Messages](#messages)). A
   repeated Event is a count on one Event; a new note is a new Event.
4. **Metrics** (the chart's PodMonitor, `podMonitor.enabled`):
   `patchbay_agent_reachable` (0: the controller cannot poll the agent),
   `patchbay_asterisk_up` (0: Asterisk's AMI session is down; absent while
   the agent cannot be reached), `patchbay_observation_age_seconds`,
   `patchbay_compile_failures_total`, `patchbay_apply_failures_total`,
   `patchbay_devices{kind,state}`, `patchbay_registered_endpoints`,
   `patchbay_streams{state}`, `patchbay_stream_bridge_reachable` (0: the
   controller cannot poll the stream bridge), the agent's own
   `patchbay_agent_*` and the bridge's `patchbay_stream_bridge_*`
   (docs/streams.md lists them).
5. **Logs**: `kubectl -n <ns> logs deploy/<fullname>-controller`, and the
   PBX pod's containers `agent`, `asterisk`, `fileserver`
   (`kubectl -n <ns> logs deploy/<fullname>-pbx -c agent`), and the
   stream bridge (`kubectl -n <ns> logs deploy/<fullname>-streams`: one
   line per stream call, and per source started, stopped or failed; never
   audio).

Restarting the PBX pod, the remedy several entries below name, is
`kubectl -n <ns> delete pod -l app.kubernetes.io/component=pbx,app.kubernetes.io/instance=<release>`.
**Calls in progress drop**, and calls to a phone fail until it registers
again (at most its 600 s expiry; Asterisk's contacts live on an emptyDir).

Restarting the stream bridge (`kubectl -n <ns> rollout restart
deploy/<fullname>-streams`, or any upgrade of it) **drops every stream
call**: it is one replica, recreated, never two (two bridges would hold
two tuners for one channel). Phone calls are unaffected, and the Streams
read `Unknown` (`BridgeUnreachable`) until it is back, their history kept.

## The install

| Symptom | Cause | Action |
|---|---|---|
| The PBX ReplicaSet reports `FailedCreate` ("violates PodSecurity") | The namespace does not admit host-network pods. | Label it `pod-security.kubernetes.io/enforce=privileged` (at home: Argo's `managedNamespaceMetadata`). |
| The PBX pod stays in `ContainerCreating` | Its `<fullname>-agent-token` Secret does not exist yet: the controller mints it, and is not running (or not yet). | Look at the controller pod. |
| The controller crash-loops; its log names `settings.json` | A chart value it refuses: a time zone the 79x5 has no name for (the message lists the ones it takes), a number plan that clashes, an `amiSecret` that is not this install's. | Fix the value. The old controller pod keeps running meanwhile (rolling update). |
| The PBX pod is not Ready; the agent's startup probe fails, `/readyz` says no config is live | The agent waits for `pbx-config` and `pbx-secrets`, or refused what it found (`Agent:` and the agent log say why). After a change of `pbx.address` or `pbx.ports.sip`, or a new release, the new PBX pod refuses a version compiled for its old SIP bind or modules until the new controller has compiled ("refused: compiled for SIP on …"): Asterisk would keep the transports it booted with, and no reload moves them. | Normally seconds. If it lasts, the controller is not compiling: see the `Controller:` and `Config:` lines. The startup probe allows 300 s, then the kubelet restarts the agent. |
| The stream bridge pod stays in `ContainerCreating` | Its `<fullname>-streams` ConfigMap (streams.json) does not exist yet: the controller writes it on its first compile, before `pbx-config`. | Look at the controller pod. |
| The stream bridge is not Ready; `/readyz` (port 9093) says why | It has not loaded a valid streams.json yet, or `ffmpeg -version` fails in its image. | Its log; `streamsError` in its observations (docs/streams.md). |
| The controller exits at start naming `streams.` or `streamBridge` | A stream setting it refuses: `streams.callLimit` outside 10s to 24h or not whole seconds, a tuner pool whose name is not a DNS label or repeats, a host in two pools, `max` outside 1 to 16. | Fix the value; the message names the field. |
| `/readyz` names a sound prompt | The image lacks a prompt in the voice `sounds.voice` picks (a broken image). | Use a released image. |
| The agent exits at start naming `--sounds-voice` | A voice the image does not carry; the chart refuses one (`helm template` fails), so the pod spec was edited by hand. | Set `sounds.voice` to one of the voices it lists (docs/sounds.md). |

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
| `Invalid` (`NumberConflict`, `MACConflict`, `UnknownFirmware`, `UnsupportedFeature`) | An older resource holds its number or MAC, its model's load is not in the manifest (a 7975 in release A), or its spec asks for a feature its model lacks (`spec.display` without `displayPowerSave` in `patchbay models`). | Change the number or MAC; the older resource keeps it. Remove the feature the message names. |
| `NeedsFirmware` | Its load is not staged, or the manifest has no SHA-256 for it yet (plan §11: staging Cisco firmware needs a patchbay release). | `patchbay firmware load <cisco zip> -n <ns>`; for pending digests, the release first. |
| `NeedsTrustReset` | It trusts another call manager (old trust list): it asks only for the signed `.sgn` config. | Factory reset (hold # while powering on, then 123456789*0#), after the default load is staged. |
| `Upgrading` | It is downloading firmware. | Do not unplug it. "Upgrade stuck" after 20 min: look at its screen. |
| `Registering` | It fetched its config but has not registered; the human action quotes its own log. | Usually a wrong time, VLAN or network; after a renumbering it registers under the old number and needs a power-cycle (plan §11). |
| `ConfigStale` | Its config changed; the controller sends a resync NOTIFY (twice, 5 min apart), never during a call ("waiting for the call to end"). Ready says when a resync did not go out, and it is tried again in 30 s. | Nothing, unless the human action asks: press Settings then **#**, or power-cycle it. |
| `Unreachable` | Its registration lapsed: at most 600 s plus the 10 min grace (plan §11). | Power-cycle it, check its cable. |
| `LabelShortened` True | The 79x5 shows at most 12 characters without spaces on its screen and 30 on its line; runs of `@` become one. | The message says what it shows; shorten the label if you mind. |

`patchbay phone resync <name> -n <ns>` asks for a resync (the request
annotation `patchbay.chrismiller.xyz/resync-requested`, acted on once;
held while the phone is on a call).

### The Directories and Services buttons

**Directories** lists the phone's own Missed, Received and Placed Calls,
then patchbay's two lists; **Services** opens the same two lists alone:

- **Extensions**: every Phone and Softphone that holds its number, and
  every Person who rings at least one of them, by number.
- **Streams**: every Stream whose number dials the stream bridge, by
  number. Absent while no Stream can play: a Stream with a refused
  source, and every Stream of an install without a bridge, plays "not
  available" and is not offered.

Pick an entry and press Dial. Each list holds at most 32 entries (Cisco's
limit; the highest numbers are dropped, and the file says how many) and
cuts names to 32 characters. The phone fetches them from device HTTP
(`http://<pbx.address>:8088/directory.xml` and `/services.xml`, which open
`/directory/extensions.xml` and `/directory/streams.xml`) on every press,
so a new Phone, Person or Stream shows on the next press after the agent
has applied its version: no resync is needed.

| Symptom | Cause | Action |
|---|---|---|
| Services shows nothing, Directories works | The phone still runs a SEP file without `servicesURL` (rendered before the Services menu). | `patchbay status`: a `ConfigStale` phone is resynced on its own, never during a call. |
| Directories shows only the call histories | The phone cannot reach device HTTP :8088, or gets no menu there. | curl `http://<pbx.address>:8088/directory.xml` from the voice VLAN; the agent's observations record each device HTTP fetch (transport `http8088`). |
| An entry is missing | Over 32 entries, a Stream that cannot play (above), or a Phone or Softphone that does not hold its number (`Invalid`). | The resource's status says why. |

### Pushed messages (bench 2026-10-27)

Bench results on a CP-7945G (SIP 9.4(2)SR4-3), patchbay 0.4.0-rc.1:

- An authenticated `/CGI/Execute` push is accepted (`CiscoIPPhoneResponse`, Status 0). The phone checks the credentials with a GET to `authenticationURL`, passing `UserID`, `Password` and `devicename`.
- `Play:patchbay-chime.raw` (from the TFTP tree) chimes. During a Stream call the pushed text shows, the chime plays, and the stream's audio continues.
- A push wakes a screen that display power-save turned off.
- A newer push replaces the shown text.
- `Init:Services` returns the phone to its idle screen.
- Exit closes the message back to idle.

Check push on a phone with `patchbay phone test-push <phone> [--text …] [--beep] [--clear] [--execute URI]…`.

## Messages

Text on a desk phone's screen, from Home Assistant or a shell. Each
message is a `Message` resource naming one Phone; the controller alone
decides when it shows and pushes the phone its page through the agent
(the bench results above are what the 7945 does with it). The design is
[docs/superpowers/specs/2026-10-26-phone-messages-design.md](superpowers/specs/2026-10-26-phone-messages-design.md).

### Sending

From a shell, with your kubeconfig:

```sh
patchbay phone message desk "Dinner!" -n <ns>      # prints the Message's name
patchbay phone message desk "Water heater leak" --title Leak --urgent --expires 2h -n <ns>
patchbay phone message desk "Fridge door open" --key fridge-door -n <ns>
patchbay phone withdraw desk --key fridge-door -n <ns>
patchbay messages -n <ns>                          # every Message: state, reason, key, sent, expires
```

The text is 1 to 256 characters and the title at most 32, neither with
control characters; `--expires` is 0s to 24h (default 1h); `--key` is a
DNS label. `patchbay messages` never prints the text (`kubectl -n <ns> get
message <name> -o yaml` does), and `patchbay status` lists the Held ones
with their reason. `kubectl -n <ns> get messages` shows phone, state,
reason, key, urgent and expiry.

From inside the cluster without Kubernetes credentials (Home Assistant):
the messages endpoint, which the chart turns on with `messages.enabled`.
It is the Service `<fullname>-messages` on port `messages.port` (9441),
ClusterIP only, served by every controller replica. Every request needs
`Authorization: Bearer <token>`, the token in the Secret
`messages.tokenSecret.name` (key `messages.tokenSecret.key`, default
`token`), which you create: the controller never mints it, since the
caller holds the same value. It is read on every request, so a rotation
needs no restart, and white space around it (the newline `echo` leaves)
is ignored.

| Request | Body | Answer |
|---|---|---|
| `POST /v1/messages` | `{"to": ["desk"], "text": "Dinner!", "title": "", "urgent": false, "expires": "1h", "key": ""}` (only `to` and `text` required) | 201 `{"names": [...]}`: one Message per phone |
| `POST /v1/withdraw` | `{"key": "fridge-door", "to": ["desk"]}` (no `to`: every phone) | 200 `{"withdrawn": 1}` |
| `PUT /v1/house` | `{"quiet": true}`, `{"away": false}` or both | 200 the house as stored: `{"quiet": true, "away": false, "updatedAt": "…"}` |
| `GET /v1/house` | | 200 the house |

An error is `{"error": "…"}`: 400 for a body that is not exactly the
fields above or breaks a limit (the text says which), 401 for a missing or
wrong token, 413 for a body over 8 KiB, 503 while the token Secret or its
key does not exist. The house state lives in the ConfigMap
`<fullname>-house` (`house.json`), so it survives a restart; a house never
told anything is neither quiet nor away, and messages are delivered.

### What the phone shows

Every current message, newest first, each with the time it was sent and
`URGENT` before an urgent one's title ("18:02  URGENT Leak: Water heater
leak"), under the title `patchbay` and a count. A message that lives over
12 hours (`--expires`) has the weekday before its time ("Mon 09:00"),
since it can still be up the next day. A page with a message the
phone has not shown before plays the chime first; a page that only drops
one (expired, withdrawn) or is sent again does not. When none is left the
screen is cleared, back to the idle screen. Exit closes the page on the
phone, but the messages stay current: the next push shows them again. The
page holds up to 4000 bytes of text; past that the oldest are dropped for
a "+N older" line.

The page holds only what a 7945's font can show (English locale,
Latin-1): typographic quotes, dashes and `…` become `'`, `"`, `-` and
`...`, every other character past Latin-1 is dropped (an emoji: "💧 Leak
detected" shows "Leak detected"), and runs of spaces become one. Accented
Latin-1 letters stay (`café`). A title left empty is no title; a text left
empty shows `(no text)`.

A phone that restarts (a resync, which reboots a 79x5 and so follows
every config change; a power cut) comes back on its idle screen. The
agent sees it register under a new Call-ID, stops reporting its page, and
the controller sends the page again, without the chime.

### States and reasons

| State (reason) | Meaning |
|---|---|
| `Held` (`InCall`) | A person-to-person call is up on the phone, ringing included. Shown when it ends. |
| `Held` (`QuietHours`) | The house is quiet and the message is not urgent. |
| `Held` (`Away`) | Nobody is home and the message is not urgent. |
| `Held` (`PhoneUnreachable`) | The agent does not answer, or the phone has no live registration. Shown once it is back. |
| `Held` (`PushFailing`) | The last push to the phone failed; the status message says why. Retried 30 s after the failure, with the message on the page. |
| `Shown` | On the phone's screen since `shownAt`. It stays Shown, and on every page, until it expires, is withdrawn or is replaced: a call, quiet hours or away never take a shown message back. |
| `Expired` | `expiresAt` passed; one that arrived expired is never shown. |
| `Withdrawn` | Withdrawn by its key. |
| `Replaced` | A newer message to the same phone with the same key. |
| `Failed` (`PhoneMissing`) | No Phone of that name. |
| `Failed` (`PushUnsupported`) | Its model takes no pushes (`patchbay models`). |

Expired, Withdrawn, Replaced and Failed are final; the controller deletes
each a day after its `finishedAt`. Each change is an Event on the Message:
`MessageShown`, `MessageHeld`, `MessageExpired`, `MessageWithdrawn`,
`MessageReplaced`, and the Warnings `MessageFailed` and `PushFailed`.

### When a message waits

- **A person-to-person call** holds every message, urgent or not, ringing
  included, and nothing at all is pushed to a phone while it lasts. **A
  Stream call** (radio, TV) does not hold: the text shows and the chime
  plays over the stream.
- **Quiet hours and away** hold the messages that are not urgent. Urgent
  skips both, never a call. Home Assistant owns both and PUTs them;
  patchbay keeps the last state it was told. A `house.json` that does not
  parse counts as quiet (only urgent messages go out), logged once.
- **Expiry**: 1 hour after it was sent unless the sender says otherwise
  (0s to 24h). An expired message leaves the queue and the screen.
- **A key**: a newer message with the same key, to the same phone,
  replaces the older one (a door that keeps opening shows once), and
  withdrawing by key takes back every unfinished one with it, from the
  queue or the screen.

### Troubleshooting

| Symptom | Cause | Action |
|---|---|---|
| `Held` `PushFailing`, `PushFailed` Events | The phone refused the push, or did not answer; the message names the agent's reason and the phone's answer. | `patchbay phone test-push <phone> -n <ns>`: one push straight through the agent, printing the phone's answer (`status 0` is accepted). Then the agent's log (`kubectl -n <ns> logs deploy/<fullname>-pbx -c agent`). |
| test-push is refused (the phone answers `error 4` or `http 401`), and the agent logs a `device HTTP authenticate` line with `result=UN-AUTHORIZED` | The phone asked its authenticationURL with other credentials than the agent's: a push from something other than the agent (rightly refused), or a bug, since the agent answers with the credentials it pushes with. | The line names the method and the parameters the phone sent, never their values; report a refused test-push with it. |
| test-push is refused and the agent logs no `device HTTP authenticate` line | The phone did not ask: it runs a SEP file without `authenticationURL` (from before 0.4.0, resynced once on the upgrade and never during a call), or cannot reach device HTTP :8088. | `patchbay status`: a ConfigStale phone resyncs when idle; `patchbay phone resync <name>`. From the voice VLAN, `http://<pbx.address>:8088/authenticate` answers `UN-AUTHORIZED`. |
| `Held` `PhoneUnreachable` | The phone is not registered, or the controller cannot poll the agent. | The Phone's own state (`Unreachable`, `AgentUnreachable`): its table above. |
| Everything not urgent stays `Held` `QuietHours` or `Away` | Home Assistant's last PUT said so, or `house.json` does not parse (the controller log says so). | `GET /v1/house`, or `kubectl -n <ns> get configmap <fullname>-house -o yaml`; PUT the state again. |
| The endpoint answers 503 | The token Secret, or its key, does not exist. | Create it in the install's namespace. |
| The endpoint answers 401 | The caller's token is not the Secret's. | Put the same value on both sides. |
| A message shows with no chime | Only a page with a message the phone never showed chimes; a re-send (after an agent or phone restart) and a withdraw or expiry are silent. | Nothing. |

Known limits:

- **A test-push to a phone that has Messages** does not stay: while one
  is current the controller pushes its own page over it within seconds,
  and with only finished ones (kept a day) its next decision clears it.
- **After an agent restart** the agent has lost its pages, and the
  controller sends the current page again, without the chime. If every
  message finishes before that re-send, nothing is sent and the old text
  stays on the screen until Exit.
- **A phone that never comes back** keeps its last Message past the day:
  the phone is still owed a clear, and the controller keeps the Message,
  and goes on looking, until the phone takes one.
- **`/authenticate` has no throttling.** It is on device HTTP :8088,
  reachable from the voice VLAN, and answers whether a user and password
  are the push account's; the password is 16 random alphanumerics (about
  95 bits) the controller generated (`<fullname>-push`).

### Upgrading to 0.4.0

The SEP file gains `authenticationURL` and turns `phonePersonalization`
on, so every Cisco phone's config changes: each resyncs once, never
during a call. An agent on the new image refuses a bundle the old
controller compiled (it has no push password) and waits, not ready, for
the new controller's first compile; the upgrade rolls both together, so
that is normally seconds.

## One Softphone

| State | Cause | Action |
|---|---|---|
| `Issued` | A one-time provisioning URL waits to be scanned. | `patchbay softphone qr <name> -n <ns>`, scan it in Linphone. It works once. |
| `Provisioned` | Linphone fetched its settings; the controller has replaced the spent token. | Wait for it to register. |
| `Offline` | Its registration lapsed (the laptop sleeps). | Nothing; it registers when it wakes. |
| Linphone says provisioning failed | The URL was spent (it re-fetches at start), or the softphone was reissued. | `patchbay softphone qr <name> --reissue` and scan the new one. |

### Linphone's contacts

Linphone shows the Extensions and Streams lists above as read-only
contacts: every entry, names whole (Cisco's 32-entry and 32-character
limits do not apply). Its provisioning document names
`http://<pbx.address>:8088/directory/contacts.vcf`, a vCard 4.0 list that
Linphone downloads again at every start, so a new Phone, Person or Stream
shows after Linphone restarts (on a phone, quit the app; it otherwise
keeps running). A download that fails keeps the contacts it had.

A softphone provisioned before patchbay 0.3.0 has none: the new
document changes its settings, so it shows `Issued` with "its settings
changed: scan `patchbay softphone qr <name>` in Linphone again". Scan it
once; no `--reissue` is needed, as the controller publishes the URL.

| Symptom | Cause | Action |
|---|---|---|
| No patchbay contacts | Linphone runs settings from before 0.3.0, or cannot reach device HTTP :8088. | `patchbay status` (above); curl the URL from Linphone's network. |
| A new entry is missing | Linphone has not restarted since, or its last download failed. | Restart Linphone. |

## Changes and upgrades

- **A Stream** (new, renumbered, a new source): compiled into
  `<fullname>-streams` before `pbx-config`, then `Pending` until the
  kubelet syncs the bridge's mounted copy (usually under a minute, up to
  about two); a source change alone rewrites only `<fullname>-streams` (no
  PBX reload). Calls already playing keep their source until they hang up.
- **The stream settings** (`streams.callLimit`, `streams.tunerPools`)
  roll the controller only; the new limit is in the next compiled
  dialplan, the new pools in the next streams.json.
- **A label, number, `spec.display` or new device**: compiled and applied
  by reload, no call drops. Phones are resynced when idle.
- **The house default display** (the chart value `phoneDefaults.display`,
  documented in values.yaml) rolls the controller only, never the PBX pod;
  every phone that takes it is resynced. A value the controller refuses
  (not whole minutes, a bad start or day) stops the new controller at
  start, naming the field: `kubectl logs` it, fix the value.
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
- **0.4.0** (phone messages) resyncs every Cisco phone once:
  [Upgrading to 0.4.0](#upgrading-to-040).

## Unknown phones

An UnknownPhone Event on `pbx-config` and a line of `patchbay status`
("unknown phone <MAC> requested config from <IP>"): a phone on the voice
VLAN with no Phone resource. Create a Phone with that MAC. A flood of
random MACs delays real ones (plan §11).

## Uninstall

`helm uninstall` keeps the CRDs (and so every Phone, Softphone and
Message) and the firmware volume (`firmware.persistence.keep`). The
controller's own objects are not Helm's; once the controller is gone,
delete them by name (`<fullname>-house` exists once the messages endpoint
was told a house state):

```sh
kubectl -n <ns> delete configmap <fullname>-pbx-config <fullname>-streams <fullname>-house --ignore-not-found
kubectl -n <ns> delete secret <fullname>-pbx-secrets <fullname>-ami <fullname>-push <fullname>-agent-token
```

The messages token Secret is yours, not the controller's: delete it with
whatever created it.

Not by the `app.kubernetes.io/managed-by=patchbay-controller` label: each
device Secret (`patchbay-<id>`, its SIP password and a softphone's token)
carries it too, and goes with its Phone or Softphone. Delete those as well
and, after a reinstall, every 7945 needs a power-cycle and every softphone
a `qr --reissue`.
