# Release A ("core calling"): implementation plan

**Status:** plan, 2026-10-14; adversarially reviewed 2026-10-15 (changes
are marked *review*, and checked facts carry their source and date).
**Spec:** [docs/design.md](../design.md) (§11, A).
**Contracts landed with this plan:** commit `f346cf7` ("contracts: release A
shared contracts …"), amended by the review's `8971df8` (§3). Read
[CLAUDE.md](../../CLAUDE.md) first.

Release A is built in three workflows, each implement → review → fix:

| Workflow | Builds | Touches |
|---|---|---|
| **W1: the pure core** | `internal/drivers/cisco79x5`, `internal/drivers/softphone`, `compile.Compile` and its helpers, `internal/drivers/builtin` | no I/O, no cluster |
| **W2: agent + chart** | apply/verify/rollback, TFTP/HTTP serving, device HTTP, AMI, observations, actions, scraper, firmware store and upload, sound prompts, chart | the PBX pod |
| **W3: controller + CLI + e2e loop** | reconcilers, Secrets, Compile → ConfigMap, polling, Decide orchestration, actions, conditions/events/metrics, conflicts, requests; CLI; e2e loop and rollback | the controller, `cmd/patchbay`, `test/e2e` |

W1 is specified at task level (files, signatures, tests). W2 and W3 are
specified at component level, with the interfaces between them exact.

Contents: [1 Global constraints](#1-global-constraints) ·
[2 Decisions made now](#2-decisions-made-now) ·
[3 Contracts landed](#3-contracts-landed-commit-f346cf7) ·
[4 W1 tasks](#4-w1-the-pure-core-task-level) ·
[5 W2](#5-w2-agent-and-chart-component-level) ·
[6 W3](#6-w3-controller-cli-and-e2e-loop-component-level) ·
[7 Agent ↔ controller interfaces](#7-agent--controller-interfaces) ·
[8 Bench switches](#8-bench-test-switches-86) ·
[9 Exit criteria → tests](#9-exit-criteria--acceptance-tests) ·
[10 Review Focus](#10-review-focus) ·
[11 Known limitations](#11-known-limitations-accepted-for-a)

---

## 1. Global constraints

Every concrete value the design fixes, copied verbatim. Where a value comes
from the research notes rather than the design, it says *(research)*. Where
this plan decides it, it says *(plan)*.

### 1.1 Resources and numbers (§4)

- API group `patchbay.chrismiller.xyz/v1alpha1`. Every resource is namespaced,
  in the PBX's namespace; the controller watches only that namespace.
- "One resource per dialable thing, and it carries its own number."
- A number is a string of digits, quoted in YAML (`number: "100"`). The API
  server rejects a bare integer.
- "On every pass the controller builds the full number table from all
  resources. That build is a pure function."
- "If two resources claim a number, the older one (by `creationTimestamp`)
  keeps it. The newer one gets `NumberConflict=True` and is not routed."
  MACConflict likewise, oldest wins.
- **Number plan** (lives in chart values; the controller enforces it; the CRD
  only checks digits):

  | Range | Meaning | Release |
  |---|---|---|
  | `1XX` | Phones and softphones (Desk = `100`, softphone = `150`) | A |
  | `2XX` | Intercom / auto-answer | reserved |
  | `3XX` | People | A |
  | `600` | Echo test (built in) | A |
  | `7XX` | Streams | B |
  | `0`, `9…`, `*8` | Assistant, outside line, page-all | reserved: plays "not available" |
  | `911` | Plays "this phone cannot call emergency services" (built in). | A |

  As `compile.DefaultNumberPlan()`: Devices `1XX`, People `3XX`, Streams
  `7XX`, EchoTest `600`, Emergency `911`, Reserved `2XX`, `0`, `9.`, `*8`.

### 1.2 Secrets (§4)

- Generated, not committed; each in a Secret the resource owns.
- One random SIP password per `Phone` and per `Softphone`. **`cisco79x5`: 12
  alphanumerics** (about 71 bits). **`softphone`: 24.** "The 79x5 reportedly
  fails with 32-character passwords and is known to work at 12 or fewer."
- Each softphone's provisioning token; the shared AMI password and agent
  token.
- "A lost Secret heals itself. The controller generates a new password, the
  config version changes, and the phone is re-provisioned with no human
  involved (a softphone needs its QR code re-scanned)."
- Names *(plan, landed)*: device Secret `patchbay-<device ID>` with keys
  `sip-password` and (softphones) `provisioning-token`.

### 1.3 The 7945 facts (§4, research)

- `phoneLabel` is at most **12** characters with **no spaces**: spaces
  removed, then truncated to 12; a longer or spaced label gets a warning
  condition (`LabelShortened`, *plan*). Line label and caller ID name use
  `label` as written (**≤ 30**).
- Registration: line `proxy=USECALLMANAGER`, `registerWithProxy`, **TCP**
  (`transportLayerProtocol=1`), Asterisk `force_rport=no`,
  `rewrite_contact=no`. Caller ID updates: `remotePartyID=true` with
  `send_rpid=yes`.
- Time: NTP by **IP address** (`192.168.20.1`). IANA zone → Windows name:
  `America/Los_Angeles` → `Pacific Standard/Daylight Time`.
- Codecs: **G.722** between phones (`advertiseG722Codec`), **G.711µ**
  otherwise. DTMF RFC 4733 on payload **101** (`dtmfAvtPayload=101`).
- **PC port off** (`pcPort=1`). Web UI **on** (read-only, a sensor;
  `webAccess=0` means enabled, *research*). **SSH off** (`sshAccess=1`,
  *research*: 0 = enabled). Settings menu restricted (`settingsAccess`:
  usecallmanager documents only 0 = disabled and 1 = enabled; CUCM's
  "Restricted" is `2`, *review*, a bench check in §8).
- A `dialplan.xml` dial template generated from the number plan, so complete
  numbers dial at once instead of after the phone's **15 s** timeout.
- *(research)* SEP file name `SEP` + **upper-case** MAC + `.cnf.xml`
  (case-sensitive); `featureID` 9 = Line; `featureLabel` ≤ 30; `maxNumCalls`
  ≤ 50; `loadInformation` without `.loads` (blank = keep); `versionStamp`
  bumps on every change; URLs ≤ 255 characters; media ports default
  16384–32766; webAccess/sshAccess 0 = enabled.
- *(review, checked against https://usecallmanager.nz/sepmac-cnf-xml.html
  on 2026-10-15)* line `name`, `displayName`, `authName`, `authPassword`
  and `featureLabel` are each **at most 30 characters**, so the device ID
  (the authName) is capped at 30 (`drivers.DeviceIDMaxLength`);
  `processNodeName` and the NTP `name` at most 50; `remotePartyID` lives in
  `sipProfile/sipStack`; `remoteCcEnable` "must be set to true";
  `sipStack/timerRegisterExpires` is the REGISTER expiry the phone asks for
  (sample 3600); the time-zone list includes every name in T1's table.

### 1.4 PBX, ports, flow (§5, §6)

- SIP `192.168.0.240:5060` udp/tcp; RTP `10000–10099`; TFTP `69`; HTTP
  `6970` (same files as TFTP); device HTTP `8088`; control API `9440`
  (token). AMI on `127.0.0.1` (5038) with a generated password; Asterisk's
  HTTP and ARI disabled.
- PJSIP ACL accepts registrations only from `192.168.20.0/24` and
  `192.168.0.0/24`. No anonymous endpoint, no NAT rule, no trunk.
- Accepted: `SEP<MAC>.cnf.xml` carries the line password in clear over TFTP.
- Compile → ConfigMap `pbx-config` (placeholders `@@secret:<name>/<key>@@`,
  no secrets); `version` = SHA-256 over the files plus the values of the
  secrets they reference.
- Apply (commit-confirm): resolve placeholders into a fresh directory
  (refuse the whole version if any is unresolvable) → swap atomically
  (symlink) → AMI `module reload res_pjsip.so`, then `dialplan reload` →
  verify endpoints and contexts match the bundle → on mismatch swap back,
  reload, report `applyFailed: <reason>`. "The last good config keeps
  running."
- Agent is a native sidecar; its startup probe passes once the first version
  is applied, so Asterisk starts only after valid config exists.
- Observations polled "every few seconds" *(plan: every 5 s; stale after
  30 s)*; web pages polled **every 60 s while not Ready, every 5 min once
  Ready**.
- Resync = `NOTIFY Event: service-control`, body `action=restart` plus the
  contact's `RegisterCallId`, via `PJSIPNotify`. Restart = same with
  `action=reset`. "No second resync of the same version within 5 minutes."
- Discovery Event text: `unknown phone D0C7… requested config from
  192.168.20.101`.
- Chart: controller Deployment, 1 replica, leader election; PBX Deployment
  `replicas: 1`, `strategy: Recreate`, `hostNetwork: true`, `dnsPolicy:
  ClusterFirstWithHostNet`, `nodeSelector`; firmware PVC `firmware` 1 Gi
  RWO; Kubernetes ≥ 1.33; PodMonitor label `release: prometheus`.
- Metrics (controller and agent): devices by state, registered endpoints,
  applied config version, apply failures, observation age.

### 1.5 State machines (§8)

- Decide is pure; an ordered rule table, first match wins; memory only in
  status (`observed.firstSeen`, `observed.firmware`,
  `observed.lastConfigFetch{version, at}`, `observed.lastRegistered`,
  `observed.upgradeStartedAt`, `lastAction{kind, version, at, attempts}`).
- Conditions: `Ready`, `FirmwareAvailable`, `FirmwareCurrent`, `Provisioned`,
  `Registered`, `ConfigCurrent`, plus `Unsupported` / `NumberConflict`
  (and `MACConflict`, `LabelShortened`, *plan*).
- **Timeouts:** NeedsTrustReset **3 min** without progress; Upgrading: a
  firmware fetch within **3 min**, stuck after **20 min**; Registering
  **5 min**; ConfigStale resync **at most once per version per 5 min**,
  human action **after 2 attempts**; Unreachable **10 min grace**.
- Default load **`SIP45.9-4-2SR4-3S`** (`cmterm-7945_7965-sip.9-4-2SR4-3`,
  8 files, ~10.5 MB): `SIP45.9-4-2SR4-3S.loads` 673,
  `apps45.9-4-2SR4-3.sbn` 4,637,904, `cnu45.9-4-2SR4-3.sbn` 582,170,
  `cvm45sip.9-4-2SR4-3.sbn` 2,693,976, `dsp45.9-4-2SR4-3.sbn` 364,907,
  `jar45sip.9-4-2SR4-3.sbn` 2,260,034, `term45.default.loads` 673,
  `term65.default.loads` 673. Fallback `9-4-2SR3-1` (not in the A manifest).
- Factory reset: "hold # while powering on, then 123456789\*0#". It boots
  whatever `term45.default.loads` TFTP serves.
- Signalling: one install-wide setting `pbx.signalling`; only `pjsip`. On
  PJSIP the 79x5 profile marks **unsupported**: BLF, Park and Conference
  softkeys, DND/call-forward sync, auto-answer intercom, a second line.
  **Works:** hold/resume, transfer (bench), pickup and MeetMe via dialplan,
  distinctive ring via `Alert-Info`, G.722, RFC 4733, caller ID via RPID.
- §7 failures: controller down → calls keep working, status stops; agent
  down → `Registered=Unknown`, `AgentUnreachable`; Asterisk down →
  `Registered=False`, `PBXDown`; bad config → `ConfigCurrent=False`,
  `ApplyFailed` + Event.

### 1.6 Out of scope for A

PSTN, AI, voicemail/MWI, paging/intercom, TLS/SRTP, softphones away from home,
more than one PBX, moving the PBX, Grafana, streams (B), phone extras (C).

---

## 2. Decisions made now

### (a) How a driver tells Compile its PJSIP endpoint options

**Decision (landed):** a typed `drivers.EndpointOptions` on
`Profile.Endpoint`, per model. Compile renders it; the driver never writes
Asterisk text.

```go
type EndpointOptions struct {
    Transport          SIPTransport  // "udp" | "tcp" -> transport-<udp|tcp>
    Codecs             []string      // allow=, in order; from KnownCodecs {g722, ulaw, alaw}
    ForceRport         bool          // force_rport
    RewriteContact     bool          // rewrite_contact
    RTPSymmetric       bool          // rtp_symmetric
    DirectMedia        bool          // direct_media (false in A)
    SendRPID           bool          // send_rpid
    SendPAI            bool          // send_pai
    TrustIDOutbound    bool          // trust_id_outbound
    QualifyFrequency   time.Duration // AOR qualify_frequency; 0 = off (bench)
    RegistrationExpiry time.Duration // AOR default_expiration = maximum_expiration
}
```

The compiler owns what every endpoint gets: `context`, `auth` (userpass,
username = device ID), `aors` = number, `identify_by = auth_username`,
`disallow = all`, `dtmf_mode = rfc4733`, `media_address` and
`bind_rtp_to_media_address = yes` (PBX address; docs/e2e.md "RTP source
address"), `contact_deny`/`contact_permit` (the allowed networks), and an AOR
with `max_contacts = 1`, `remove_existing = yes`. `NewRegistry` validates
every model's options (`ValidateProfile`).

*Why:* a free-form `map[string]string` lets a driver inject any option (or a
newline) into pjsip.conf and cannot be validated; a driver-rendered
pjsip.conf snippet breaks the rule that drivers write only their own tree.
A struct is printable by `patchbay models`, testable, and the bench answers
(qualify, expiry) become one-line changes to Go data.

Values *(plan)*:

| Field | `cisco79x5` | `softphone` |
|---|---|---|
| Transport | `tcp` | `udp` (Linphone's default for third-party SIP; LAN only) |
| Codecs | `g722`, `ulaw` | `g722`, `ulaw` |
| ForceRport / RewriteContact / RTPSymmetric | no / no / no | yes / yes / yes |
| DirectMedia | no | no |
| SendRPID / SendPAI / TrustIDOutbound | yes / no / yes | no / yes / yes |
| QualifyFrequency | 0 (bench §8.6) | 0 |
| RegistrationExpiry | 600 s, **also rendered into the SEP file** as `sipStack/timerRegisterExpires`, so the phone asks for exactly what Asterisk grants | 600 s (also in the provisioning document) |

*Review change:* the 79x5's expiry was 3600 s, "the phone's default; bench
whether it honours less". The SEP file sets the expiry the phone requests
(`timerRegisterExpires`, usecallmanager's sample), so there is nothing to
bench: Render writes the profile's value and the two sides agree by
construction. 600 s bounds Unreachable detection at about 20 min (expiry
plus the 10 min grace) instead of 70, and bounds how long a phone stays
uncallable after the PBX pod is recreated (contacts live in an emptyDir).

### (b) The firmware manifest before genuine SHA-256s exist

**Decision (landed):** `drivers.SHA256Pending` (`"pending"`) is the digest of
every file until Chris takes the real one from Cisco's zip. One line per file
changes. `ValidateFirmware` accepts it; `FirmwareFile.Pending()`,
`FirmwareLoad.PendingFiles()`, and `FirmwareLoad.Missing(store)` (a pending
file is *always* missing) make it impossible to treat as verified:

- the CLI refuses it locally ("the manifest has no SHA-256 for X yet");
- the agent's upload answers **412** and stores nothing;
- TFTP never serves a pending file (it cannot be in the store);
- `FirmwareAvailable` is `False`, reason `ManifestPending`, and NeedsFirmware's
  human action says the manifest needs the digests.

An empty digest is still invalid, so a forgotten field is an error, not
silently pending. Sizes stay from the research notes and are advisory.

**e2e test load:** a load that exists only in binaries built with the `e2e`
build tag (`internal/drivers/cisco79x5/e2eload_e2e.go`, `//go:build e2e`),
whose files are generated deterministically by `cisco79x5.E2ELoadFile(name)`
(untagged, so the harness can build the zip) and whose digests are computed
from those bytes. It is genuinely verified, just not Cisco's. The e2e images
are built with `GO_TAGS=e2e` (Dockerfile build arg); the production image and
`make build` are not, and a unit test asserts the default build's manifest has
no `E2E` load. Phones in e2e select it with `spec.firmware`. *Why not a
runtime flag or a manifest file:* the manifest is code so that nothing outside
the binary can make the agent accept a file; a build tag keeps that true for
every production binary.

### (c) Sound prompts for 911 and "not available"

**Decision:** text-to-speech at **image build**, from text in the repo, with a
SIT tone in the dialplan.

- `hack/gen-sounds.sh` (run by `make sounds`, inside the flake) renders each
  prompt with **espeak-ng** (added to `flake.nix`, pinned by `flake.lock`)
  using espeak's own `en-us` voice (never an mbrola `mb-*` voice, whose data
  has its own licence), and converts it with ffmpeg (already in the shell)
  to `<name>.ulaw` (8 kHz) and `<name>.g722` (16 kHz) under `build/sounds/`.
  The Dockerfile copies them to `/usr/share/patchbay/sounds/`; `make image`
  depends on `make sounds`. Two repo files must change with it, or the build
  silently ships no prompts: **`.dockerignore`** admits only go.mod, go.sum,
  api/, cmd/ and internal/ today, so it gains `!build/sounds/`; and
  **`.gitignore`** gains `/build/`.
- *Checked in review (2026-10-15):* with the flake's pinned nixpkgs,
  `espeak-ng -v en-us -s 150 -w x.wav '<text>'` then
  `ffmpeg -i x.wav -ar 8000 -ac 1 -f mulaw -acodec pcm_mulaw x.ulaw` and
  `ffmpeg -i x.wav -ar 16000 -ac 1 -acodec g722 -f g722 x.g722` work
  (espeak-ng 1.52.0.1, ffmpeg 9.0.1), and two runs give byte-identical
  files (the 911 prompt is about 45 KB, 5.7 s, per format).
- Texts (verbatim): `no-emergency`: "This phone cannot call emergency
  services. Hang up and use a mobile phone." `not-available`: "That number is
  not available."
- The agent copies them into the `sounds` emptyDir at start (before
  `/readyz`); the chart mounts it at `compile.SoundsDir`
  (`/var/lib/patchbay/sounds`) in the agent and read-only in Asterisk. The
  dialplan plays them by absolute path.
- Dialplan: `Answer()`, the SIT tone with `Playtones(!950/330,!1400/330,!1800/330)`
  (no file), `Wait(1)`, then the prompt. 911 repeats its prompt three times.
  If the file is ever missing, the caller still hears the SIT tone.

*Why:* the Asterisk core sounds (CC-BY-SA 3.0) have no "cannot call emergency
services" prompt, so 911 would play something generic exactly when clarity
matters most; they also need a pinned download and attribution. Tones alone
fail the exit criterion ("plays its announcement"). TTS gives the exact
wording, has no third-party audio to license (espeak-ng's GPL covers the
program, not its output), commits nothing binary, and is reproducible through
the flake. The cost is a robotic voice, which is acceptable.

### (d) Bundle → ConfigMap encoding under 1 MiB

**Decision (landed, `internal/compile/configmap.go`):** one key per file, the
path with `/` written as `__` (`asterisk/pjsip.conf` → `asterisk__pjsip.conf`),
UTF-8 files in `data`, others in `binaryData`, plus `bundle.json` (the
`compile.Index`: version, device index, AMI account). `TreeOf` now requires
portable names (letters, digits, `.`, `-`, `_`, never `__`, no leading or
trailing `_`), so the encoding is reversible. `ConfigMapData` refuses a bundle
over 1 MiB (`ErrBundleTooLarge`, counting keys and values as the API server
does); `BundleFromConfigMap` decodes and validates.

*Why:* `kubectl get configmap pbx-config -o yaml` stays readable file by file
(design §5: "the result is easy to inspect"); a tarball or JSON blob hides
it. Release A's bundle is tens of kilobytes. Release C's wallpapers and
ringtones are what could approach 1 MiB; they will need content-addressed
objects of their own then, which the index can point to.

### (e) The device bundle ID

**Decision (landed):** `drivers.DeviceID(kind, name)` = lower-case kind, `-`,
resource name: `phone-desk`, `softphone-chris-laptop`; over **30**
characters it is cut and suffixed with 8 hex digits of the name's SHA-256
(*review:* the plan stage capped it at 63, but the 79x5's `authName` holds
at most 30 characters, so a Phone named with more than 24 would have
rendered an auth username the phone cannot send; a Phone name of up to 24
characters and a Softphone name of up to 20 stay whole). It is
`DeviceObservations.Device`, `Device.ID`, the PJSIP **endpoint** name and the
**auth username**. The device's **number** is its SIP user and its **AOR**
name; endpoints are identified with `identify_by = auth_username` (requests
are challenged first, then matched by the Authorization username).

*Why:* the ID must survive renumbering and relabelling, so observations stay
attached; the kind prefix keeps a Phone and a Softphone with the same name
apart. Keeping the number as the SIP user keeps the 79x5's line `name` the
plain extension it expects in CUCM mode (a non-numeric line name is
unverified on that firmware) and gives Linphone the identity `sip:150@…`.
Consequence: renumbering renames the AOR (see §11).

### (f) Where the driver registry is wired

**Decision:** `internal/drivers/builtin.Registry() (*drivers.Registry, error)`
returns `cisco79x5.New()` and `softphone.New()`. `compile.Compile` takes the
registry as an argument (landed: `Compile(reg, in, s)`), so Compile's own
tests use fake drivers and the golden tests use `builtin`. The controller,
the agent (firmware manifests via `Registry.Drivers()`), and the CLI
(`models`, `firmware load`) each call `builtin.Registry()` once at start.

*Why not* `init()` self-registration: import side effects, and tests could not
choose drivers. *Why not* Compile importing `builtin`: compile tests would be
coupled to real drivers' output.

### Further decisions recorded with the contracts

- **PBX down vs. agent unreachable.** §7 and §8.3 disagree; Asterisk down is
  `Registered=False` `PBXDown`, agent unreachable or stale is
  `Registered=Unknown` `AgentUnreachable` (CLAUDE.md deviations).
- **Config liveness.** `Observations.CompiledVersion` +
  `AgentStatus.LastApplyVersion` → `ConfigLive()` and `ApplyFailure()`. A
  phone is never resynced before its new config is served (else it re-reads
  the old one and spends an attempt).
- **No resync mid-call** (`DeviceObservations.Calls`).
- **The running load** *(review, landed)*: `/DeviceInformationX` has both
  `appLoadID` (the application image, a file such as
  `jar45sip.9-4-2SR4-3.sbn`) and `versionID` (the load ID the SEP file's
  `loadInformation` names, such as `SIP45.9-4-2SR4-3S`). The plan stage
  compared `appLoadID` with the target load, which can never be equal, so a
  correctly upgraded phone would have looked like it ran the wrong load and
  been resynced until a human was asked. `agentapi.DeviceInformation` now
  carries `VersionID`, and the driver accepts either field (T1 *runsLoad*),
  because which one a 79x5 fills is a bench check (§8).
- **Requests** (`resync-requested`, `reissue-requested`) are marked handled
  by the first Decide after Unknown that sees them. A phone resync request
  sends a resync only in Registered/ConfigStale, not during a call and not
  before the compiled config is live (it stays pending until both hold); in
  any other row it is dropped and the
  controller emits Event `RequestIgnored` with the state (W3 infers this when
  `HandledRequest` advances and no action of that kind was returned).
- **Labels and names reject control characters** in the schema (landed;
  envtest pins it). Everything else is escaped by whoever renders it.
- **Two Render passes** give the 79x5 its `versionStamp`: Compile renders with
  `dev.ConfigVersion == ""`, hashes that into the device version, renders
  again with it set; the outputs differ only in the stamp.
- **Decision.Conditions:** unlisted condition types keep their value; every
  row lists all of the driver's types except Unknown (Ready and Registered
  only).
- **Agent secret access** *(recommended for W2/W3, see §7)*: one projected
  Secret, not a watch on every Secret.

---

## 3. Contracts landed (commit `f346cf7`)

Types, helpers and docs only, each pinned by a test; `make generate
check-generated fmt vet lint test` is green.

| Package | Added or changed |
|---|---|
| `internal/drivers` | `EndpointOptions`, `SIPTransport`, `KnownCodecs`, `Profile.Endpoint`, `ValidateProfile` (called by `NewRegistry`); `Provisioning.ResyncByNotify`; `PasswordPolicy.Validate`, `Alphanumeric`; `DeviceID`, `DeviceIDMaxLength`, `ModelSoftphone`, `RequestMaxLength`; `PhoneDevice`, `SoftphoneDevice`, `Memory.ToPhoneStatus`, `Memory.ToSoftphoneStatus`; `Device.NumberOutOfPlan`, `Device.ClaimMessage`; `ErrInvalidSpec`; `Observations.CompiledVersion`, `.AgentProblem`, `.ConfigLive`, `.ApplyFailure`; `Condition`; `DecideSettings.PBXAddress`; `RenderContext.DirectoryURL` (and `ProvisioningToken` removed: the token is in the URL, not the document); `SHA256Pending`, `FirmwareFile.Pending`, `FirmwareLoad.PendingFiles`, `.Missing`, `.Source`; `Registry.Drivers` |
| `internal/compile` | `Compile(reg, in, s)` (stub); `Bundle.Devices` (`BundleDevice`, replaces `DeviceVersions`), `Bundle.AMI` (`AMIAccount`, `AMIUsername`); `Validate` checks the index; `Settings.AMISecret` and JSON tags; `SecretRef` JSON tags; `DeviceSecretName`, `PasswordRef`, `ProvisioningTokenRef`, `SecretKeySIPPassword`, `SecretKeyProvisioningToken`; portable `TreeOf`; `DirectoryFile`; `SoundsDir`, `SoundNoEmergency`, `SoundNotAvailable`; `configmap.go` (`ConfigMapKey`, `PathFromConfigMapKey`, `IndexKey`, `Index`, `Bundle.ConfigMapData`, `BundleFromConfigMap`, `MaxConfigMapBytes`, `ErrBundleTooLarge`) |
| `internal/agent/api` | `AgentStatus.LastApplyVersion`; `DeviceObservations.Calls`; `Fetch.File` is the bundle path for provisioning documents (never the token); `PathFirmware`, `FirmwarePath`, `MaxFirmwareFileSize`, `FirmwareUploadResponse`; `ProvisioningPathPrefix` (`/p/`) |
| `api/v1alpha1` | control characters rejected in Phone/Softphone `label`, Person/Stream `name`; `ConditionLabelShortened`; `ReasonApplyPending`, `ReasonNumberNotInPlan` |
| *review, `8971df8`* | `drivers.DeviceIDMaxLength` 63 → **30** (the 79x5 `authName` limit); `agentapi.DeviceInformation.VersionID` added and `AppLoadID` documented as the application image, never a load name; `PhoneObserved.Firmware`'s example is `SCCP45.9-4-2SR4-3S` |

---

## 4. W1: the pure core (task level)

W1 runs as **four tasks**: T1, T2 and T3 in parallel (disjoint files), then
T4. Every task: TDD (test first), `nix develop -c make fmt vet lint test`
green before each commit, small commits, explicit `git add` paths.

Shared conventions for all W1 tests:

- `t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)`; every time in a
  test is `t0` plus or minus a duration. `metav1.Time` values are built with
  `metav1.NewTime(t0.Add(…))`.
- Fixture secrets are obviously fake (`not-a-secret-desk-0001`); gitleaks
  runs in `make check-hygiene`.
- Golden files: `testdata/<case>/…`, regenerated with `go test ./<pkg>
  -run Golden -update` (a package-level `flag.Bool("update", …)`). A diff in
  goldens is reviewed like code.

### T1. `internal/drivers/cisco79x5` (owner: W1-T1)

**Files** (all new): `doc.go`, `driver.go` (Driver, New, Name, Models,
Profile, Firmware), `profile.go` (profiles, reasons), `firmware.go` (the
manifest), `e2eload.go` + `e2eload_e2e.go` (`//go:build e2e`),
`timezone.go`, `label.go`, `dialtemplate.go`, `render.go`, `sep.go` (the SEP
document as Go structs or a `text/template` with an XML escaper),
`fold.go` (sensor → memory), `decide.go` (the rule table), and tests
`*_test.go` (external package `cisco79x5_test` unless noted),
`testdata/render/<case>/`.

**Exported API (exact):**

```go
package cisco79x5 // github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5

const Name = "cisco79x5"
const DefaultLoad = "SIP45.9-4-2SR4-3S"
const DialTemplateFile = "dialplan.xml" // bundle path tftp/dialplan.xml

type Driver struct{ /* unexported: loads []drivers.FirmwareLoad */ }

func New() *Driver
var _ drivers.Driver = (*Driver)(nil)

func (*Driver) Name() string                                   // "cisco79x5"
func (*Driver) Models() []string                               // cisco-7945, cisco-7965, cisco-7975
func (*Driver) Profile(model string) (drivers.Profile, bool)
func (*Driver) Render(dev drivers.Device, ctx drivers.RenderContext) (drivers.Files, error)
func (*Driver) Decide(dev drivers.Device, obs drivers.Observations, s drivers.DecideSettings, now time.Time) drivers.Decision
func (*Driver) Firmware() []drivers.FirmwareLoad

func ConfigFileName(mac string) string            // "SEP" + mac + ".cnf.xml" (mac already upper case)
func ScreenLabel(label string) string              // spaces removed, then first 12 runes
func LineLabel(label string) string                // first 30 runes
func WindowsTimeZone(iana string) (string, bool)
func DialTemplate(patterns []string) []byte        // the dialplan.xml document
func E2ELoadFile(name string) []byte               // deterministic bytes of an e2e test-load file

// Condition reasons (exported: they are status contract).
const (
    ReasonUnknownModel = "UnknownModel"; ReasonUnknownFirmware = "UnknownFirmware"
    ReasonUnseen = "Unseen"; ReasonNeedsFirmware = "NeedsFirmware"; ReasonNeedsTrustReset = "NeedsTrustReset"
    ReasonUpgrading = "Upgrading"; ReasonRegistering = "Registering"; ReasonConfigStale = "ConfigStale"
    ReasonRegistered = "Registered"; ReasonUnreachable = "Unreachable"
    ReasonStaged = "Staged"; ReasonNotStaged = "NotStaged"; ReasonManifestPending = "ManifestPending"
    ReasonRunning = "Running"; ReasonOtherLoad = "OtherLoad"; ReasonNotReported = "NotReported"
    ReasonFetched = "Fetched"; ReasonNotFetched = "NotFetched"; ReasonOldVersion = "OldVersion"
    ReasonContact = "Contact"; ReasonNoContact = "NoContact"; ReasonQualifyFailed = "QualifyFailed"
    ReasonCurrent = "Current"; ReasonStale = "Stale"
    ReasonNoConflict = "NoConflict"; ReasonAllSupported = "AllSupported"
    ReasonShortened = "Shortened"; ReasonAsWritten = "AsWritten"; ReasonInvalid = "Invalid"
)
```

`Models()` matches the CRD enum. A model the driver serves always has a
profile.

**Profiles** (Go data, `profile.go`):

- Hardware 7945: LineKeys 2, Softkeys 4, Display 320×240, ServiceArea
  298×156, Wallpaper 320×212, WallpaperThumbnail 80×53, RingtoneFormat "raw
  µ-law, 8 kHz, 8-bit mono, 240–16080 samples", Codecs `g722 ulaw alaw g729a
  ilbc l16`. 7965: LineKeys 6, otherwise as 7945. 7975: LineKeys 8,
  otherwise as 7945. Mark 7965/7975 hardware figures "unverified" in a doc
  comment (only the 7945 is in the house).
- Provisioning: ConfigFile `SEP{MAC}.cnf.xml`; DefaultFirmware
  `SIP45.9-4-2SR4-3S` for 7945 and 7965, **`SIP75.9-4-2SR4-3S` for 7975**
  (not in the manifest, so a 7975 is Invalid, reason UnknownFirmware, until
  its load is added: honest, not guessed); ScreenLabelMax 12; LineLabelMax
  30; Password `{12, drivers.Alphanumeric}`; **ResyncByNotify `true`**.
- Endpoint: §2(a) table.
- Features `pjsip`: Supported: hold, wideband, rfc4733DTMF, callerIDUpdate,
  distinctiveRing. Untested: transfer, pickup, meetMe, speedDial,
  serviceURL, wallpaper, customRingtones, xmlServices, pushNotification,
  rtpRx. Absent (Unsupported): blf, park, conference, dndSync,
  callForwardSync, intercom, secondLine.

**Manifest** (`firmware.go`): one load `SIP45.9-4-2SR4-3S`, Source "Cisco's
cmterm-7945_7965-sip.9-4-2SR4-3.zip", Models `cisco-7945`, `cisco-7965`,
the 8 files and sizes of §1.5, every digest `drivers.SHA256Pending`,
`term45.default.loads` and `term65.default.loads` `Shared: true`. A comment
above the list says exactly how to fill a digest in. `New()` appends any
`extraLoads` (set only by `e2eload_e2e.go`'s `init`).

**E2E load** (`e2eload_e2e.go`): load `E2E45.0-0-1S`, Source "patchbay e2e
test load (generated)", Models `cisco-7945`, `cisco-7965`, files
`E2E45.0-0-1S.loads` and `appsE2E.0-0-1.sbn`, digests and sizes computed from
`E2ELoadFile` (content: `"patchbay e2e firmware " + name + "\n"` repeated to
4096 bytes for `.sbn`, once for `.loads`). No Shared files.

**Render** — inputs `dev` (from `drivers.PhoneDevice` + claims), `ctx`. Files:
`tftp/SEP<MAC>.cnf.xml` and `tftp/` + `DialTemplateFile`. Errors:

- `ErrInvalidSpec`-wrapped: model not served; target load (`dev.Firmware` or
  the profile default) not in the manifest or not listing the model.
- plain errors: `WindowsTimeZone(ctx.Timezone)` fails; `dev.MAC` not 12
  upper-case hex; `ctx.SIPPassword` empty; a URL over 255 characters; a
  `ctx.PBXAddress` or `ctx.NTPServer` over 50 characters.

SEP document — required values. Element paths and value meanings below were
checked against https://usecallmanager.nz/sepmac-cnf-xml.html in review
(2026-10-15); the rest of the document follows that page's sample, element
by element, and each sample element this list leaves out (VPN, TVS, IPv6,
video, add-on modules, ...) is named in the golden test's comment with why:

| Element | Value |
|---|---|
| `fullConfig`, `deviceProtocol` | `true`, `SIP` |
| `devicePool/dateTimeSetting/timeZone` | `WindowsTimeZone(ctx.Timezone)` |
| `…/ntps/ntp/name`, `ntpMode` | `ctx.NTPServer` (≤ 50), `unicast` (the page's spelling, lower case) |
| `devicePool/callManagerGroup/members/member[@priority=0]/callManager/processNodeName` | `ctx.PBXAddress` (≤ 50) |
| `…/ports/sipPort` | `ctx.SIPPort` |
| `sipProfile/sipProxies/registerWithProxy` | `true` ("must be set to true") |
| `sipProfile/sipCallFeatures/remoteCcEnable` | `true` ("must be set to true") |
| `sipProfile/sipStack/remotePartyID` | `true` ("must be set to true") |
| `sipProfile/sipStack/timerRegisterExpires` | `profile.Endpoint.RegistrationExpiry` in whole seconds (`600`) |
| `sipProfile/dtmfAvtPayload`, `dtmfOutofBand` | `101`, `avt` |
| `sipProfile/phoneLabel` | `ScreenLabel(dev.Label)` |
| `sipProfile/dialTemplate` | `dialplan.xml` |
| `sipProfile/startMediaPort`, `stopMediaPort` | `16384`, `32766` |
| `sipProfile/natEnabled` | `false` |
| `sipLines/line[@button=1][@lineIndex=1]/featureID` | `9` |
| `…/featureLabel`, `displayName` | `LineLabel(dev.Label)` (≤ 30) |
| `…/proxy` | `USECALLMANAGER` |
| `…/port` | `ctx.SIPPort` |
| `…/name`, `contact` | `dev.Number` |
| `…/authName` | `dev.ID` (≤ 30: `drivers.DeviceIDMaxLength`) |
| `…/authPassword` | `ctx.SIPPassword` (the placeholder; the password is 12) |
| `loadInformation` | the target load name (no `.loads`) |
| `vendorConfig/pcPort` | `1` (1 = disabled) |
| `vendorConfig/webAccess` | `0` (0 = enabled) |
| `vendorConfig/sshAccess` | `1` (1 = disabled) |
| `vendorConfig/settingsAccess` | `2`, CUCM's "Restricted" (`settingsAccessRestricted` in sep.go). usecallmanager documents only `0` = disabled, `1` = enabled; §8 has the bench check and the fallback `1` |
| `versionStamp` | `dev.ConfigVersion[:32]`, empty when `ConfigVersion` is empty |
| `transportLayerProtocol` | `1` (TCP; the profile's Transport) |
| `advertiseG722Codec` | `1` (1 = enabled; 2 is "device default") |
| `directoryURL` | `ctx.DirectoryURL` (≤ 255) |
| `userLocale/name` / `networkLocale/name` | `English_United_States` / `United_States` |

All text is XML-escaped by an XML encoder (never string concatenation). The
document is UTF-8 with an XML declaration saying so. Whether the 79x5 shows
non-ASCII label characters correctly (its English locale is ISO-8859-1) is
a bench check (§8); the output is valid XML either way. Render returns a
plain error for any value over its documented limit that the caller could
have kept within it (a URL over 255, an address over 50); labels are cut by
`ScreenLabel`/`LineLabel`, never refused.

`WindowsTimeZone` table (at least): America/Los_Angeles → "Pacific
Standard/Daylight Time"; America/Denver → "Mountain Standard/Daylight Time";
America/Phoenix → "US Mountain Standard Time"; America/Chicago → "Central
Standard/Daylight Time"; America/New_York → "Eastern Standard/Daylight Time";
America/Anchorage → "Alaskan Standard/Daylight Time"; Pacific/Honolulu →
"Hawaiian Standard Time"; Europe/London → "GMT Standard/Daylight Time"; UTC
and Etc/UTC → "Greenwich Standard Time". Every one of these names is in the
time-zone list on usecallmanager's SEPMAC.cnf.xml page (checked in review);
a name added later must be too. Aliases (US/Pacific …) map like their
canonical zone.

`DialTemplate(patterns)` writes the format of
https://usecallmanager.nz/dial-template.html (checked in review,
2026-10-15): root element **`<dialTemplate>`**, then a `<versionStamp>`,
then one **`<TEMPLATE match="…" timeout="…"/>`** per pattern (the element
name must be upper case; the attributes are lower case: `match`, `timeout`,
and the unused `line`, `rewrite`, `tone`). In `match`, a digit or `#`
matches itself, `.` matches one digit, `#` or `*`, `*` matches **zero or
more** digits, `#` or `*`, and `\*` is a literal star; `timeout` is "the
number of seconds to wait for more digits if this pattern matches". The
conversion, in the order given (`NumberPlan.DialPatterns()` puts exact
numbers first):

- a fixed-length pattern → `match` with `X` → `.`, `timeout="0"`;
- a trailing `.` (Asterisk: one or more) → `match="<prefix>.*"` (one, then
  zero or more), `timeout="3"`;
- **a pattern containing `*` or `#` is omitted.** usecallmanager documents
  `\*` as a literal star, but if this firmware read `\*8` as the wildcard
  `*` then `8`, any number would be dialled the moment an 8 is pressed
  (`18` instead of `180`). Omitting `*8` costs only the catch-all's wait.
  The switch is `dialTemplateLiteralStar = false` (§8);
- last, the catch-all `<TEMPLATE match="*" timeout="5"/>`.

`versionStamp` is the first 32 hex digits of the SHA-256 of the template
lines, so the file depends only on the patterns and stays byte-identical
across devices. The page does not say whether the phone takes the first
matching template or the most specific one, so the generated template must
not depend on it (tested below).

**Fold** (`fold.go`): `fold(dev, obs, now, profile) drivers.Memory`, used by
every row except Unknown. `d := obs.Device`.

| Field | Rule |
|---|---|
| `FirstSeen` | if nil and `d` has a fetch or a live contact: the earliest fetch `At`, else `now` |
| `Address` | live contact's `Address`, else the latest fetch's `Source`, else unchanged |
| `Firmware` | from the latest successful scrape: `DeviceInformation.VersionID` when non-empty, else `AppLoadID` when non-empty, else unchanged (see *runsLoad*) |
| `LastConfigFetch` | the latest fetch with `File == ConfigFileName(MAC)`, `Result == served`, `Version != ""`, if newer than the remembered one |
| `LastRegistered` | if a live contact: set to `now` when nil, when `now − LastRegistered ≥ 1 min` (coarse, so status is not rewritten every poll), or when `fetchedAfter(LastRegistered)` (so a config fetch while registered never looks newer than the registration: rows 7 and 10 split on that order); else unchanged |
| `UpgradeStartedAt` | non-nil exactly while the Upgrading predicate holds (set by that row, cleared by every other row except Invalid/Unknown) |
| `LastAction`, `HandledRequest` | unchanged by the fold; rows change them |

Definitions used below: *live contact* = `d.Registration != nil &&
d.Registration.ExpiresAt.After(now) && !(profile.Endpoint.QualifyFrequency > 0
&& d.Registration.Qualify == unreachable)`. *Firmware fetch* = a served fetch
whose `File` ends in `.sbn` or `.loads`. *sgn fetch* = any fetch of
`ConfigFileName(MAC) + ".sgn"`. *target* = `dev.Firmware` or the profile
default; *load* = its manifest entry; *available* = `len(load.Missing(obs.Firmware)) == 0`.
*current* = `m.LastConfigFetch != nil && m.LastConfigFetch.Version == dev.ConfigVersion`.
*runsLoad(reported, load)* (unexported) = `reported == load.Name`, or
`reported` with any `.sbn` suffix removed equals the name, `.sbn` removed,
of one of the load's `.sbn` files (so `jar45sip.9-4-2SR4-3.sbn` and
`jar45sip.9-4-2SR4-3` both mean `SIP45.9-4-2SR4-3S`); the SCCP image
`jar45sccp.…` and `SCCP45.…` never match a SIP load.
*fwWrong* = `m.Firmware != "" && !runsLoad(m.Firmware, load)`.
*fwRequested* = some fetch of a file named in *load* (its `.loads`, `.sbn`
or Shared files) has `Result == notFound` and `At` at or after
`m.LastConfigFetch.At` (any, if nil): the phone asked for the target load
and it was not there. A phone fetches `<load>.loads` only when it runs
another load, and `term45.default.loads` only after a factory reset, so
this sees a wrong load even when the web page cannot be read (the Desk
phone's went quiet after one response, design §2). *fetchedAfter(t)* =
`m.LastConfigFetch != nil && m.LastConfigFetch.At.After(t)`.
*lastFw* = time of the latest firmware fetch (zero if none). *upgradeRef* =
the later of *lastFw* and `m.UpgradeStartedAt`, so an agent restart that
empties the fetch log mid-upgrade does not end the upgrade on a config
fetch from before it.

**Standard conditions** (rows 3–10; fixed order: Ready, FirmwareAvailable,
FirmwareCurrent, Provisioned, Registered, ConfigCurrent, NumberConflict,
MACConflict, Unsupported, LabelShortened):

| Condition | True | False / Unknown |
|---|---|---|
| FirmwareAvailable | `Staged` | False `ManifestPending` (msg: pending files) if any pending, else False `NotStaged` (msg: `missing: a, b`) |
| FirmwareCurrent | `Running` (msg: target load) when `runsLoad(m.Firmware, load)` | Unknown `NotReported` when `m.Firmware == ""`; False `OtherLoad` (msg `running X, target Y`) |
| Provisioned | `Fetched` | False `NotFetched` (no fetch) / `OldVersion` |
| Registered | `Contact` (msg: contact, User-Agent) | False `NoContact` / `QualifyFailed` |
| ConfigCurrent | `Current` (current and `ConfigLive()`) | False `ApplyFailed` (msg: `ApplyFailure()`) / `ApplyPending` / `Stale` (msg: versions, resync count, "waiting for the call to end") / `NotFetched` |
| NumberConflict, MACConflict | — | False `NoConflict` |
| Unsupported | — | False `AllSupported` |
| LabelShortened | `Shortened` (msg: `shows "<screen>" on screen and "<line>" on its line`) when `ScreenLabel(l) != l` or `LineLabel(l) != l` | False `AsWritten` |

**Rule table** (`decide.go`, a `[]rule{state, match, outcome}` slice, first
match wins; `outcome` may branch, and each branch is a test case):

| # | State | Predicate | Outcome |
|---|---|---|---|
| 1 | `Invalid` | model not in `Models()` → `UnknownModel`; `dev.MACConflict`; `dev.NumberConflict`; `dev.NumberOutOfPlan` → `NumberNotInPlan`; target not in the manifest for the model → `UnknownFirmware` (checked in this order; the first gives Ready's reason) | Ready False `<reason>`, message = `dev.ClaimMessage` or text P1; NumberConflict / MACConflict True or False; FirmwareAvailable, FirmwareCurrent, Provisioned, Registered, ConfigCurrent **Unknown `Invalid`**; Unsupported False; LabelShortened standard. No actions. HumanAction = Ready's message. A pending request is marked handled. Memory = fold. Requeue 0. |
| 2 | `Unknown` | `obs.AgentProblem(now, s.StaleAfter) != ""` | State = `dev.LastState`, or `Unknown` if empty. Conditions **only** Ready (Unknown `AgentUnreachable` / False `PBXDown`) and Registered (same status and reason). No actions; memory = `dev.Memory` unchanged; request untouched; HumanAction ""; requeue 0. |
| 3 | `Unseen` | after the fold, `FirstSeen == nil` | Ready False `Unseen`; standard conditions. HumanAction P2. Requeue 10 min. |
| 4 | `NeedsFirmware` | (`fwWrong` or `fwRequested`) and `!available` | Ready False `NeedsFirmware`. HumanAction P4 when the target load has pending digests, else P3. Requeue 0. |
| 5 | `NeedsTrustReset` | no live contact, and either (a) *sgnFirst* (the earliest sgn fetch after `m.LastConfigFetch.At`, or ever) exists and `now − sgnFirst ≥ 3 min`, or (b) a `d.Web.DeviceLog` line matches `trustErrorPattern` and not `loadAuthPattern` (below) | Ready False `NeedsTrustReset`. HumanAction P5 when the **default** load (the profile's) is available, else P6. Requeue 0. |
| 6 | `Upgrading` | `now − lastFw < 3 min`, or (`m.UpgradeStartedAt != nil` and no live contact and `!fetchedAfter(upgradeRef)`) | Set `UpgradeStartedAt` if nil to the earliest firmware fetch not older than 20 min (else `now`). Ready False `Upgrading`. HumanAction P8 once `now − UpgradeStartedAt ≥ 20 min`, else P7. **No actions ever** (a request is dropped). Requeue = the nearer of the 20-min mark and `lastFw + 3 min`, of those still ahead. |
| 7 | `Registering` | no live contact and (`m.LastRegistered == nil` or `fetchedAfter(*m.LastRegistered)`) | *start* = `m.LastConfigFetch.At`, else `m.FirstSeen`. Ready False `Registering`. Before 5 min: HumanAction "", requeue per the requeue rule. From 5 min, the first that applies: never fetched its config → P11; `fwWrong` → P10; Admin VLAN set → P9; else P12 (with its two variants). |
| 8 | `ConfigStale` | live contact and (`!current` or `fwWrong`) | Branches, in order: **(i)** `!obs.ConfigLive()` → no action; ConfigCurrent False `ApplyFailed` (requeue 0) or `ApplyPending` (requeue 15 s). **(ii)** `d.Calls > 0` → no action, request stays pending, ConfigCurrent False `Stale` "waiting for the call to end", requeue 30 s. **(iii)** a pending request → action `resync` (Version = `dev.ConfigVersion`), `HandledRequest = Request`, `LastAction = {resync, V, now, A}` where `A` is `LastAction.Attempts` if `LastAction` is a resync for V, else 0 (a requested resync is not counted). **(iv)** `!profile.Provisioning.ResyncByNotify` → HumanAction P13, no action. **(v)** `LastAction` is a resync for V: `now − At < 5 min` → wait, requeue the rest (any `Attempts`); else `Attempts ≥ 2` → P13, no action; else `Attempts == 1` → `resync`, `Attempts = 2`; else (`Attempts == 0`: only requested resyncs so far) fall through to (vi). **(vi)** otherwise → `resync`, `LastAction = {resync, V, now, 1}`, requeue 5 min. Ready False `ConfigStale` in every branch. |
| 9 | `Registered` | live contact (and so `current && !fwWrong`) | Ready True `Registered`; standard conditions (FirmwareCurrent may be Unknown `NotReported`). A pending request with no call and `obs.ConfigLive()` → `resync` (as 8.iii); otherwise it stays pending (a call, or a new bundle not yet live). Requeue 10 min. |
| 10 | `Unreachable` | no live contact, `m.LastRegistered != nil`, `!fetchedAfter(*m.LastRegistered)` | Ready False `Unreachable`; Registered False `NoContact`/`QualifyFailed`. HumanAction P14 once `now − LastRegistered ≥ 10 min`; before, "" and requeue the rest. |

Rows 7–10 partition every seen device (no contact: 7 or 10; contact: 8 or
9), so the table is total; a defensive final branch returns `Unknown` with
message "no rule matched" and a test proves it unreachable. **Requeue rule:**
RequeueAfter is the smallest positive time until any timer above changes the
result (sgnFirst+3m, lastFw+3m, UpgradeStartedAt+20m, start+5m,
LastAction.At+5m, LastRegistered+10m), capped by the row's own value (10 min
for Unseen/Registered), 0 when none.

Patterns (row 5), case-insensitive Go regexps, bench-refinable:

```text
trustErrorPattern  (?i)(trust ?list|\bITL\b|\bCTL\b|(config|cnf).*(verif|signat))
loadAuthPattern    (?i)load auth     (firmware "Load Authentication Failed" is not a trust problem)
```

**Human-action texts, verbatim** (`<…>` are substitutions; `<files>` is
comma-and-space separated in manifest order; `<N>` is whole minutes; `|`
in P12 is a literal separator):

```text
P1  no firmware load <target> for <model> in the cisco79x5 manifest
P2  connect it to a VLAN 20 switch port; it will request SEP<MAC>.cnf.xml from <PBXAddress>
P3  run `patchbay firmware load <zip>` with <Source>; missing: <files>
P4  the cisco79x5 firmware manifest has no SHA-256 yet for <files>: put each file's sha256sum from <Source> into internal/drivers/cisco79x5/firmware.go, release patchbay with it, then run `patchbay firmware load <zip>`
P5  factory reset: hold # while powering on, then 123456789*0#
P6  stage firmware first: run `patchbay firmware load <zip>` with <Source>; a factory reset with nothing to download leaves the phone stuck
P7  upgrading: do not unplug
P8  upgrade stuck: check the phone's screen
P9  its Admin. VLAN ID is <v> but its switch port is untagged; clear it with a factory reset (its settings menu is restricted): hold # while powering on, then 123456789*0#
    (variant when the default load is not available: "…untagged; stage firmware first with `patchbay firmware load <zip>` (<Source>), then factory-reset it: hold # while powering on, then 123456789*0#")
P10 it runs <running>, not <target>: power-cycle it so it fetches its config and upgrades
P11 it has not fetched SEP<MAC>.cnf.xml from <PBXAddress>: check its TFTP server (DHCP option 66)
P12 fetched its config <N> min ago but has not registered; its log says: <line> | <line> | <line>
    (variants: "…; its web page could not be read: <error>" and "…; its web page has not been read yet")
P13 press Settings then **#**, or power-cycle it
P14 not responding; power-cycle it
S1  scan `patchbay softphone qr <name>` in Linphone
S2  its settings changed: scan `patchbay softphone qr <name>` in Linphone again
S3  Linphone fetched its settings <N> min ago but has not registered: is it on the LAN?
```

P2, P5, P7, P8, P13 and P14 are the design's words (§8.3); P3 and P6 extend
its "run `patchbay firmware load <zip>`" and "stage firmware first" with what
to load. *Review:* P9 used to say "clear its Admin. VLAN ID (Settings >
Network Configuration)", which the phone's own config forbids once it has
fetched it: `settingsAccess` is restricted (design §4), and a restricted
menu shows only the user preferences. A factory reset clears the network
settings, and P9 now uses the same reset texts as P5/P6. P9 is also asked
after P11 and P10 rather than first: a phone that fetched its config over
the network has a working VLAN, so the Admin VLAN is the least likely
reason it has not registered.

**Tests (T1):**

- `TestProfiles`: every model has a profile that passes
  `drivers.ValidateProfile`; the 7945 values above, field by field; the
  Features table.
- `TestManifest`: `ValidateFirmware` passes; the Cisco load has 8 files with
  the §1.5 sizes, every digest pending, `term45`/`term65` Shared.
  `TestManifestHasNoE2ELoad` lives in a file constrained `//go:build !e2e`
  (an unconstrained one would fail W1's own `go test -tags e2e` run), and
  its twin under `//go:build e2e` checks the test load is present with
  digests and sizes matching `E2ELoadFile`.
- `TestScreenLabel` / `TestLineLabel`: `Desk` → `Desk`; `Front Door` →
  `FrontDoor` (changed); 12, 13 and 31 characters at the boundaries;
  multi-byte runes (`Küche Oben` → `KücheOben`, never a split rune).
- `TestWindowsTimeZone`: every row of the table; aliases; unknown →
  `("", false)`.
- `TestDialTemplate` (golden; the root is `dialTemplate`, every element
  `TEMPLATE` with lower-case `match` and `timeout`, parsed back with
  `encoding/xml`) and `TestDialTemplateMatching`: a small simulator of the
  documented matching (`.` one of `0-9#*`, `*` zero or more, `\*` literal;
  after each digit, a template whose `match` covers all the digits dialled
  is a full match, and a full match with `timeout="0"` dials at once). It
  evaluates every case under **both** readings of the undocumented choice,
  first full match in file order and most specific full match (fewest
  wildcards), and fails if they differ. It asserts `100`, `150`, `301`,
  `600`, `911`, `705`, `0` dial at once with exactly those digits, no
  shorter prefix of any of them dials at once, `912` and `9206555` wait on
  `timeout="3"`, `*8` is not in the template, and `911` is never cut short
  by the `9.` pattern.
- `TestRenderGolden` cases: `desk` (house values), `lab-7965` (term65, line
  keys irrelevant), `hostile-label` (`R&D; "Lab" <1> Küche`), `long-label`
  (`Upstairs Office Phone` → 12/30), `new-york` (time zone), `explicit-firmware`
  (`spec.firmware` = default), and errors: unknown model, unknown load,
  7975 (default not in manifest) → `errors.Is(err, drivers.ErrInvalidSpec)`;
  unknown time zone, lower-case MAC, empty password placeholder, a 256-character
  directory URL, a 51-character PBX address → plain
  errors.
- `TestRenderProperties`: every output parses with `encoding/xml`; only paths
  under `tftp/`; the two-pass property (`ConfigVersion` empty vs set differ
  only inside `<versionStamp>`); dialplan.xml is identical for any two
  devices with the same ctx; no character of the password placeholder is
  escaped; every element with a documented limit is within it, for every
  golden device and for a Phone whose name is 253 characters (`authName`,
  `name`, `displayName`, `featureLabel` ≤ 30; `phoneLabel` ≤ 12 runes;
  `processNodeName` ≤ 50); `timerRegisterExpires` equals the profile's
  expiry in seconds.
- `TestFold`: one case per table row above, including `LastRegistered` at 59 s
  (unchanged) and 60 s (advanced), and at 10 s with a config fetch after it
  (advanced); `ExpiresAt == now` is not live, `now+1s` is; qualify
  unreachable counts only when `QualifyFrequency > 0`; `Firmware` from
  `VersionID` when both fields are set, from `AppLoadID` when only it is,
  unchanged by a failed scrape. `TestRunsLoad`: `SIP45.9-4-2SR4-3S`,
  `jar45sip.9-4-2SR4-3.sbn` and `jar45sip.9-4-2SR4-3` run the default load;
  `SCCP45.9-4-2SR4-3S`, `jar45sccp.9-4-2SR4-3.sbn`, another load's image
  (`jar45sip.9-4-2ES26.sbn`), `term45.default.loads` and `""` do not.
- `TestDecide/<row>/<case>` — table tests, each asserting the **whole**
  Decision (State, Conditions in order, Actions, StatusPatch, HumanAction,
  RequeueAfter). Required cases and boundaries:

  | Row | Cases (boundaries are just before / at / after) |
  |---|---|
  | 1 Invalid | each reason alone; two reasons at once (order); a MAC loser with `obs.Device == nil`; a pending request → handled, no action; 7975 |
  | 2 Unknown | `PolledAt = now − StaleAfter` (not Unknown) / `− StaleAfter − 1ns` (Unknown); `AgentReachable=false`; `AsteriskUp=false` → PBXDown with Ready False; `LastState == ""` → `Unknown`; memory and request untouched |
  | 3 Unseen | no observations; observations with only another device's MAC (nil); `FirstSeen` set but no observations (agent restarted) → not Unseen; human text with MAC and address |
  | 4 NeedsFirmware | SCCP load reported as `SCCP45.9-4-2SR4-3S` and as `jar45sccp.9-4-2SR4-3.sbn`, store empty (lists all 8 files); web page never read but a `notFound` fetch of `SIP45.9-4-2SR4-3S.loads` after the last config fetch (match), the same fetch before it (no match); a `notFound` `term45.default.loads` (match); manifest pending (pending text); partially staged (lists the rest); available → not this row; the phone reports `jar45sip.9-4-2SR4-3.sbn` (the target: not this row) |
  | 5 NeedsTrustReset | sgnFirst at `now−3m+1s` (→ row 7, requeue 1 s), `now−3m`, `now−3m−1s`; served SEP after the sgn → not; contact → not; trust log line → at once; "Load Authentication Failed" → not; default load available vs not (two texts) |
  | 6 Upgrading | lastFw at `now−3m+1s` (match), `now−3m` with `UpgradeStartedAt` already set and no fetch after (still match via in-progress), `now−3m` with `UpgradeStartedAt` nil (→ row 7), served SEP after lastFw → row 7; UpgradeStartedAt at `now−20m+1s` ("do not unplug", requeue 1 s), `now−20m`, `now−20m−1s` ("stuck"); first sight sets UpgradeStartedAt; a request is dropped; row 7 clears UpgradeStartedAt; agent restarted mid-upgrade (`UpgradeStartedAt` set, no fetches, a config fetch from before it) → still Upgrading |
  | 7 Registering | start at `now−5m+1s` (requeue 1 s), `now−5m`, `now−5m−1s`; each human variant in order (never fetched, wrong firmware, Admin VLAN with the default load available and not, log lines, web error, not scraped; and the first applies when several do); `LastRegistered` before the last fetch (rebooted) |
  | 8 ConfigStale | (i) pending (15 s) and failed (message is the agent's error, failure about an older version ignored); (ii) `Calls=1`, also with a pending request (stays pending); (iii) request (attempts not incremented, HandledRequest set); (iv) `ResyncByNotify=false`; (v) attempt 1 at `now−5m+1s` (wait 1 s) / `now−5m` (resync, attempts 2); attempt 2 at `now−5m+1s` (wait) / `now−5m` (human); an older version's resync → fresh attempt 1; a requested resync of V (`Attempts` 0) at `now−5m+1s` (wait) / `now−5m` (resync, `Attempts` 1); (vi) first resync; config current but `fwWrong` with firmware available → resync |
  | 9 Registered | plain; firmware not reported; LabelShortened True; pending request → resync; pending request during a call, or while `!ConfigLive()`, → stays pending; a correctly upgraded phone reporting only `AppLoadID = jar45sip.9-4-2SR4-3.sbn` → Registered, FirmwareCurrent True |
  | 10 Unreachable | `LastRegistered` at `now−10m+1s` (requeue 1 s), `now−10m`, `now−10m−1s`; contact lost via qualify |

- `TestDecideTotal`: a generated grid (agent health × observations ×
  memory × timing, ≥ 10 000 combinations, seeded) never reaches the
  defensive branch, always lists Ready, never returns an action from rows
  1–7 or 10, and never `resync` while `Calls > 0` or `!ConfigLive()`.
- `TestRenderDecideAgree`: for every Render test device, `errors.Is(err,
  drivers.ErrInvalidSpec)` ⇔ Decide's state is `Invalid` (no conflicts set).

### T2. `internal/drivers/softphone` (owner: W1-T2)

**Files:** `doc.go`, `driver.go`, `profile.go`, `render.go`, `fold.go`,
`decide.go`, tests, `testdata/render/<case>/`.

```go
package softphone // github.com/ChristopherJMiller/patchbay/internal/drivers/softphone

const Name = "softphone"
type Driver struct{}
func New() *Driver
var _ drivers.Driver = (*Driver)(nil)
// Name() "softphone"; Models() []string{drivers.ModelSoftphone}; Firmware() nil.
func ProvisioningFile(id string) string // "provisioning/" + id + ".xml"

const (
    ReasonUnknownModel = "UnknownModel"; ReasonIssued = "Issued"; ReasonProvisioned = "Provisioned"
    ReasonRegistered = "Registered"; ReasonFetched = "Fetched"; ReasonNotFetched = "NotFetched"
    ReasonOldVersion = "OldVersion"; ReasonContact = "Contact"; ReasonNoContact = "NoContact"
    ReasonCurrent = "Current"; ReasonStale = "Stale"; ReasonNoConflict = "NoConflict"; ReasonInvalid = "Invalid"
)
```

Profile: Model `softphone`; Hardware Codecs `g722 ulaw alaw`, others zero;
Provisioning ConfigFile `provisioning/{ID}.xml`, DefaultFirmware "",
ScreenLabelMax 0, LineLabelMax 64, Password `{24, Alphanumeric}`,
ResyncByNotify false; Endpoint per §2(a); Features `pjsip`: hold, transfer,
wideband, rfc4733DTMF, callerIDUpdate Supported.

**Render:** `provisioning/<ID>.xml`, a Linphone remote-provisioning document
(`<config xmlns="http://www.linphone.org/xsds/lpconfig.xsd">`, sections and
entries with `overwrite="true"`; verify names against Linphone's remote
provisioning documentation and liblinphone's lpconfig keys):

- `sip/default_proxy` = 0;
- `proxy_0`: `reg_proxy` = `<sip:<PBXAddress>:<SIPPort>;transport=udp>`,
  `reg_identity` = `<sip:<Number>@<PBXAddress>>` (**no display name**:
  what others see is the endpoint's `callerid`, which Compile sets from the
  label, so a label change must not change this document; it would make
  the softphone stale and ask its owner to scan a QR code again for a
  cosmetic edit), `reg_expires` = the profile's `RegistrationExpiry` in
  seconds (600), `reg_sendregister` = 1, `publish` = 0;
- `auth_info_0`: `username` = Number, `userid` = ID, `passwd` = the
  placeholder, `domain` = PBXAddress. *Review:* liblinphone's published
  key reference (download.linphone.org, "Provisioning configuration keys")
  lists `username`, `passwd`, `realm`, `domain`, `ha1` and `algorithm` for
  `auth_info_N`, but not `userid`, and nothing in `sip`, `misc` or `video`
  (it says it is incomplete). T2 confirms each of those key names in
  liblinphone's source (the code that reads an `[auth_info_N]` section,
  and `misc/config-uri`) and cites file and line in the test comment.
  `userid` is load-bearing: without it Linphone authenticates as `150`,
  which identifies no endpoint, and never registers. The e2e fake reads the
  same document, so only the `[real]` Linphone check (§9) proves the names;
- `video`: capture, display, automatically_initiate, automatically_accept
  all 0;
- **`misc/config-uri` = "" (overwrite)**, so Linphone does not re-fetch the
  spent URL at every start (Review Focus 4).

No token in the document. Errors: `ErrInvalidSpec` when `dev.Model !=
ModelSoftphone`; plain errors for an empty placeholder or address.

**Fold:** as T1's, with *config fetch* = a served fetch whose `File ==
ProvisioningFile(dev.ID)`; no firmware fields. One rule more: when
`m.LastConfigFetch` is nil and `m.LastAction` is a `rotateToken`, only a
fetch **after** `LastAction.At` may become `LastConfigFetch`. S3 clears
`LastConfigFetch` to send a reissued softphone back to Issued, but the old
fetch is still in the agent's ring buffer; without this rule the next fold
would restore it and the softphone would leave Issued one poll later.

**Rule table** (conditions in order: Ready, Provisioned, Registered,
ConfigCurrent, NumberConflict):

| # | State | Predicate | Outcome |
|---|---|---|---|
| S1 | `Invalid` | model ≠ softphone (`UnknownModel`), `NumberConflict`, `NumberOutOfPlan` (`NumberNotInPlan`) | as phone row 1 (Provisioned/Registered/ConfigCurrent Unknown `Invalid`); a pending request is handled with no action |
| S2 | `Unknown` | `AgentProblem` | as phone row 2 |
| S3 | `Issued` | a pending reissue request | action `rotateToken`; `HandledRequest = Request`; `LastAction = {rotateToken, "", now, 1}`; `LastConfigFetch = nil` (back to Issued); Ready False `Issued`; HumanAction S1 |
| S4 | `Provisioned` | a served provisioning fetch with `At` after the last `rotateToken` (or any, if none): the token is spent | action `rotateToken`, `LastAction = {rotateToken, "", now, 1}`; Ready False `Provisioned` (Registered per contact) |
| S5 | `Issued` | `!current` | Ready False `Issued`; Registered per contact; HumanAction S1, or S2 when it had fetched an older version |
| S6 | `Provisioned` | current, no live contact, `LastRegistered` nil or before `LastConfigFetch.At` | Ready False `Provisioned`; from 5 min after the fetch HumanAction S3 *(an addition to §8.4)*; before, requeue the rest |
| S7 | `Registered` | current and a live contact | Ready True `Registered`; requeue 10 min |
| S8 | `Offline` | current, no live contact, `LastRegistered` after `LastConfigFetch.At` | **Ready True `Offline`**, Registered False `Offline` (`v1alpha1.ReasonOffline`); HumanAction "" |

**Tests (T2):** `TestProfile`; `TestRenderGolden` (`chris-laptop`,
`hostile-label`, `long-name` ID) and properties (XML parses; only
`provisioning/`; `config-uri` cleared; placeholder present once; no `@@`
other than it; two devices that differ only in label render identical
documents); `TestDecide/<row>/<case>` with: S3 while registered (→
Issued, Registered True); S3 then the next Decide with the pre-reissue
fetch still in the observations (stays Issued); S4 fetch `At == LastAction.At` (no rotate) and
`+1ns` (rotate); no prior rotation; S5 with contact and a changed password
(stale); S6 at `5m−1s`/`5m`/`5m+1s`; S6 vs S8 with `LastRegistered ==
LastConfigFetch.At` (S6) and `+1s` (S8); S7; S2 boundaries as T1;
`TestDecideTotal`.

### T3. `compile.Compile` and helpers (owner: W1-T3)

**Files:** `internal/compile/claims.go`, `plan.go` (NumberPlan.Validate,
DialPatterns, MatchPattern), `settings.go` (Settings.Validate), `version.go`,
`asterisk.go` (each Asterisk file), `dialplan.go` (extensions.conf),
`directory.go`, `compile.go` (Compile body; the stub and its test go), tests
`claims_test.go`, `plan_test.go`, `settings_test.go`, `version_test.go`,
`compile_test.go` (fake drivers), `asterisk_test.go`.

```go
// claims.go
type ResourceKind string
const (
    ResourcePhone ResourceKind = "Phone"; ResourceSoftphone ResourceKind = "Softphone"
    ResourcePerson ResourceKind = "Person"; ResourceStream ResourceKind = "Stream"
)
type ResourceRef struct{ Kind ResourceKind; Name string }
type ClaimProblem struct {
    NumberConflict, MACConflict, NumberOutOfPlan bool
    Message string // for people: who holds it, or which range it should be in
}
type Claims struct {
    Holders  map[string]ResourceRef       // routed number -> the resource that holds it
    Problems map[ResourceRef]ClaimProblem // every resource that is not routed, and why
}
func ResolveClaims(in Inputs, plan NumberPlan) Claims
func (c Claims) Apply(dev *drivers.Device) // sets NumberConflict, MACConflict, NumberOutOfPlan, ClaimMessage

// plan.go
func (p NumberPlan) Validate() error
func (p NumberPlan) DialPatterns() []string // EchoTest, Emergency, Devices, People, Streams, Reserved...
func MatchPattern(pattern, number string) bool

// settings.go
func (s Settings) Validate() error

// compile.go
var ErrMissingSecret = errors.New("compile: a referenced secret has no value")
const (
    DevicesContext     = "patchbay-devices"
    UnavailableContext = "patchbay-unavailable"
)
func Compile(reg *drivers.Registry, in Inputs, s Settings) (Bundle, error)
```

**ResolveClaims:** MACs first (Phones only, normalised; oldest by
`creationTimestamp`, ties by name; losers get `MACConflict`, message `MAC
<mac> is Phone <holder>'s, which is older`); then the plan (each kind's
range: Phone/Softphone `Devices`, Person `People`, Stream `Streams`; an
out-of-range number → `NumberOutOfPlan`, message `number <n> is not in the
<kind> range <pattern>`); then numbers among the rest (oldest wins across
kinds; ties by kind order Phone, Softphone, Person, Stream, then name; message
`number <n> is held by <Kind> <name>, which is older`). Losers and
out-of-plan resources hold nothing.

**Settings.Validate:** PBXAddress and NTPServer are IPv4 literals; ports in
1–65535; RTPPorts First ≤ Last; Timezone non-empty; Signalling `pjsip`;
NumberPlan valid; AllowedNetworks non-empty CIDRs; AMISecret set;
StreamBridge empty or host:port. **NumberPlan.Validate:** each pattern is
`[0-9*#X]+` with an optional trailing `.`; EchoTest and Emergency are plain
digits; the **ranges** (Devices, People, Streams, each Reserved) are pairwise
disjoint; EchoTest and Emergency lie in no Devices/People/Streams range and
differ from each other. They **may** lie in a Reserved pattern (the default
plan's `911` is inside `9.`): built-ins are exact extensions, which Asterisk
prefers to any pattern, and they come first in the dial template.

**Compile steps:**

1. `s.Validate()`; fail fast.
2. `claims := ResolveClaims(in, s.NumberPlan)`.
3. Devices in name order: `drivers.PhoneDevice` / `SoftphoneDevice`,
   `claims.Apply`, driver by `reg.ForModel` (none → index entry, not
   rendered; Decide says UnknownModel). Two devices with one ID → error.
4. For each device without a claim problem: ctx (below); `Render` with
   `ConfigVersion == ""`; `ErrInvalidSpec` → not rendered; other error →
   fail. Every path must be in the device kind's tree (Phone `tftp/`,
   Softphone `provisioning/`) → else fail. Device version = §4.T3 "version"
   over these files and the secrets they reference (missing value →
   `ErrMissingSecret`). Render again with the version; same paths required.
   Merge files: one path from two devices must be byte-identical (the shared
   dialplan.xml) → else fail.
5. Asterisk files (`asterisk.go`), dialplan, directory (below).
6. Index: every Phone and Softphone (rendered or not), `Number` only when
   routed, `MAC` unless a MAC loser, `ConfigFile`, sorted `Files`,
   `ProvisioningToken` for rendered softphones; AMI = `{AMIUsername,
   s.AMISecret}`.
7. Bundle version; `b.Validate()`; return.

RenderContext: `PBXAddress`, `SIPPort`, `DeviceHTTPURL =
http://<PBXAddress>:<DeviceHTTPPort>`, `DirectoryURL = DeviceHTTPURL +
"/directory.xml"`, `NTPServer`, `Timezone`, `Signalling`, `DialPatterns =
s.NumberPlan.DialPatterns()`, `SIPPassword = PasswordRef(dev.ID).Placeholder()`.

**Versions** (`version.go`): SHA-256 over a framed stream, lower-case hex.
Frames: `"file\x00" + path + "\x00" + uint64be(len) + content` per file in
path order; for the bundle, then `"index\x00" + uint64be(len) + json(Index
without Version)`; then per referenced SecretRef sorted by (Name, Key):
`"secret\x00" + name + "\x00" + key + "\x00" + hex(sha256(value))`.
Referenced = every placeholder in the files, plus (bundle only) index tokens
and the AMI secret. A device version uses only its first-pass files and the
secrets they reference.

**Asterisk files** (all under `asterisk/`; each starts with a one-line
"generated by patchbay; do not edit" comment):

- `asterisk.conf`: `[directories]` as the static config, with `astetcdir =>
  compile.AsteriskConfigDir`; `[options]` documentation_language en_US,
  hideconnect yes.
- `modules.conf`: `autoload = no`; the static config's list
  (`charts/patchbay/templates/pbx-static-config.yaml`, which W2 retires) plus
  `res_pjsip_refer.so` (transfer) and `app_playtones.so` (SIT tone).
  `res_pjsip_endpoint_identifier_user.so` (it implements `auth_username`)
  is already in that list. Nothing for BLF or presence: unsupported. W2
  asserts every listed module loads.
- `logger.conf`, `cdr.conf` (enable no), `cel.conf` (enable no), `http.conf`
  (enabled no), `rtp.conf` (`rtpstart`/`rtpend` from RTPPorts),
  `indications.conf` (`country = us` with the US tones), `pjsip_notify.conf`
  (`cisco-restart`, `cisco-reset` stanzas for manual use; the module needs the
  file).
- `manager.conf`: `[general]` enabled yes, bindaddr 127.0.0.1, port 5038,
  displayconnects no; `[patchbay-agent]` `secret = <AMI placeholder>`,
  `deny = 0.0.0.0/0.0.0.0`, `permit = 127.0.0.1/255.255.255.255`, `read =
  system,reporting`, `write = system,reporting` (W2 confirms against
  Asterisk 22; never `all`, `command`, `originate`). *As built (W1
  review):* the classes do not contain a client that holds the secret.
  `system`, which `PJSIPNotify` and `PJSIPShowContacts` need, also grants
  `ModuleLoad` of anything in astmoddir and `DialplanExtensionAdd`
  (Asterisk 22.11, "system,all"), and the pinned image ships
  `app_system.so`, `func_shell.so` and `res_agi.so`: with the secret, a
  client can add an extension that runs `System()`. The AMI secret is the
  barrier (only the agent holds it, §5.1), and W2.10 narrows the module
  directory (§5).
- `pjsip.conf`: `[global]` `user_agent = patchbay`, `default_realm =
  patchbay`, `endpoint_identifier_order = auth_username`; `[transport-udp]`
  and `[transport-tcp]` bound to `<PBXAddress>:<SIPPort>`; `[patchbay-acl]`
  `type = acl`, `deny = 0.0.0.0/0.0.0.0`, one `permit` per allowed network;
  per routed, rendered device: `[<ID>] type = endpoint` with the compiler's
  options (§2a) and the profile's, `callerid = "<name>" <<number>>`; `[<ID>]
  type = auth`; `[<number>] type = aor` with `max_contacts = 1`,
  `remove_existing = yes`, `default_expiration` and `maximum_expiration` =
  the profile's `RegistrationExpiry` in seconds, and `qualify_frequency`
  from the profile. Every section carries its `type =` line (`[global]`
  too). Checked in review against the Asterisk 22 res_pjsip reference:
  `identify_by = auth_username` matches "the username and realm in the
  Authentication header", which "is not present on the first message of a
  dialog", so a device is identified only after the challenge; the default
  `endpoint_identifier_order` is `ip,username,anonymous`, which is why the
  compiled `[global]` names `auth_username`; `default_realm` (default
  `asterisk`) is the challenge's realm; `force_rport` defaults to **yes**,
  so the 79x5's `no` must be written out (render every boolean option
  explicitly, never rely on a default).
- **Escaping** (one function, tested): in Asterisk values `;` → `\;`; in a
  caller ID name `"` → `'` and `\` → `/` (Asterisk's caller ID parser has no
  quote escape); caller ID name = the label cut to the profile's
  `LineLabelMax`. Labels never appear in extensions.conf.

**Dialplan** (`extensions.conf`): `[general]` static yes, writeprotect yes.
`[patchbay-devices]`: per routed device `exten => <number>,1,Dial(PJSIP/<ID>,30)`;
per Person with at least one routed target `Dial(PJSIP/a&PJSIP/b,30)` (targets
in ring order, missing or unrouted ones skipped; none → unavailable); after
every Dial: BUSY → `Busy(5)`, NOANSWER → `Hangup()`, otherwise
`Goto(patchbay-unavailable,s,1)`; `600` → `Answer() / Echo() / Hangup()`;
`911` → `Answer()`, SIT, `Wait(1)`, `Playback(<SoundsDir>/no-emergency)` three
times with `Wait(1)` between, `Hangup()`; each Reserved pattern, each Stream
number (A: no bridge) and the catch-alls `_X!` and `_[*#]!` → unavailable.
`[patchbay-unavailable]` `s`: `Answer()`, `Playtones(!950/330,!1400/330,!1800/330)`,
`Wait(1)`, `StopPlaytones()`, `Playback(<SoundsDir>/not-available)`,
`Hangup()`.

**Directory** (`http/directory.xml`): `CiscoIPPhoneDirectory`, Title
`patchbay`, one `DirectoryEntry {Name, Telephone}` per routed device and
person, sorted by number, at most 32 (the object's limit; the rest dropped
with a comment in the XML).

**Tests (T3), with fake drivers** (a `fakeDriver` in `compile_test.go` that
renders a deterministic file per device into its tree, can return
`ErrInvalidSpec` or a plain error on demand, and a profile with chosen
endpoint options):

- `TestResolveClaims`: oldest wins; ties by name; MAC case-insensitive; MAC
  loser holds no number; out-of-plan holds no number; a Person and a Phone
  on one number; messages verbatim; `Apply` sets the four fields.
- `TestNumberPlan`: `MatchPattern` for `1XX`, `9.` (`9` no, `91` yes),
  `*8`, `0`; `DialPatterns` order; the default plan validates (with `911`
  inside `9.`); Validate rejects overlapping ranges (`1XX` vs `10X`), a
  built-in inside a device/people/streams range (`EchoTest: "100"`), equal
  built-ins, and malformed patterns (`1X`, `X.X`, `12a`).
- `TestSettingsValidate`: each rule.
- `TestCompileWithFakes`: device left out on `ErrInvalidSpec`; a plain
  Render error fails Compile; a path outside the kind's tree fails; shared
  path identical → once; different → fail; duplicate ID → fail; missing
  secret → `ErrMissingSecret`; determinism (twice, and with every input
  slice shuffled → byte-identical bundle).
- `TestVersions`: a changed secret value changes the bundle version and only
  that device's version; a label change changes only that device; a
  Timezone change changes every phone; AllowedNetworks changes the bundle
  version, no device version; a token rotation changes the bundle version
  only.
- `TestAsteriskEscaping`: `;`, `"`, `\`, `<`, multi-byte runes; the
  rendered pjsip.conf parsed by a minimal Asterisk-config tokenizer in the
  test (sections, `key = value`, `;` comments, `\;`) yields exactly the
  expected objects and values.

### T4. Integration: `builtin` and goldens (owner: W1-T4, after T1–T3)

**Files:** `internal/drivers/builtin/builtin.go`, `builtin_test.go`;
`internal/compile/golden_test.go`, `internal/compile/testdata/golden/<case>/`
(`input.yaml` — settings, resources with `creationTimestamp`, fake secret
values — and `want/` holding every bundle file plus `want/bundle.json`).

```go
package builtin // github.com/ChristopherJMiller/patchbay/internal/drivers/builtin
func Registry() (*drivers.Registry, error) // cisco79x5.New(), softphone.New(), in that order
```

**Golden cases:**

| Case | Inputs | Pins |
|---|---|---|
| `empty` | settings only | every Asterisk file, built-ins, no endpoints |
| `house` | Phone `desk` (cisco-7945, `D0C78915E8C8`, `100`, `Desk`), Softphone `chris-laptop` (`150`, `Chris (laptop)`), Person `chris` (`301` → desk, chris-laptop) | the release A deployment: SEP, dialplan.xml, Linphone XML, pjsip.conf with `identify_by`, `send_rpid`, TCP, `force_rport = no`, `rewrite_contact = no`, `media_address`, AOR expiry 600 matching the SEP `timerRegisterExpires` and Linphone `reg_expires`, extensions with 100/150/301/600/911/reserved, directory |
| `conflicts` | two Phones on `100` (older wins), two Phones with one MAC (one lower case), Softphone `1500` (out of plan), Phone on `600`, Person ringing a missing and a conflicted device | index entries for losers, nothing rendered for them, Person dials only the routed target |
| `hostile-labels` | labels `R&D; "Lab" <1> Küche`, `Mom & Dad's`, a 64-character label | escaping in SEP, Linphone XML, pjsip.conf, directory |
| `second-phone` | `desk` plus Phone `kitchen` (cisco-7965, `D0C78915EBB5`, `101`) | shared dialplan.xml once, two SEP files, term65 path unaffected |
| `streams-disabled` | Stream `king5` (`705`) with no bridge | 705 → unavailable |
| `settings-variant` | America/New_York, address 10.0.0.5, SIP 5070, RTP 20000–20099 | every setting reaches its file |

**Tests (T4):** `TestRegistry` (`builtin.Registry()` succeeds; models
`cisco-7945 cisco-7965 cisco-7975 softphone`); `TestGolden` per case plus
cross-checks on every golden: `Validate`; `ConfigMapData` round trip;
encoded size under 64 KiB for `house`; no secret value from `input.yaml` in
any file; every placeholder names `PasswordRef(id)` of a rendered device or
the AMI secret; every rendered device's endpoint, auth and AOR present in
pjsip.conf, and its number in extensions.conf; no placeholder in `http/`; every phone's SEP `authName` is its endpoint's auth `username` and its `name` its AOR, and every device's requested expiry (SEP `timerRegisterExpires`, Linphone `reg_expires`) equals its AOR's `maximum_expiration`.

### W1 acceptance

```sh
cd /home/chris/Repos/patchbay
nix develop -c make generate fmt vet lint test        # green
nix develop -c go test -race -count=1 ./internal/drivers/... ./internal/compile/...
nix develop -c go test -tags e2e ./internal/drivers/cisco79x5/   # the e2e load exists only here
nix develop -c go test -cover ./internal/drivers/cisco79x5/ ./internal/drivers/softphone/ ./internal/compile/
                                                     # >= 90% statements each
nix develop -c make check-hygiene                    # no binaries, no secrets
# after committing: nix develop -c make check-generated
```

Done when: every row and boundary of §4 has a named test; goldens reviewed;
`TestDecideTotal` and `TestRenderDecideAgree` pass; the stub test is gone;
CLAUDE.md "Deviations" updated for anything W1 changed.

---

## 5. W2: agent and chart (component level)

| # | Component | What it does | Key tests |
|---|---|---|---|
| W2.1 | **Bundle intake** | Informer on ConfigMap `pbx-config` and Secret `pbx-secrets` (resourceNames, field selector on name). Decodes with `compile.BundleFromConfigMap`; resolves placeholders from `pbx-secrets` (§7.2); refuses the whole version on any unresolved placeholder (`LastApplyError = "unresolved placeholder <name>/<key>"`). A version that failed is retried only when `pbx-secrets` changes or after 5 min. | unit: decode/resolve/refuse; retry policy |
| W2.2 | **Apply / verify / rollback** | Writes `v<version>/` (dirs 0750, files 0640, group fsGroup), swaps `current` by renaming a temp symlink, keeps the 3 newest versions. Over AMI: reload `res_pjsip.so`, then `pbx_config.so` (dialplan), and `manager` when the AMI account changed; any other changed file reloads its owner (`rtp.conf` → `res_rtp_asterisk.so`, `pjsip_notify.conf` → `res_pjsip_notify.so`, `indications.conf` → `indications`, `logger.conf` → `logger`). **Not reloadable** (Asterisk docs: "Transport configuration is not affected by reloads"): the PJSIP transports, `asterisk.conf`, `modules.conf`. Only settings change them (address, SIP port, the module list), and the chart rolls the PBX pod when those values change (the address and ports are already in the pod spec; W2.10 adds a checksum annotation over the rest), so the agent only applies the files and lets the restart load them. Verify: `PJSIPShowEndpoints` = the bundle's rendered device IDs, and `ShowDialPlan` has `patchbay-devices` and `patchbay-unavailable`. Mismatch → swap back, reload, `LastApplyError`. First version (Asterisk not yet started): write + swap only, `/readyz` passes, verify once AMI connects (failure reported, nothing to roll back to). Sets `AppliedVersion`, `LastApplyVersion`, `LastApplyError`, `LastApplyAt`. | unit with a fake AMI (success, mismatch → rollback, reload error); e2e rollback (§9) |
| W2.3 | **File servers** | TFTP :69 and HTTP :6970 serve `current/tftp/` and the firmware store (by name; a Shared file only from the default load; never a pending or unverified file). Each transfer → fetch log (W2.6). The port-69 privilege per §5.1. | unit: pin/tftp client against the server; 6970 parity; Shared resolution; no path escapes (os.Root) |
| W2.4 | **Device HTTP :8088** | `current/http/<p>` at `/<p>` (`text/xml` for `.xml`); `ProvisioningPathPrefix + token` → the softphone's document, constant-time token compare, **one use per token** (the agent marks it spent at once; the controller rotates it), unknown/spent → 404 with no difference; logs and fetch log never contain the token (`Fetch.File` = bundle path). | unit: single use; 404 equality; no token in logs (capture logr) |
| W2.5 | **AMI client** | Login as the bundle's AMI account; reconnect with backoff; `AsteriskUp`, `AsteriskVersion`. Contacts (`PJSIPShowContacts`: URI, address, User-Agent, Call-ID, expiry, qualify), channels per endpoint (`CoreShowChannels` → `Calls`). Actions: `PJSIPNotify` to the endpoint with `Event: service-control`, `Subscription-State: active`, `Content-Type: text/plain`, and the body of usecallmanager's `[cisco-restart]` stanza (its Asterisk 20.20.0 patch, `sip_notify.conf`), one line each: `action=restart` (resync; `action=reset` for restart), `RegisterCallId={<the contact's REGISTER Call-ID>}`, then `ConfigVersionStamp`, `DialplanVersionStamp`, `SoftkeyVersionStamp` and `FeatureControlVersionStamp`, each `={00000000-0000-0000-0000-000000000000}`, and `HeadsetVersionStamp={0-0000000000}`. W2 confirms `PJSIPShowContacts` reports the Call-ID; without it no NOTIFY is sent (`Sent=false`, "no Call-ID"). | unit with a fake AMI server (the exact body); e2e: fakephone receives the NOTIFY and refetches |
| W2.6 | **Observations** | Ring buffer of 64 fetches per device; attribution: `SEP<MAC>.cnf.xml(.sgn)`, `CTLSEP<MAC>.tlv` and `ITLSEP<MAC>.tlv` (asked for first at boot) by MAC from the index, provisioning by token → device, anything else by source IP (the device that last fetched its config or registered from it). `File` = requested name without a leading `/`. `UnknownDevices` for SEP requests of MACs not in the index. Registration from AMI, web scrape (W2.7), Calls. Firmware store sensor. | unit: attribution matrix; JSON shape (`api.Empty` lists) |
| W2.7 | **Phone web scraper** | `GET http://<ip>/DeviceInformationX`, `/NetworkConfigurationX`, `/DeviceLogX` (5 s timeout; XML twins), every 60 s for phones without a live contact, every 5 min with one. Parses `versionID` and `appLoadID` (both, as the phone gives them: `api.DeviceInformation`), BootLoadID, hardware revision, serial, model, Admin VLAN, Unified CM 1, last 20 log lines. | unit against httptest fakes built from the documented XML shapes (generated, not copied) |
| W2.8 | **Firmware store + upload** | Store at `/var/lib/patchbay/firmware/<load>/<name>`. `PUT FirmwarePath(load, name)`: stream to a temp file with SHA-256, compare with the manifest from `builtin.Registry()`, atomic rename; codes as in `api.PathFirmware` (201/200/404/412/413/422). Store sensor: hash every manifest file present at start and after each upload (cache by size+mtime). | unit: every status code; pending → 412 stores nothing; mismatch → 422 stores nothing |
| W2.9 | **Sound prompts** | §2(c): `hack/gen-sounds.sh`, `make sounds`, espeak-ng in `flake.nix`, `!build/sounds/` in `.dockerignore`, `/build/` in `.gitignore`, Dockerfile COPY, agent installs into `SoundsDir` before `/readyz` and refuses to report ready if a prompt is missing from the image. | unit: install copies both formats; e2e: 911 and 0 return non-silent audio |
| W2.10 | **Chart** | Agent ServiceAccount token mounted; Role: `get,list,watch` on `configmaps` resourceNames `[<fullname>-pbx-config]`, `secrets` resourceNames `[<fullname>-pbx-secrets]`. Port 69 per §5.1, recorded in docs/e2e.md. `sounds` emptyDir. A checksum annotation on the PBX pod over the settings that compile into non-reloadable files (W2.2). `--bundle-configmap`, `--secrets` flags. **Retire `pbx.staticConfig`** (template, values, e2e values). Update `TestChartUsesTheLiveLayout` for the sounds mount. **A narrow module directory** (W1 review): Asterisk's `astmoddir` holds only the modules `compile` lists (an init step copies them from the image into an emptyDir, and the compiled `asterisk.conf` points there), so even the AMI secret cannot `ModuleLoad` `app_system`, `func_shell`, `res_agi` or `res_ari`. | helm lint; `TestChartUsesTheLiveLayout`; e2e |
| W2.11 | **Agent metrics** | `patchbay_agent_applied_version_info{version}`, `patchbay_agent_apply_failures_total`, `patchbay_agent_fetches_total{transport,result}`, `patchbay_agent_registered_endpoints`. PodMonitor gains the agent. | unit |

W2's e2e, before W3 exists: with the controller scaled to 0, the harness
itself compiles a bundle (`compile.Compile` with `builtin.Registry()` and
fixed inputs), creates the device Secrets and `pbx-secrets`, and writes
`pbx-config`; the existing register/echo tests then run against compiled
config instead of `staticConfig`. W3 replaces the harness's compile with the
real controller. The e2e settings' `AllowedNetworks` must include kind's
pod CIDR (`10.244.0.0/16`), since fakephone registers from the pod network
and the compiled `[patchbay-acl]` denies everything else; and fakephone
needs `--auth-user` (the device ID) separate from `--user` (the number)
from this point, because the compiled endpoints identify by auth username.

### 5.1 Open decisions in W2, with recommendations

| Decision | Options | Recommendation |
|---|---|---|
| **Port 69 privilege** (docs/e2e.md) | (a) a separate file-server container; (b) bind-then-drop in the agent; (c) file capability | **(a), with (b) inside it:** a `patchbay fileserver` container (same image) serving TFTP :69 and HTTP :6970 from read-only mounts of the live volume and the firmware PVC; it starts as uid 0 with only `NET_BIND_SERVICE`, `SETUID`, `SETGID`, binds, then `setresgid`/`setresuid` to 65532 (clearing all capabilities) before reading any file, and exits if the drop did not take. It holds no token, no AMI password and no resolved secret beyond the phone SEP files it serves anyway. It reports transfers to the agent over a unix socket on a shared emptyDir. The agent stays 65532 with no capabilities. Why: the TFTP parser is the code that faces untrusted packets from any device on VLAN 20; isolating it from the AMI password and control token is worth a second container. e2e asserts both processes' uid/caps from `/proc` as `TestPBXUp` does for Asterisk. |
| **Agent access to secrets** | watch every Secret in the namespace, or one projected Secret | **One projected Secret** `<fullname>-pbx-secrets`, written by the controller, key `<secret name>_<key>` (Secret names never contain `_`, so the first `_` splits). The agent's Role names exactly two objects. The controller writes it before the ConfigMap, so the agent never sees a bundle whose secrets are not there yet. Record in CLAUDE.md deviations (§5 says "its Secrets"). |
| **Retrying a failed version** | every poll, or on change | On `pbx-secrets` change, or after 5 min. |
| **Scrape cadence without "Ready"** | the controller tells the agent, or the agent approximates | The agent approximates (no live contact → 60 s, else 5 min); it decides nothing about state. |

---

## 6. W3: controller, CLI and e2e loop (component level)

| # | Component | What it does |
|---|---|---|
| W3.1 | **Settings** | The chart renders `<fullname>-settings` (ConfigMap, `settings.json` = `compile.Settings` JSON, landed tags) from values: `pbx.address`, ports, `timezone`, `ntpServer` (default `192.168.20.1`), `pbx.signalling` (`pjsip`), `numberPlan` (default §1.1), `allowedNetworks`, `amiSecret`. The controller reads it at start; a checksum annotation restarts it on change. Controller-only: poll 5 s, StaleAfter 30 s. |
| W3.2 | **Secrets** | Per device `patchbay-<ID>` (`compile.DeviceSecretName`), ownerReference to the resource (controller, blockOwnerDeletion): `sip-password` from the driver's `PasswordPolicy` with `crypto/rand`; softphones also `provisioning-token` (32 alphanumerics). Install: `<fullname>-ami` (`password`, 32 alphanumerics) and `<fullname>-agent-token` (`token`, 32 random bytes base64url), unowned. A deleted Secret is regenerated (watch owned Secrets). Then the projected `<fullname>-pbx-secrets`. Charts stop asking humans to create the agent token. |
| W3.3 | **Compile → ConfigMap** | One reconciler for the install (a singleton key): list resources and Secrets → `compile.Compile(builtin registry, …)` → write `pbx-secrets`, then `pbx-config` (`ConfigMapData`), only when the version changed. `ErrBundleTooLarge`/compile errors → Event on the ConfigMap, metric, old config stays. Triggers: any resource or owned Secret change. |
| W3.4 | **Observation poller** | Leader-only `Runnable`: `GET /v1/observations` on the agent Service every 5 s with the token (on 401, retry once with the previous token; `agent.TokenFile` docs). Keeps the last good document, `PolledAt`, `AgentReachable`. Per-device fingerprints; changed devices are enqueued through a `source.Channel`; an agent health change enqueues every device. |
| W3.5 | **Phone and Softphone reconcilers** | Build `drivers.PhoneDevice` / `SoftphoneDevice`, `claims.Apply`, `ConfigVersion` from the compiled index; `Observations` (device by ID, `CompiledVersion`); `Decide` with `DecideSettings{pjsip, 30s, PBXAddress}`. Act: `resync`/`restart` → POST to the agent (on failure or `Sent=false`: keep the previous `LastAction`, Event `ActionFailed`, requeue 30 s); `rotateToken` → new token in the Secret. Write status only when changed: `State`, conditions via `meta.SetStatusCondition` with `ObservedGeneration`, `StatusPatch.ToPhoneStatus`, `HumanAction`, softphone `provisioningURL = DeviceHTTPURL + ProvisioningPathPrefix + token`. Events: state changes, actions, `RequestIgnored`. `RequeueAfter` from the Decision. |
| W3.6 | **Person and Stream reconcilers** | Claims → `NumberConflict` / Ready reason `NumberNotInPlan`; Person: Ready False `MissingRingTargets` (message lists them) when a target is missing or unrouted. |
| W3.7 | **Discovery** | An `UnknownDevices` entry → Event on the `pbx-config` ConfigMap, reason `UnknownPhone`, message `unknown phone <MAC> requested config from <IP>`, once per MAC per agent `StartedAt`. |
| W3.8 | **Metrics** | `patchbay_devices{kind,state}`, `patchbay_registered_endpoints`, `patchbay_config_version_info{version}`, `patchbay_apply_failures_total`, `patchbay_observation_age_seconds`. |
| W3.9 | **CLI** | `status` (resources' state, conditions, human action; unknown phones via a port-forwarded `GET /v1/observations`); `softphone qr <name> [--reissue]` (QR of `status.provisioningURL` in the terminal; `--reissue` sets the annotation to the current RFC 3339 time and waits up to 30 s for a new URL); `phone resync <name>` (annotation; warns unless the phone is Registered or ConfigStale); `firmware load <zip>` (match zip entries by base name against every load of every driver, case-sensitive, ignoring directories; local SHA-256; a name several loads carry (Shared `term45.default.loads`) goes to the load whose digest it matches, never to the others; refuse pending digests and mismatches with the file named; port-forward to the agent pod (`pods/portforward`), PUT each match, print each result; exit non-zero if any file failed or no file matched); `models` (profiles, endpoint options, features, loads with pending markers). Every CLI test passes a `--kubeconfig` that does not exist. |
| W3.10 | **e2e loop** | §9. fakephone gains (`--auth-user` arrives with W2): a `run` script mode (fetch SEP over TFTP, parse it, register with its credentials for the `timerRegisterExpires` it carries, answer service-control NOTIFY by refetching, serve fake `/DeviceInformationX` etc. with a scripted `versionID` and `appLoadID`, request `.sgn`, fetch firmware files, stop refreshing); a blind transfer (in-dialog REFER to another number); a Linphone mode (GET the provisioning URL, parse the lpconfig XML, register over UDP). The e2e image and CLI are built with `GO_TAGS=e2e`. The harness stops creating the agent token. |

### 6.1 Open decisions in W3, with recommendations

| Decision | Recommendation |
|---|---|
| Where `status` gets unknown phones | Port-forward to the agent like `firmware load`; no extra object. |
| Event target for discovery | The `pbx-config` ConfigMap (always exists, namespaced). |
| What a failed action records | Nothing: memory keeps the previous `LastAction`, so the rate limit counts only sent NOTIFYs. |
| Rollback e2e without production fault-injection | Split (§9): the agent half in e2e by writing a broken ConfigMap with the controller scaled to 0; the controller half in envtest with a fake agent document. |
| QR library | A small pure-Go one (`rsc.io/qr` or `github.com/mdp/qrterminal/v3`, pinned in go.mod); whichever renders in a plain terminal at 80 columns. |

---

## 7. Agent ↔ controller interfaces

### 7.1 Control API (`internal/agent/api`, landed)

| Call | Body | Notes |
|---|---|---|
| `GET /v1/observations` | → `Observations` | `AgentStatus` now has `LastApplyVersion`; `DeviceObservations` has `Calls`; provisioning fetches carry the bundle path |
| `POST /v1/actions/resync` | `ActionRequest{device, version}` → `ActionResponse{sent, error}` | service-control `action=restart` |
| `POST /v1/actions/restart` | same | `action=reset` |
| `PUT /v1/firmware/<load>/<name>` | file bytes → `FirmwareUploadResponse` | 201/200/404/412/413/422; via `pods/portforward` from the CLI |
| `GET /healthz`, `/readyz` | — | no token |

All `/v1` calls carry `Authorization: Bearer <agent token>`.

### 7.2 Kubernetes objects

| Object | Writer | Reader | Format |
|---|---|---|---|
| ConfigMap `<fullname>-pbx-config` | controller | agent | `compile.Bundle.ConfigMapData()` (landed): file keys `tree__path`, `bundle.json` index |
| Secret `<fullname>-pbx-secrets` | controller | agent | key `<secret name>_<key>` → value, for every `SecretRef` the bundle references (placeholders, tokens, AMI) |
| Secret `<fullname>-agent-token` | controller (mints if missing) | agent (mounted file), controller | key `token` |
| Secret `<fullname>-ami` | controller | via `pbx-secrets` | key `password` |
| Secret `patchbay-<ID>` | controller | via `pbx-secrets`; CLI (`qr` reads status, not this) | `sip-password`, `provisioning-token` |
| ConfigMap `<fullname>-settings` | chart | controller | `settings.json` (`compile.Settings`) |

---

## 8. Bench-test switches (§8.6)

Each unknown is one value with a safe default; the bench answer is a one-line
change.

| Bench question | Switch (file) | Safe default | When answered |
|---|---|---|---|
| Does the phone request `.cnf.xml.sgn`? | `trustErrorPattern` and row 5 (cisco79x5/decide.go) | row on; it fires only on what is observed | refine the predicate or the pattern |
| Does it try tcp 6970 first? | not in code: 6970 is always served; router rule in luma-homeops | serve it | add the allow rule, or turn the drop into a reject (Ansible, commit-confirm) |
| Does `PJSIPNotify` service-control make it re-read config? | `Provisioning.ResyncByNotify` (cisco79x5/profile.go) | `true` (try; a human after 2 attempts) | `false` sends ConfigStale straight to the human step. Before giving up, try the body without the version-stamp lines, and `Event: check-sync` (usecallmanager's `[cisco-check-cfg]`); a body change is W2's, not a contract change |
| Highest password length that registers | `Provisioning.Password.Length` | `12` | the largest tested value ≤ 12 |
| Does it answer OPTIONS? | `Endpoint.QualifyFrequency` | `0` (off: a failed qualify would skip the contact when dialling) | `60s`; Unreachable then uses qualify |
| Blind and attended transfer on stock PJSIP | `Features[pjsip][transfer]` | `Untested` | `Supported` or `Unsupported` |
| `RTPRx` push on SIP 9.4(2) | `Features[pjsip][rtpRx]` | `Untested` | `Supported` or `Unsupported` (release C) |
| *(review)* Does the REGISTER carry `Expires: 600`? | `Endpoint.RegistrationExpiry`, rendered as `timerRegisterExpires` | `600s` | read it from `PJSIPShowContacts`; if the phone ignores the element, Asterisk still grants at most 600 and the phone must honour that |
| *(plan)* Does `\*` match a literal star in the dial template? | `dialTemplateLiteralStar` (cisco79x5/dialtemplate.go) | `false` (omit `*8`) | `true` adds `match="\*8"` (usecallmanager documents `\*` as literal; the bench confirms dialling `180` still waits for its third digit) |
| *(review)* Does `settingsAccess=2` give a restricted menu? | `settingsAccessRestricted` (cisco79x5/sep.go) | `2` (CUCM's Restricted; not on usecallmanager's page, which lists 0 and 1) | if the phone treats 2 as disabled or enabled, decide with Chris between `0` and `1`, and fix P9 to match |
| *(review)* Which `/DeviceInformationX` field holds the load ID? | none: the fold reads `versionID`, then `appLoadID`, through `runsLoad` | both accepted | record what the real phone reports in the research notes; `FirmwareCurrent` must be True on the bench |
| *(review)* Does the screen show non-ASCII labels? | none (rendered as UTF-8 XML) | as written | if they show garbled, `ScreenLabel`/`LineLabel` transliterate to ASCII and `LabelShortened` reports it |
| *(review)* Does Linphone honour `userid`, `misc/config-uri` and the `video` keys? | the provisioning document (softphone/render.go) | as in T2 | fix the key names; the golden changes |

---

## 9. Exit criteria → acceptance tests

`[real]` marks what only the real phone, Linphone, the Cisco zip or the home
cluster can close; the automated test listed beside it covers everything up
to that point.

| §11 criterion | Automated test (owner) | Real-world close |
|---|---|---|
| CI green: unit, golden, table, envtest, e2e | `make generate check-generated fmt vet lint test`, `make e2e`, `make check-hygiene` in CI (all) | — |
| Deployed: Argo `telephony` Healthy, PBX on `top` | — (luma-homeops) | `[real]` cluster |
| `firmware load` accepts the Cisco zip | `TestFirmwareLoad` e2e with the e2e load (W3); CLI unit tests on generated zips (W3); agent 201/200 (W2) | `[real]` Cisco zip, after the digests are filled |
| … and rejects a wrong checksum | e2e: a corrupted test-load file → CLI refuses locally and a forced PUT gets 422 (W3, W2) | — |
| Every §8.6 bench test run and recorded | the §8 switches exist with defaults (W1) | `[real]` 7945 |
| Dialling `600` connects at once | `TestDialTemplateMatching` (W1); e2e echo (W2) | `[real]` 7945 (no 15 s wait) |
| Desk walks to `Registered` with FirmwareCurrent and ConfigCurrent, no other human step | e2e `TestPhoneLifecycle`: fakephone scripted Unseen → NeedsFirmware → (load staged) Upgrading → Registering → Registered, asserting state, conditions and human action at each step (W3) | `[real]` 7945 |
| Shows "Desk" and the correct time | goldens pin `phoneLabel`, `timeZone`, NTP (W1) | `[real]` screen |
| Softphone: scanning the QR in Linphone registers; the URL is then spent | e2e `TestSoftphone`: fakephone fetches the URL, parses the document, registers with its credentials; the same URL → 404; state Issued → Provisioned → Registered (W3) | `[real]` Linphone |
| `100`↔`150` both ways, two-way audio, caller ID names | e2e `TestCalls`: two fakephones (phone over TCP, softphone over UDP) call each other with tones both ways; the callee sees the caller's name in From/RPID (W3) | `[real]` 7945 screen |
| `301` rings both | e2e: a third fakephone dials 301, both registered fakephones receive the INVITE (W3) | — |
| `600` echoes | existing `TestEchoCall`, on compiled config (W2) | — |
| `911` plays its announcement | e2e: call 911, answered, ≥ 2 s of non-silent audio (W2/W3) | `[real]` listen to the wording |
| Reserved numbers play "not available" | e2e: `0`, `999`, `205`, `199` (unassigned) answered with audio (W3) | — |
| Label change in git reaches the screen | e2e `TestLabelLoop`: patch `desk`'s label → new version → resync NOTIFY → fakephone refetches → SEP has the new `phoneLabel` → Registered (W3) | `[real]` screen |
| Deleting `desk`'s Secret: new password, re-provision, re-register | e2e `TestSecretHeals` (W3) | `[real]` 7945 |
| Rollback: broken config stays off, `ApplyFailed` reported | e2e `TestRollback` (controller scaled to 0, broken ConfigMap → old config still registers and echoes; agent reports `LastApplyVersion` + error) (W2); envtest: agent document with that failure → `ConfigCurrent=False ApplyFailed` + Event (W3) | — |
| Second phone appears as "unknown phone" before a resource | e2e `TestDiscovery`: fakephone with an unknown MAC → Event + `patchbay status` lists it (W3) | `[real]` second 7945 |
| … reaches `Registered` once a Phone is written | e2e (W3) | `[real]` |
| Phone↔phone calls and transfer | e2e calls between two fakephones (W3); e2e `TestTransfer`: A calls B, B sends an in-dialog REFER to `150`, the softphone fakephone rings and A's audio reaches it (proves `res_pjsip_refer.so` loads and the transfer lands in `patchbay-devices`) (W3) | `[real]` blind and attended transfer on the 7945 (bench) |

---

## 10. Review Focus

The five inputs most likely to break a real user that no test covers yet.

1. **A label with characters special to XML or Asterisk config** (`&`, `<`,
   `"`, `;`, `\`, non-ASCII; over 12 with spaces; over 30). A raw `&` makes
   the SEP file invalid XML and the phone rejects all of its config; a raw
   `;` truncates a pjsip.conf value; a quote breaks caller ID. *Pinned by:*
   golden `hostile-labels` and `TestAsteriskEscaping` (W1-T3), SEP and
   Linphone goldens with the same label and the XML-parses properties
   (W1-T1, W1-T2); control characters are already refused by the schema
   (landed, envtest).
2. **Dialling 911 on the 79x5.** The dial template mixes an exact `911` with
   the reserved `9.` and a literal-star `*8`, and Cisco reads `*` as "zero
   or more": a wrong order or conversion sends `91` or `9` early, and the
   caller hears "not available" instead of the emergency announcement.
   Whether the phone takes the first or the most specific matching template
   is undocumented. *Pinned by:* `TestDialTemplateMatching` under both
   readings (W1-T1) and the e2e 911 call (W3).
3. **The real Cisco firmware zip.** It may hold a top-level folder, extra
   files (readme, `.cop.sgn`), names in another case, or files of another
   load, and its digests are pending until Chris fills them. `firmware load`
   must match by base name, refuse pending digests with a clear message,
   and never upload a near-miss. *Pinned by:* CLI unit tests on generated
   zips of each layout (W3), agent 412/422 tests (W2), e2e `TestFirmwareLoad`
   (W3).
4. **Linphone and the one-time URL.** Linphone keeps the provisioning URL
   and re-fetches it at every start; with the token spent that is a
   provisioning error at each launch. The document must clear `config-uri`,
   and the spent URL must answer 404 without affecting the registered
   client. *Pinned by:* the softphone golden asserting `misc/config-uri` is
   cleared (W1-T2), agent single-use tests (W2), e2e second fetch → 404 while
   the softphone stays Registered (W3).
5. **A change in git while someone is on that phone.** A resync restarts the
   phone's SIP stack; sent mid-call it can drop the call. Decide must hold
   the resync until `Calls == 0`. *Pinned by:* `TestDecide/ConfigStale`
   in-call cases (W1-T1), the agent's channel count (W2), e2e: a label change
   during a fakephone call sends no NOTIFY until the BYE, then one (W3).

---

## 11. Known limitations accepted for A

- **Renumbering a phone** renames its AOR; the phone's next REGISTER (old
  number) gets 404 and it needs a power-cycle (its row reaches Registering
  or Unreachable with that instruction). Rare; documented in the runbook.
- **Unreachable detection** waits for the registration to lapse (at most
  600 s) plus the 10 min grace, until the qualify bench answer allows
  shortening it.
- **After the PBX pod is recreated** (chart or Asterisk upgrade, node
  restart) Asterisk has no contacts, because astdb lives on an emptyDir:
  calls to a device fail until it re-registers, which takes at most the
  600 s expiry (the 79x5 over TCP should notice the dropped connection
  sooner; bench).
- **Staging Cisco firmware needs a patchbay release.** The manifest is code
  and starts with pending digests, so the first `patchbay firmware load` of
  the real zip waits for a commit that fills them in and an image built
  from it. That is the price of nothing outside the binary deciding what is
  genuine.
- **Hold plays silence**: `res_musiconhold` is not loaded and there are no
  music files.
- **Long resource names**: a Phone name over 24 characters, or a Softphone
  name over 20, gets a shortened device ID with a hash (`phone-<15
  characters>-1a2b3c4d`), visible as its Secret and endpoint names.
- **A 7975** is Invalid until its load is added to the manifest.
- **More than 32 directory entries** are dropped (Cisco's object limit).
- The SEP file carries the line password in clear over TFTP (design §5).
- **In CUCM mode the 79x5 may send Cisco-proprietary requests** (out-of-dialog
  `x-cisco-remotecc` REFERs, SUBSCRIBEs for features it was not given);
  stock PJSIP answers them with errors. Expected, and harmless unless the
  bench shows otherwise.
