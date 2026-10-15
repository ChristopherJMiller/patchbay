# Release A ("core calling"): implementation plan

**Status:** plan, 2026-10-14; adversarially reviewed 2026-10-15 (changes
are marked *review*, and checked facts carry their source and date).
**Spec:** [docs/design.md](../design.md) (§11, A).
**Contracts landed with this plan:** commit `14f22fe` ("contracts: release A
shared contracts …"), amended by the review's `4a087ec` (§3). Read
[CLAUDE.md](../../CLAUDE.md) first.

Release A is built in three workflows, each implement → review → fix:

| Workflow | Builds | Touches |
|---|---|---|
| **W1: the pure core** | `internal/drivers/cisco79x5`, `internal/drivers/softphone`, `compile.Compile` and its helpers, `internal/drivers/builtin` | no I/O, no cluster |
| **W2: agent + chart** | apply/verify/rollback, TFTP/HTTP serving, device HTTP, AMI, observations, actions, scraper, firmware store and upload, sound prompts, chart | the PBX pod |
| **W3: controller + CLI + e2e loop** | reconcilers, Secrets, Compile → ConfigMap, polling, Decide orchestration, actions, conditions/events/metrics, conflicts, requests; CLI; e2e loop and rollback | the controller, `cmd/patchbay`, `test/e2e` |

W1, W2 and W3 are specified at task level (files, signatures, tests; W2
in §5.2 and W3 in §6.2, each written once its contracts had landed).

Contents: [1 Global constraints](#1-global-constraints) ·
[2 Decisions made now](#2-decisions-made-now) ·
[3 Contracts landed](#3-contracts-landed-commit-14f22fe) ·
[4 W1 tasks](#4-w1-the-pure-core-task-level) ·
[5 W2](#5-w2-agent-and-chart-component-level) ([5.2 task level](#52-w2-task-level)) ·
[6 W3](#6-w3-controller-cli-and-e2e-loop-component-level) ([6.2 task level](#62-w3-task-level)) ·
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

The compiler owns what every endpoint gets: `context`, `auth` (username =
device ID; *as built:* `auth_type = digest`, since Asterisk 22 marks
`userpass` deprecated, and `trust_id_inbound = no` written out, so a device
cannot set its own caller ID), `aors` = number, `identify_by = auth_username`,
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

## 3. Contracts landed (commit `14f22fe`)

Types, helpers and docs only, each pinned by a test; `make generate
check-generated fmt vet lint test` is green.

| Package | Added or changed |
|---|---|
| `internal/drivers` | `EndpointOptions`, `SIPTransport`, `KnownCodecs`, `Profile.Endpoint`, `ValidateProfile` (called by `NewRegistry`); `Provisioning.ResyncByNotify`; `PasswordPolicy.Validate`, `Alphanumeric`; `DeviceID`, `DeviceIDMaxLength`, `ModelSoftphone`, `RequestMaxLength`; `PhoneDevice`, `SoftphoneDevice`, `Memory.ToPhoneStatus`, `Memory.ToSoftphoneStatus`; `Device.NumberOutOfPlan`, `Device.ClaimMessage`; `ErrInvalidSpec`; `Observations.CompiledVersion`, `.AgentProblem`, `.ConfigLive`, `.ApplyFailure`; `Condition`; `DecideSettings.PBXAddress`; `RenderContext.DirectoryURL` (and `ProvisioningToken` removed: the token is in the URL, not the document); `SHA256Pending`, `FirmwareFile.Pending`, `FirmwareLoad.PendingFiles`, `.Missing`, `.Source`; `Registry.Drivers` |
| `internal/compile` | `Compile(reg, in, s)` (stub); `Bundle.Devices` (`BundleDevice`, replaces `DeviceVersions`), `Bundle.AMI` (`AMIAccount`, `AMIUsername`); `Validate` checks the index; `Settings.AMISecret` and JSON tags; `SecretRef` JSON tags; `DeviceSecretName`, `PasswordRef`, `ProvisioningTokenRef`, `SecretKeySIPPassword`, `SecretKeyProvisioningToken`; portable `TreeOf`; `DirectoryFile`; `SoundsDir`, `SoundNoEmergency`, `SoundNotAvailable`; `configmap.go` (`ConfigMapKey`, `PathFromConfigMapKey`, `IndexKey`, `Index`, `Bundle.ConfigMapData`, `BundleFromConfigMap`, `MaxConfigMapBytes`, `ErrBundleTooLarge`) |
| `internal/agent/api` | `AgentStatus.LastApplyVersion`; `DeviceObservations.Calls`; `Fetch.File` is the bundle path for provisioning documents (never the token); `PathFirmware`, `FirmwarePath`, `MaxFirmwareFileSize`, `FirmwareUploadResponse`; `ProvisioningPathPrefix` (`/p/`) |
| `api/v1alpha1` | control characters rejected in Phone/Softphone `label`, Person/Stream `name`; `ConditionLabelShortened`; `ReasonApplyPending`, `ReasonNumberNotInPlan` |
| *review, `4a087ec`* | `drivers.DeviceIDMaxLength` 63 → **30** (the 79x5 `authName` limit); `agentapi.DeviceInformation.VersionID` added and `AppLoadID` documented as the application image, never a load name; `PhoneObserved.Firmware`'s example is `SCCP45.9-4-2SR4-3S` |

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
    // As built (W1 review): the reasons of NumberConflict / MACConflict True,
    // and Ready's when a conflict makes the phone Invalid. The softphone
    // driver exports ReasonNumberConflict too (its MACConflict, never set,
    // uses v1alpha1.ConditionMACConflict as the reason).
    ReasonNumberConflict = "NumberConflict"; ReasonMACConflict = "MACConflict"
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
conversion, one template per pattern (*as built:* sorted most specific
first, a stable sort, then duplicates dropped, rather than in the order
given, so the phone's undocumented choice between first match and most
specific match can never change what dials, for any number plan; with the
default plan the order is `600`, `911`, `0`, `1..`, `3..`, `7..`, `2..`,
`9.*`, `*`):

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
| `FirstSeen` | if nil and `d` has a fetch or a live contact: the earliest fetch `At`, else `now` (rounded down to a whole second, see *Times*) |
| `Address` | live contact's `Address`, else the latest fetch's `Source`, else unchanged |
| `Firmware` | *As built (W1 review):* first, when the phone is back from a download (a config fetch after *loadChanged*, the later of *lastFw* and `m.UpgradeStartedAt`, while the remembered config fetch was not), the remembered load is forgotten (`""`). Then, from a *fresh scrape* only: `DeviceInformation.VersionID` when non-empty, else `AppLoadID` when non-empty, else unchanged (see *runsLoad*). Last, a live contact with an SCCP load remembered (`sccp` in the name, any case) clears it: an SCCP phone never sends a SIP REGISTER (design §8.2) |
| `LastConfigFetch` | the latest fetch with `File == ConfigFileName(MAC)`, `Result == served`, `Version != ""`, if newer than the remembered one at whole seconds, or in the same second with another version |
| `LastRegistered` | if a live contact: set to `now` (rounded up) when nil, when `now − LastRegistered ≥ 1 min` (coarse, so status is not rewritten every poll), or when `fetchedAfter(LastRegistered)` (so a config fetch while registered never looks newer than the registration: rows 7 and 10 split on that order); else unchanged |
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

*As built (W1 review).* *progress* = the later of `m.LastConfigFetch.At`
and *lastFw*: the phone's last config fetch or firmware download, either of
which means it restarted or changed its load. A *fresh scrape* is `d.Web`
when `Error == ""` (a failed scrape carries an older success's fields, of
unknown age) and `At` is not before *progress*; the fold's `Firmware`, row
5's trust log and row 7's Admin VLAN (P9) read only a fresh scrape. P12
still quotes whatever the agent has. *Times:* status stores `metav1.Time`
in whole seconds and the agent reports nanoseconds, so every time the fold
or a row writes is whole seconds already: observed events (`FirstSeen`,
`LastConfigFetch.At`, `UpgradeStartedAt`) round down, marks
(`LastRegistered`, a resync's `LastAction.At`) round up, and the agent's
times are compared with memory at whole seconds. Decide on memory read back
from status, with the same observations, returns that memory unchanged
(`TestDecideTotal`).

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
| 5 | `NeedsTrustReset` | no live contact, and either (a) *sgnFirst* (the earliest sgn fetch after *progress*, or ever; *as built:* after the last firmware download too, since a factory reset's download is progress) exists and `now − sgnFirst ≥ 3 min`, or (b) a *fresh scrape*'s `DeviceLog` line matches `trustErrorPattern` and not `loadAuthPattern` (below) | Ready False `NeedsTrustReset`. HumanAction P5 when the **default** load (the profile's) is available, else P6. Requeue 0. |
| 6 | `Upgrading` | `now − lastFw < 3 min`, or (`m.UpgradeStartedAt != nil` and no live contact and `!fetchedAfter(upgradeRef)`) | Set `UpgradeStartedAt` if nil to the earliest firmware fetch not older than 20 min (else `now`). Ready False `Upgrading`. HumanAction P8 once `now − UpgradeStartedAt ≥ 20 min`, else P7. **No actions ever** (a request is dropped). Requeue = the nearer of the 20-min mark and `lastFw + 3 min`, of those still ahead. |
| 7 | `Registering` | no live contact and (`m.LastRegistered == nil` or `fetchedAfter(*m.LastRegistered)`) | *start* = `m.LastConfigFetch.At`, else `m.FirstSeen`. Ready False `Registering`. Before 5 min: HumanAction "", requeue per the requeue rule. From 5 min, the first that applies: never fetched its config → P11; `fwWrong` → P10; Admin VLAN set (in a *fresh scrape*) → P9; else P12 (with its variants). |
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
    (variants: "…; its web page could not be read: <error>", "…; its web page has not been read yet" and, as built, "…; its log is empty")
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
  first full match in file order and most specific full match (*as
  built:* the full match whose matches are a subset of every other full
  match's, and the test fails if there is none; "fewest wildcards" would
  pick `*` over `9.*` after `91`, contradicting the `912` assertion
  below), and fails if they differ. It asserts `100`, `150`, `301`,
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
  | 5 NeedsTrustReset | sgnFirst at `now−3m+1s` (→ row 7, requeue 1 s), `now−3m`, `now−3m−1s`; served SEP after the sgn → not; contact → not; trust log line → at once; "Load Authentication Failed" → not; default load available vs not (two texts). *As built (W1 review):* a trust line read before the last config fetch, or in a failed scrape → not; a factory reset's download after old .sgn requests, or while the agent holds only the old trust log → row 6; a .sgn `3m−1s` after the download → row 6, `3m` after → this row |
  | 6 Upgrading | lastFw at `now−3m+1s` (match), `now−3m` with `UpgradeStartedAt` already set and no fetch after (still match via in-progress), `now−3m` with `UpgradeStartedAt` nil (→ row 7), served SEP after lastFw → row 7; UpgradeStartedAt at `now−20m+1s` ("do not unplug", requeue 1 s), `now−20m`, `now−20m−1s` ("stuck"); first sight sets UpgradeStartedAt; a request is dropped; row 7 clears UpgradeStartedAt; agent restarted mid-upgrade (`UpgradeStartedAt` set, no fetches, a config fetch from before it) → still Upgrading |
  | 7 Registering | start at `now−5m+1s` (requeue 1 s), `now−5m`, `now−5m−1s`; each human variant in order (never fetched, wrong firmware, Admin VLAN with the default load available and not, log lines, web error, not scraped; and the first applies when several do); `LastRegistered` before the last fetch (rebooted) |
  | 8 ConfigStale | (i) pending (15 s) and failed (message is the agent's error, failure about an older version ignored); (ii) `Calls=1`, also with a pending request (stays pending); (iii) request (attempts not incremented, HandledRequest set); (iv) `ResyncByNotify=false`; (v) attempt 1 at `now−5m+1s` (wait 1 s) / `now−5m` (resync, attempts 2); attempt 2 at `now−5m+1s` (wait) / `now−5m` (human); an older version's resync → fresh attempt 1; a requested resync of V (`Attempts` 0) at `now−5m+1s` (wait) / `now−5m` (resync, `Attempts` 1); (vi) first resync; config current but `fwWrong` with firmware available → resync |
  | 9 Registered | plain; firmware not reported; LabelShortened True; pending request → resync; pending request during a call, or while `!ConfigLive()`, → stays pending; a correctly upgraded phone reporting only `AppLoadID = jar45sip.9-4-2SR4-3.sbn` → Registered, FirmwareCurrent True. *As built (W1 review):* back from an SCCP→SIP upgrade with no scrape since, a failed one, or one from before its reboot → Registered, FirmwareCurrent Unknown, no resync; a scrape since → FirmwareCurrent True; a live contact with an SCCP load remembered → Registered, the load forgotten |
  | 10 Unreachable | `LastRegistered` at `now−10m+1s` (requeue 1 s), `now−10m`, `now−10m−1s`; contact lost via qualify |

- `TestDecideTotal`: a generated grid (agent health × observations ×
  memory × timing, ≥ 10 000 combinations, seeded) never reaches the
  defensive branch, always lists Ready, never returns an action from rows
  1–7 or 10, and never `resync` while `Calls > 0` or `!ConfigLive()`.
  *As built (W1 review):* the agent's times are drawn between seconds, and
  the memory, stored through status (`ToPhoneStatus`, JSON,
  `PhoneDevice`), is unchanged and decides to itself.
- `TestRenderDecideAgree`: for every Render test device, `errors.Is(err,
  drivers.ErrInvalidSpec)` ⇔ Decide's state is `Invalid` (no conflicts set).
- *As built (W1 review):* `TestLifecycle` walks the Desk phone from its old
  trust list (NeedsTrustReset, P5) through the factory reset's download
  (Upgrading, its web page down) to Registered, storing status through JSON
  between polls with sub-second agent times, and requires no second reset
  request, no action and a stable memory at every step;
  `TestRegisteredInTheFetchSecond` registers in its config fetch's second,
  and a lapsed contact is then Unreachable, not Registering.

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
wideband, rfc4733DTMF Supported. *As built:* callerIDUpdate is
Unsupported, not Supported: liblinphone never reads Remote-Party-ID, and
reads P-Asserted-Identity only at call setup and ringing, and only behind an
opt-in key (`callbacks.c` `call_received`, `call-session.cpp`
`updateToFromAssertedIdentity`).

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
| S5 | `Issued` | `!current` | Ready False `Issued`; Registered per contact; HumanAction S1, or S2 when it had fetched an older version. *As built:* no HumanAction while the bundle with its current settings is refused (`ApplyFailed`): a scan could only fetch the old settings; ConfigCurrent says why |
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

### W1 as built

Where W1 departs from this plan (CLAUDE.md records only departures from
the design). The items marked *as built* above are repeated here, so W2
and W3 write their code and e2e assertions against what exists.

**cisco79x5**
- **Fresh evidence only** (W1 review). The fold forgets the remembered
  load when the phone comes back from a download, takes a load only from a
  *fresh scrape* (succeeded, not older than its last config fetch or
  firmware download), and lets a live SIP registration rule out an SCCP
  load. Row 5's `.sgn` clock starts at that progress, and its trust log and
  P9's Admin VLAN come from a fresh scrape only. So a factory reset's
  download is Upgrading, never P5 again, and a freshly upgraded phone is
  Registered (FirmwareCurrent Unknown until a scrape succeeds), never
  resynced. See the Fold table and rows 5 and 7.
- **Whole seconds** (W1 review): events round down, marks round up; memory
  read back from status decides to itself. W3 may store `StatusPatch` as
  is; an unchanged patch means nothing to write.
- **Dial template**: most specific first, not in the order given.
- **Reasons**: `ReasonNumberConflict`, `ReasonMACConflict` added.
- **P12** has a fourth variant, "…; its log is empty".
- **Render** also refuses (plain error) an empty PBX address, a SIP port
  outside 1–65535, an NTP server that is not an IP address, an unknown
  signalling backend, a dial pattern outside the RenderContext notation,
  and an empty or over-30 device ID or number.
- **SEP values the plan left open**: `dateTemplate` `M/D/YA`,
  `peerFirmwareSharing` 0 (every firmware fetch reaches the fetch log),
  `phonePersonalization` 0, `recordingOption` disable, no voicemail
  service; every sample element left out is named in `TestRenderGolden`.
  No `authenticationURL` (release C), so `/CGI/Execute` is unauthenticated
  on VLAN 20.
- **Requeue** adds the `.sgn` deadline to rows 6, 7 and 10, by the general
  requeue rule.
- Ready messages and Unknown-row messages are the driver's; tests pin them.

**softphone**
- **Features**: callerIDUpdate Unsupported (T2 Profile).
- **S5**: no human action while the bundle is refused (S5 row).
- **Document keys beyond T2's list**, all checked in liblinphone source
  (cited in `doc.go`): `audio_codec_0` G722 and `audio_codec_1` PCMU
  enabled (Linphone ships G.722 disabled), `sip/use_rfc2833=1`,
  `sip/use_info=0`, `proxy_0/reg_route` cleared,
  `misc/transient_provisioning=1`.
- **Fold Address**: with no live contact, the latest fetch's source
  replaces the address only when that fetch is after `LastRegistered`.
- **S4 requeue**: the time left to fetch + 5 min while it waits to
  register; S7 10 min; S3, S5, S8 0.
- **Render** also refuses an IPv6 or URI-unsafe PBX address, a port outside
  1–65535, a placeholder with XML-special characters or white space, a
  non-digit number and a non-portable ID.
- A served provisioning fetch without a `Version` rotates the token but
  never becomes `LastConfigFetch`: W2 must record the version.

**compile**
- **PJSIP auth**: `auth_type = digest`, and `trust_id_inbound = no`.
- **AMI**: the classes do not contain a client with the secret (T3's
  manager.conf); W2.10 narrows the module directory.
- **User text**: runs of `@` with the white space between them collapse to
  one `@` (CLAUDE.md).
- **Number plan**: `1X` alone is accepted; a fixed-length pattern may not
  shadow a prefix of another range's number (CLAUDE.md).
- **Settings.Validate** also refuses 0.0.0.0, multicast and broadcast
  addresses, a SIP or device HTTP port inside the RTP range, device HTTP on
  the SIP port, non-canonical or duplicate CIDRs, and an AMI secret whose
  placeholder would not round-trip. **Compile** also refuses non-DNS or
  duplicate resource names, malformed `@@secret:` text in device files, a
  driver that renders nothing, and a profile `ConfigFile` (expanded from
  `{MAC}` / `{ID}`) that is not among the rendered files.
- **Extra Asterisk files**: empty `acl.conf`, `ccss.conf`, `features.conf`,
  `pjproject.conf`, `stasis.conf`, `udptl.conf`, so a clean boot log means
  something; `make asterisk-check` allowlists one stasis line.
- **Dialplan**: after `Dial`, BUSY → `Busy`, NOANSWER → `Hangup`, anything
  else → unavailable; a Person with no routed device is an explicit
  unavailable extension, left out of the directory.
- **Version**: a device password change moves that device's version (its
  stamp, so it re-reads); every other byte stays. `TestVersionsIndexOnly`
  pins that the bundle version covers the device index.
- **Index**: a device that holds its number but is not rendered (a 7975)
  has an empty `Number` in the index yet still blocks newer claimants: W3
  takes claim conditions from `compile.ResolveClaims`, not the index.
- `make asterisk-check` (build tag `asterisk`, in CI) boots every golden in
  the pinned Asterisk and registers and calls as each device; it labels
  each container with its test process and removes only orphans.
- `make test` also runs the unit tests with `-tags e2e`.

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

### 5.1 Open decisions in W2, with recommendations (resolved in §5.2.1)

| Decision | Options | Recommendation |
|---|---|---|
| **Port 69 privilege** (docs/e2e.md) | (a) a separate file-server container; (b) bind-then-drop in the agent; (c) file capability | **(a), with (b) inside it:** a `patchbay fileserver` container (same image) serving TFTP :69 and HTTP :6970 from read-only mounts of the live volume and the firmware PVC; it starts as uid 0 with only `NET_BIND_SERVICE`, `SETUID`, `SETGID`, binds, then `setresgid`/`setresuid` to 65532 (clearing all capabilities) before reading any file, and exits if the drop did not take. It holds no token, no AMI password and no resolved secret beyond the phone SEP files it serves anyway. It reports transfers to the agent over a unix socket on a shared emptyDir. The agent stays 65532 with no capabilities. Why: the TFTP parser is the code that faces untrusted packets from any device on VLAN 20; isolating it from the AMI password and control token is worth a second container. e2e asserts both processes' uid/caps from `/proc` as `TestPBXUp` does for Asterisk. |
| **Agent access to secrets** | watch every Secret in the namespace, or one projected Secret | **One projected Secret** `<fullname>-pbx-secrets`, written by the controller, key `<secret name>_<key>` (Secret names never contain `_`, so the first `_` splits). The agent's Role names exactly two objects. The controller writes it before the ConfigMap, so the agent never sees a bundle whose secrets are not there yet. Record in CLAUDE.md deviations (§5 says "its Secrets"). |
| **Retrying a failed version** | every poll, or on change | On `pbx-secrets` change, or after 5 min. |
| **Scrape cadence without "Ready"** | the controller tells the agent, or the agent approximates | The agent approximates (no live contact → 60 s, else 5 min); it decides nothing about state. |

### 5.2 W2 task level

W2 runs as **four stages**: **core**, **files** and **web** in parallel, in
one working tree, each in its own packages; then the **integrator** wires
them into `cmd/patchbay`, the chart, the sounds and e2e. The seams between
them landed first as contracts (commits `0287dbc`, `7aba7d0`): types,
interfaces and their rules, each pinned by a test. **The contract code is
authoritative;** this section says what to build on it.

#### 5.2.1 Decisions (§5.1 resolved, and new ones)

| Decision | Resolution | Why |
|---|---|---|
| **Port 69 privilege** (§5.1) | **Confirmed (a) with (b) inside, changed in two places.** `patchbay fileserver`, a third container (same image), serves TFTP :69 and HTTP :6970. It starts as uid 0 with only `NET_BIND_SERVICE`, `SETUID`, `SETGID` (container `runAsUser: 0`, `runAsNonRoot: false`, `allowPrivilegeEscalation: false`, read-only root), binds both ports, then `setgroups([])`, `setresgid`, `setresuid` to **65533:65533** (`layout.FileserverUID/GID`), checks `/proc/self/status` (all four uids and gids, no groups, `CapInh/CapPrm/CapEff/CapAmb` zero, `NoNewPrivs: 1`) and exits non-zero if anything differs, all before it opens any file. It binds **the PBX address** (`<pbx.address>:69`, `:6970`), not 0.0.0.0. | *Changed:* the plan dropped to 65532, the agent's uid. The file server must mount the whole live volume (a subPath pins one version and misses the swap), so with the agent's uid it could read every resolved file the agent wrote: the AMI password, every SIP password, the softphone documents. A uid of its own with no groups, plus per-tree modes (`layout.TreeModes`: only `tftp/` is world-readable), keeps it to the files it serves anyway. *Changed:* binding the PBX address: `top` also carries the control-plane VIP, and TFTP replies come from a new socket per transfer, so the source address must not be left to routing (the same lesson as RTP, docs/e2e.md). |
| **Agent access to secrets** (§5.1) | **Confirmed.** One projected Secret `<fullname>-pbx-secrets`, key `SecretRef.ProjectedKey()` (`<name>_<key>`, landed in `compile`), holding exactly `Bundle.SecretRefs()` (landed). The agent's Role: `get,list,watch` on `configmaps` `[<fullname>-pbx-config]` and `secrets` `[<fullname>-pbx-secrets]`; informers use a `metadata.name` field selector, which RBAC needs for list/watch with `resourceNames`. Recorded in CLAUDE.md. | As §5.1. Both sides call one function for the key, so they cannot drift. |
| **Retrying a failed version** (§5.1) | **Confirmed, made exact:** intake keys an attempt by (bundle version, `pbx-secrets` resourceVersion). A failed pair is retried when either changes, or 5 min after it failed; an applied pair is never re-applied (an informer resync is a no-op). | A failure caused by a missing secret heals when the Secret changes; anything else waits 5 min instead of reloading Asterisk every poll. *Added in the W2 review:* a version is applied only with the secret values it was compiled with (`compile.Bundle.VersionWith`): a Secret ahead of the applied version is ignored until its ConfigMap follows, and a new version waits `SecretGrace` (30 s) for its own Secret before it is refused (5.2.11). |
| **Scrape cadence** (§5.1) | **Confirmed:** `sensor.ScrapeTarget.Live` (a live contact) → every 5 min, else every 60 s; an address change scrapes at once. | The agent senses; it does not compute Ready. |
| **Narrow module directory** (W1 carry-over) | **Changed from the plan.** A separate `astmoddir` does not narrow anything: `module load ../usr/lib/asterisk/modules/app_system.so` loads through it (checked 2026-10-15 on the pinned `andrius/asterisk:22.11.0_debian-trixie`, and that the shadow below blocks it). Instead an init container (the Asterisk image, uid 1000) copies exactly `compile.Modules()` from `/usr/lib/asterisk/modules` into an emptyDir mounted at `layout.ModulesStagingDir`, and the Asterisk container mounts that emptyDir **read-only over** `compile.AsteriskModuleDir` (`/usr/lib/asterisk/modules`), hiding the image's directory. `astmoddir` is unchanged. The chart lists the modules; a Go test keeps the list equal to `compile.Modules()`. | The image's other module copies: none (checked with `find`). The init container never mounts the live config, so it never sees the AMI password. |
| **Spent provisioning tokens** (W1 carry-over) | Device HTTP persists each spend **before** it sends the document: `layout.StateDir/layout.SpentTokensFile` (written whole: temp, fsync, rename), holding per spend the token's SHA-256 and the served `sensor.FetchEvent`. At start it reloads the file and re-records every spend's event, so the controller still sees the served fetch (and rotates the token) after an agent restart. A record is pruned once the *settled* grants (the live version's once it is verified and not restored by a rollback, `apply.Applier.SettledGrants`; W2 review) no longer hold a token with that digest. `StateDir` is a 0700 directory device HTTP creates on the firmware PVC. | Spend-then-serve makes a crash between the two at worst lose a document, never reuse a token. Re-recording keeps the drivers unchanged: the softphone fold already rotates on a served fetch after the last rotation, and an old fetch before it is ignored. |
| **Which version a fetch served** | Every server resolves `layout.Current` once per request and reads only under that version's directory, reporting it as `FetchEvent.BundleVersion` with the `BundlePath`. Observations take the device's `Version` from that bundle version's index (`v<version>/bundle.json`, written by apply), or from the event when device HTTP set it. | A swap during a transfer cannot make a phone look current on a file it did not get. Indexes on disk survive agent restarts. |
| **File modes** | Per tree (`layout` package doc): version dir 0755, `bundle.json` 0640, `tftp/` 0755/0644, `asterisk/` and `http/` 0750/0640, `provisioning/` 0700/0600; group `--fs-group` (the pod's fsGroup, 1000) set explicitly with `Lchown`, modes set explicitly with `Chmod`, never left to the umask or setgid inheritance. Firmware: dirs 0755, files 0644, uploads under `.incoming/` 0700. | Asterisk reads `asterisk/` through its group; the file server reads only what is world-readable. |
| **AMI injection** | The client refuses CR/LF anywhere, and in PJSIPNotify variables `,` `"` `\` (Asterisk splits and unquotes `Variable:`), with `ami.ErrUnsafeValue`; the actions also refuse a Call-ID holding `{` or `}`. | A Call-ID is chosen by the device. |
| **First apply, and AMI down** | A version is written and swapped whether or not AMI is up; with AMI down it is *unverified*: `/readyz` passes, and apply reloads and verifies it once AMI connects. A verify failure then rolls back if there is something to roll back to; for the very first version there is not, so it stays live with `LastApplyError` set. An agent restart adopts the live version (no rewrite, no reload) and verifies it. | Asterisk may be down (crash, restart) when a fix arrives; refusing to swap would keep a bad config. |
| **`/metrics`** | On the control port, without a token, like the probes (`api.PathMetrics`). | No secrets in it; a second node port is not worth it. |
| **The firmware PVC** | Mounted whole at `layout.PersistentDir`: read-write in the agent, read-only in the file server (an emptyDir without persistence). The agent creates `FirmwareDir` (0755, on the first upload) and `StateDir` (0700, device HTTP at start) inside it; a missing store directory is an empty store. Not as subPaths. | The kubelet creates a missing subPath directory itself, root-owned, with a mode that need not admit the file server (another uid, no groups); the agent cannot fix a root-owned directory. |

#### 5.2.2 W1 carry-over → owner

| Item | Closed by |
|---|---|
| Narrow module dir; AMI secret only in the agent (and Asterisk) | Decision above; `compile.Modules()`/`AsteriskModuleDir` (landed); chart and e2e `TestModulesNarrowed` (integrator) |
| Probes run `asterisk -rx` without `-C` | Integrator: every probe passes `-C /var/lib/patchbay/config/current/asterisk/asterisk.conf` |
| `WebScrape.Error`/`At` semantics; adopt nothing from a failed scrape | `api.WebScrape` and `sensor.ScrapeRecorder` docs (landed); web: `TestScrapeFailureAdoptsNothing` |
| A served provisioning fetch carries its version | `sensor.FetchEvent.Version`, `provision.Grant.Version` (landed); web sets it, core keeps it; both test it |
| Spent tokens survive a restart | Decision above; web: `TestSpendSurvivesRestart`; integrator: e2e after an agent restart |
| e2e images built with `-tags e2e` | Integrator: Dockerfile `ARG GO_TAGS`, `make e2e-images` passes `GO_TAGS=e2e` |
| Endpoint contact ACL refusal is 401, only the global ACL is 403 | Integrator: e2e assertions and docs/e2e.md say so |
| docs/e2e.md describes the staticConfig era | Integrator rewrites it |

#### 5.2.3 Package map and ownership

```text
internal/agent/              control API; Options seams landed          integrator wires
internal/agent/api/          controller wire contract (landed)          nobody
internal/agent/sensor/       contract (landed)                          nobody
internal/agent/provision/    contract (landed)                          nobody
internal/agent/layout/       contract (landed)                          nobody
internal/agent/ami/          ami.go landed; the client                  core
internal/agent/intake/       W2.1                                       core
internal/agent/apply/        W2.2                                       core
internal/agent/actions/      W2.5 actions                               core
internal/agent/observe/      W2.6, and the fetch socket's listener      core
internal/agent/metrics/      W2.11                                      core
internal/agent/firmware/     store.go landed; W2.8                      files
internal/agent/fileserver/   W2.3, and the fetch socket's client        files
internal/agent/devicehttp/   W2.4                                       web
internal/agent/scrape/       W2.7                                       web
internal/agent/sounds/       W2.9                                       integrator
cmd/patchbay/, charts/, test/e2e/, Makefile, Dockerfile, hack/,
flake.nix, .dockerignore, .gitignore, docs/, CLAUDE.md                  integrator
```

**Rules for the parallel stages** (core, files, web):

1. Write only your own packages. Never edit a landed contract file
   (`internal/agent/{api,sensor,provision,layout}/*`, `internal/agent/server.go`,
   `internal/agent/ami/ami.go`, `internal/agent/firmware/store.go`) or
   anything under `compile`, `drivers`, `cmd`, `charts`, `test`, `docs`,
   the Makefile or CLAUDE.md. If a contract is wrong, work around it inside
   your package and report the deviation; the integrator reconciles.
2. Import your own stage's packages, contract packages and pre-existing
   ones (`compile`, `drivers`, `drivers/builtin`, `agent/api`,
   `agent/sensor`, `agent/provision`, `agent/layout`, `version`); never
   another stage's package (nor `internal/agent` itself). Interfaces you
   consume are declared in contracts or in your own package; the
   integrator's `cmd/patchbay` adds the compile-time assertions that the
   other stages' types satisfy them.
3. No new modules. `pin/tftp/v3`, `client-go`, `logr` are required
   already. Only core touches `go.mod`: importing
   `github.com/prometheus/client_golang` makes it direct, so core moves that
   one line from the `// indirect` block to the direct block by hand (no
   `go mod tidy` while others are mid-edit; go.sum is unchanged).
4. Verify your own packages only; `make fmt` reformats everyone's files.
   Per stage (with `<pkgs>` your package paths):
   `nix develop -c golangci-lint fmt <pkgs>`, `nix develop -c go vet <pkgs>`,
   `nix develop -c golangci-lint run <pkgs>`,
   `nix develop -c go test -race -count=1 <pkgs>` and the same with
   `-tags e2e`. At the end run `nix develop -c make vet lint test-unit` once
   and report (never fix) a failure that lies in another stage's package.
5. TDD, small commits, `git add` your own paths only. No cluster: only the
   integrator runs e2e. Tests use `t.TempDir()`, loopback listeners on
   port 0, and fakes; nothing binds a privileged port.

#### 5.2.4 Contracts landed (read the code; summary)

```go
// compile (0287dbc)
func (r SecretRef) ProjectedKey() (string, error)          // "<name>_<key>"
func SecretRefFromProjectedKey(key string) (SecretRef, error)
func (b Bundle) SecretRefs() ([]SecretRef, error)          // placeholders + tokens + AMI, sorted, unique
func (b Bundle) Index() Index
func (b Bundle) VersionWith(secrets map[SecretRef][]byte) (string, error) // the version these values give (W2 review; Compile uses it)
func Modules() []string                                    // modules.conf's list, in order
const AsteriskModuleDir = "/usr/lib/asterisk/modules"

// agent/layout
const PersistentDir = "/var/lib/patchbay/persistent" // the firmware PVC, whole
const FirmwareDir = PersistentDir + "/firmware"; StateDir = PersistentDir + "/state"
const StateDirMode fs.FileMode = 0o700; SpentTokensFile = "spent-tokens.json"
const FetchSocketDir = "/var/run/patchbay/fetchlog"; FetchSocket = FetchSocketDir + "/fetches.sock"
const FetchSocketMode fs.FileMode = 0o666
const ModulesStagingDir = "/var/lib/patchbay/asterisk-modules"
const AgentUID, AgentGID = 65532, 65532; FileserverUID, FileserverGID = 65533, 65533
const AsteriskUID = 1000; DefaultFSGroup = 1000
const VersionDirPrefix = "v"; IndexFile = "bundle.json"; VersionDirMode = 0o755; IndexFileMode = 0o640
func TreeModes(tree string) (dir, file fs.FileMode, ok bool)
func ValidVersion(v string) bool
func VersionDir(version string) (string, error)
func VersionFromDir(name string) (string, bool)
var ErrNoCurrent error
func Current(liveDir string) (string, error)
func OpenTree(liveDir, version, tree string) (*os.Root, error)

// agent/sensor
type FetchEvent struct {
    Transport api.Transport; File string; Result api.FetchResult; At time.Time; Source string
    BundleVersion, BundlePath, Device, Version, Error string
}
func (e FetchEvent) Validate() error
const ProvisioningUnknownFile = "p/"; MaxFileLength = 255; MaxErrorLength = 512
func ProvisioningName(name string) bool // an element "p" followed by another: may carry a token (W2 review)
type FetchRecorder interface{ RecordFetch(FetchEvent) }
type FetchRecorderFunc func(FetchEvent)
type ScrapeTarget struct{ Device, Address string; Live bool }
type ScrapeTargets interface{ ScrapeTargets(now time.Time) []ScrapeTarget }
type ScrapeRecorder interface{ RecordScrape(device string, w api.WebScrape) }
type FirmwareSensor interface{ Status() api.FirmwareStore }
const MaxWireLine = 16 << 10
var ErrLineTooLong error
func WriteFetchEvent(w io.Writer, e FetchEvent) error
func NewFetchEventReader(r io.Reader) *FetchEventReader
func (r *FetchEventReader) Next() (FetchEvent, error)

// agent/provision
type Grant struct{ Device, Token, File, BundleVersion, Version string } // String/GoString/MarshalLog redact Token; json:"-"
type Source interface{ Grants() (bundleVersion string, grants []Grant) }
type SourceFunc func() (bundleVersion string, grants []Grant) // a func as a Source (W2 review)

// agent/ami (ami.go)
var ErrNotConnected, ErrUnsafeValue error
type Client interface {
    SetAccount(username, password string)
    State() State
    Reload(ctx context.Context, module string) error
    Endpoints(ctx context.Context) ([]string, error)
    DialplanContexts(ctx context.Context) ([]string, error)
    Contacts(ctx context.Context) ([]Contact, error)
    Channels(ctx context.Context) ([]Channel, error)
    Notify(ctx context.Context, n Notify) error
}
type State struct{ Up bool; Version string; Since time.Time; Error string }
type Contact struct{ Endpoint, AOR, URI, Address, UserAgent, CallID string; ExpiresAt time.Time; Qualify api.QualifyStatus }
type Channel struct{ Name, Endpoint string }
type Notify struct{ Endpoint string; Headers []Header; Body []string }
type Header struct{ Name, Value string }

// agent/firmware (store.go)
const IncomingDir = ".incoming"; DirMode = 0o755; FileMode = 0o644
var ErrNotFound error
type Store interface{ sensor.FirmwareSensor; Open(name string) (*File, error) }
type File struct{ *os.File; Load, Name string; Size int64; ModTime time.Time }

// agent (server.go)
type Options struct {
    Tokens func() []string; Ready func() error
    Observations func(now time.Time) api.Observations   // nil: api.Empty
    Actions Actions                                     // nil: 501
    Firmware http.Handler                               // PUT /v1/firmware/{load}/{name}, after auth; nil: 501
    Metrics http.Handler                                // GET /metrics, no token; nil: 404
    Now func() time.Time; Log logr.Logger
}
type Actions interface {
    Resync(ctx context.Context, req api.ActionRequest) api.ActionResponse
    Restart(ctx context.Context, req api.ActionRequest) api.ActionResponse
}

// agent/api: PathMetrics = "/metrics"; WebScrape and Fetch.Version semantics.
```

#### 5.2.5 Stage core: W2.1, W2.2, W2.5, W2.6, W2.11

**`internal/agent/ami`** (beside `ami.go`):

```go
type Config struct {
    Addr    string                                                    // "127.0.0.1:5038" (compile.AMIPort)
    Dial    func(ctx context.Context, network, addr string) (net.Conn, error) // nil: net.Dialer
    Backoff func(attempt int) time.Duration                           // nil: 1 s doubling to 30 s
    Now     func() time.Time
    Log     logr.Logger
}
func New(cfg Config) *Conn                // implements Client; does nothing until SetAccount and Run
func (c *Conn) Run(ctx context.Context) error // keeps one session up until ctx is done
```

Protocol: read the `Asterisk Call Manager/…` banner; `Action: Login`
(`Username`, `Secret`, `Events: off`); every action carries a unique
`ActionID` and its reply is matched by it; list actions are read to their
`…Complete` event; `CoreSettings` gives `AsteriskVersion`. `Reload` is
`ModuleLoad` with `LoadType: reload` and `Module: <name>`. `Notify` is
`PJSIPNotify` with `Endpoint`, then one `Variable: <Name>=<Value>` per
header and one `Variable: Content=<line>` per body line, in order.
`Contact.Address` is `ViaAddr`, else the URI's host; `ExpiresAt` from
`ExpirationTime` (Unix seconds); `Qualify` from `Status`. `Channel.Endpoint`
is the name without `PJSIP/` and its last `-<hex>` part.

Tests (fake AMI server on a loopback listener): login success and refusal
(the error never holds the password; capture logs with `funcr`); the exact
request bytes of every action (golden strings); ActionID matching with
interleaved replies; list parsing of `EndpointList`, `ListDialplan`,
`ContactList` (with `CallId`), `CoreShowChannel`; reconnect after the
server drops, with `ErrNotConnected` meanwhile and `State` saying why;
`ErrUnsafeValue` for CR/LF and for Notify's `,` `"` `\` and a header name
with `=` or `:`; a context deadline with no reply. **Recommended:** an
`asterisk`-tagged `TestRealAsterisk…` in this package, modelled on
`internal/compile/asterisk_check_test.go`, that drives this client against
the pinned image booted on the `house` golden: login, version, endpoints,
contexts, a REGISTER's `CallId` in `PJSIPShowContacts`, and a `Notify` that
Asterisk accepts. It settles "W2 confirms PJSIPShowContacts reports the
Call-ID" before e2e. Run it with
`CGO_ENABLED=0 nix develop -c go test -tags asterisk -count=1 -run '^TestRealAsterisk' ./internal/agent/ami/`
(the integrator adds it to `make asterisk-check`).

**`internal/agent/intake`** (W2.1):

```go
type Config struct {
    Client     kubernetes.Interface
    Namespace  string
    ConfigMap  string        // <fullname>-pbx-config
    Secret     string        // <fullname>-pbx-secrets
    Applier    Applier
    RetryAfter time.Duration // 0: 5 min
    Now        func() time.Time
    Log        logr.Logger
}
type Applier interface {
    Apply(ctx context.Context, r Resolved) error     // apply.Applier
    Refuse(version string, reason error)             // records a version that never reached apply
}
type Resolved struct {
    Bundle      compile.Bundle      // as decoded: placeholders intact
    Files       map[string][]byte   // every file, placeholders resolved
    AMIPassword string
    Tokens      map[string]string   // device ID -> provisioning token, every rendered softphone
}
func Resolve(b compile.Bundle, secretData map[string][]byte) (Resolved, error)
func New(cfg Config) (*Intake, error)
func (i *Intake) Run(ctx context.Context) error
```

`Resolve` is pure: one pass of `compile.PlaceholderPattern` over each file
(a resolved value is never expanded again; files are never otherwise
changed), values looked up by `ProjectedKey`; any ref of
`b.SecretRefs()` without a non-empty value → error
`unresolved placeholder <name>/<key>` (the first in SecretRefs order) and
nothing else. `Run` watches the two objects (one informer each, a
`metadata.name` field selector, one worker), decodes with
`compile.BundleFromConfigMap`, refuses a version that is not
`layout.ValidVersion`, resolves, and calls `Apply`; a decode, validation or
resolve failure goes to `Refuse` (so `LastApplyVersion`/`LastApplyError`
show it). Retry policy per 5.2.1. A missing object is waited for (logged
once). No secret value in any log line or error.

Tests: `TestResolve` (passwords, tokens, AMI; unresolved → exact message;
a value holding `@@secret:…@@` stays literal; non-UTF-8 files untouched);
`TestRefusesBadVersion`; `TestRetryPolicy` with
`k8s.io/client-go/kubernetes/fake` and a fake Applier (no retry of a failed
pair before 5 min, retry on a Secret resourceVersion change and after 5
min, no re-apply of an applied pair); `TestNoSecretInLogs`.

**`internal/agent/apply`** (W2.2):

```go
type Config struct {
    LiveDir       string        // compile.LiveDir
    FSGroup       int           // layout.DefaultFSGroup (chart pbx.fsGroup)
    AMI           ami.Client
    Metrics       *metrics.Metrics // may be nil
    Keep          int           // versions kept, 0: 3
    VerifyTimeout time.Duration // 0: 30 s
    Now           func() time.Time
    Log           logr.Logger
}
type Status struct {
    AppliedVersion, LastApplyVersion, LastApplyError string
    LastApplyAt *time.Time
    Verified bool                   // false while the live version waits for AMI
}
func New(cfg Config) (*Applier, error)           // adopts an existing current link
func (a *Applier) Apply(ctx context.Context, r intake.Resolved) error // intake.Applier
func (a *Applier) Refuse(version string, reason error)
func (a *Applier) Run(ctx context.Context) error  // verifies an unverified live version once AMI is up
func (a *Applier) Ready() error                   // nil once a version is live
func (a *Applier) Status() Status
func (a *Applier) Grants() (string, []provision.Grant)     // provision.Source
func (a *Applier) Applied() (string, compile.Index, bool)  // the live version's index
func (a *Applier) Index(version string) (compile.Index, bool) // live or kept (memory, else v<version>/bundle.json)
```

Apply: write `v<version>` complete under a staging name in `LiveDir`
(modes and group per 5.2.1, `bundle.json` = `json.Marshal(b.Index())`),
rename it into place (reuse an existing complete `v<version>`), point a
temporary symlink at it and rename it over `current`. With AMI up: reload
`res_pjsip.so`, then `pbx_config.so`, then the owner of each other
`asterisk/` file whose resolved bytes changed (`rtp.conf` →
`res_rtp_asterisk.so`, `pjsip_notify.conf` → `res_pjsip_notify.so`,
`indications.conf` → `indications`, `logger.conf` → `logger`), and
`manager` last when `manager.conf` changed, followed by
`AMI.SetAccount(new)`; `asterisk.conf`, `modules.conf` and the transports
are not reloadable (the pod restarts for them). Verify, retrying for up to
`VerifyTimeout`: `Endpoints()` equals the sorted IDs of the index's devices
with a `Version`, and `DialplanContexts()` holds `compile.DevicesContext`
and `compile.UnavailableContext`. A reload error or a mismatch: swap the
previous version back, repeat the reloads, set `LastApplyVersion` and
`LastApplyError` (`reload res_pjsip.so: …`, `verify: endpoints missing a, b;
unexpected c`), count `ApplyFailed`, keep `AppliedVersion`. Success: all
four fields, `SetAppliedVersion`, new grants from `r.Tokens`, and remove
versions beyond the `Keep` newest (never `current` or the previous). With
AMI down: unverified (5.2.1). `SetAccount` is called on the first apply
and whenever the account changes.

Tests (fake `ami.Client`, `t.TempDir()`, `os.Getgid()` as FSGroup):
`TestWriteModesAndGroup` (every directory and file against
`layout.TreeModes`, the index file, the group); `TestSwapIsAtomic` (a
reader resolving `current` in a loop during 50 applies always finds a
complete version); `TestReloadPlan` (which modules, in which order, for
which changed files; `manager` then `SetAccount`); `TestVerifyMismatchRollsBack`;
`TestReloadErrorRollsBack`; `TestFirstVersionWithoutAMI` (Ready at once,
verified on connect; failure reported with nothing to roll back to);
`TestAdoptOnRestart` (no rewrite, no reload); `TestKeepsThreeVersions`;
`TestGrants` (copy, every rendered softphone, versions and file);
`TestIndexOfAnOlderVersion` (from disk after a restart);
`TestRefusesInvalidVersion`.

**`internal/agent/actions`** (W2.5 actions):

```go
type Config struct {
    AMI     ami.Client
    Applied func() (string, compile.Index, bool) // apply.Applier.Applied
    Timeout time.Duration                       // 0: 10 s
    Log     logr.Logger
}
func New(cfg Config) *Actions // implements agent.Actions
```

For a device in the applied index: `Contacts()` now, its contact (by
`Endpoint`, else by `AOR` = its number), then `Notify` with headers, in
order, `Event: service-control`, `Subscription-State: active`,
`Content-Type: text/plain`, and body lines, in order:

```text
action=restart                 (resync; action=reset for restart)
RegisterCallId={<the contact's Call-ID>}
ConfigVersionStamp={00000000-0000-0000-0000-000000000000}
DialplanVersionStamp={00000000-0000-0000-0000-000000000000}
SoftkeyVersionStamp={00000000-0000-0000-0000-000000000000}
FeatureControlVersionStamp={00000000-0000-0000-0000-000000000000}
HeadsetVersionStamp={0-0000000000}
```

`Sent: false` with `Error` for: `unknown device <id>`, `no live contact`,
`no Call-ID` (the contact has none), `unsafe Call-ID`, AMI down or refusing
(its error). Tests pin the exact `ami.Notify` for both actions and every
`Sent: false` reason; the AMI bytes are pinned in `ami`'s tests.

**`internal/agent/observe`** (W2.6):

```go
type Config struct {
    AMI       ami.Client
    Applied   func() (string, compile.Index, bool)            // apply
    Index     func(version string) (compile.Index, bool)      // apply
    Status    func() apply.Status                             // apply
    Firmware  sensor.FirmwareSensor                           // the firmware store
    Metrics   *metrics.Metrics                                // may be nil
    PollEvery time.Duration                                   // 0: 5 s
    Now       func() time.Time
    Log       logr.Logger
}
func New(cfg Config) *Store
func (s *Store) Run(ctx context.Context) error                  // polls Contacts and Channels
func (s *Store) RecordFetch(e sensor.FetchEvent)                // sensor.FetchRecorder
func (s *Store) RecordScrape(device string, w api.WebScrape)    // sensor.ScrapeRecorder
func (s *Store) ScrapeTargets(now time.Time) []sensor.ScrapeTarget
func (s *Store) Document(now time.Time) api.Observations        // agent.Options.Observations
func ServeFetchSocket(ctx context.Context, path string, rec sensor.FetchRecorder, log logr.Logger) error
```

Attribution, first rule that applies: the event's `Device` (kept for that
device even before any index has it: device HTTP re-records persisted
spends at start); a file named `SEP<MAC>.cnf.xml`,
`SEP<MAC>.cnf.xml.sgn`, `CTLSEP<MAC>.tlv` or
`ITLSEP<MAC>.tlv` (12 hex digits, compared upper-case) → the device with
that MAC; else the `Source`: the device whose latest served config fetch or
live contact came from that address (the latest wins); else unattributed
(counted, not listed). `Fetch.Version`: the event's, else, for a served
event whose `BundlePath` is in the device's `Files` in the index of
`BundleVersion`, that device's `Version`; else empty. 64 fetches per
device, kept in `At` order. `UnknownDevices`: SEP config requests (`.cnf.xml`
and `.cnf.xml.sgn`) of MACs no applied device has, at most 64 (the least
recently seen go first), dropped once a device has the MAC. Registration
and `Calls` from the latest poll (a contact belongs to the device named by
its `Endpoint`, else to the one whose number is its `AOR`; a channel to
its `Endpoint`; both cleared while AMI is down; the poll also sets
`SetRegisteredEndpoints`). `Devices`: every device of the applied index
any sensor has seen, sorted by ID. Every valid event counts in
`metrics.Fetch`, attributed or not.
`ScrapeTargets`: Phones of the applied index with an address (the live
contact's, else the source of their latest attributed served fetch); `Live`
= a contact that expires after `now` and is not qualify-unreachable.
`Document` is a deep copy with every list non-nil; `Agent` from `Status()`
and `AMI.State()`; `Firmware` from `Firmware.Status()`.
`ServeFetchSocket` removes a stale socket file, listens, chmods
`layout.FetchSocketMode`, reads each connection with
`sensor.NewFetchEventReader`, drops `http8088` events and events with
`Device` or `Version` set (device HTTP is in-process, and the file server
never knows either: a compromised file server must not be able to claim a
softphone's provisioning fetch), closes a connection on its first bad
line, and removes the socket when `ctx` ends.

Tests: `TestAttribution` (a matrix: each rule × each transport, a
provisioning event with `Device`, an unattributable fetch);
`TestFetchVersion` (served SEP → the device version of that bundle
version, an older version through `Index`, notFound → none, the event's own
kept); `TestRingBuffer`; `TestUnknownDevices`; `TestRegistrationAndCalls`;
`TestScrapeTargets`; `TestDocumentShape` (`api.Empty` lists, sorted, a deep
copy); `TestFetchSocket` (a real unix socket in `t.TempDir()`: events
arrive; `http8088` refused; a bad line ends only its connection; a stale
file is replaced; the mode).

**`internal/agent/metrics`** (W2.11):

```go
func New() *Metrics                                   // its own prometheus.Registry, plus Go and process collectors
func (m *Metrics) Handler() http.Handler              // agent.Options.Metrics
func (m *Metrics) SetAppliedVersion(version string)   // patchbay_agent_applied_version_info{version} 1, one series
func (m *Metrics) ApplyFailed()                       // patchbay_agent_apply_failures_total
func (m *Metrics) Fetch(t api.Transport, r api.FetchResult) // patchbay_agent_fetches_total{transport,result}
func (m *Metrics) SetRegisteredEndpoints(n int)       // patchbay_agent_registered_endpoints
```

Every method is a no-op on a nil `*Metrics`. Test: gather and check names,
labels and values.

**Core acceptance:** with
`P="./internal/agent/ami/... ./internal/agent/intake/... ./internal/agent/apply/... ./internal/agent/actions/... ./internal/agent/observe/... ./internal/agent/metrics/..."`:
`go test -race -count=1 $P` (and `-tags e2e`), `go vet $P`,
`golangci-lint run $P` green; `go test -cover $P` ≥ 80% each.

#### 5.2.6 Stage files: W2.3, W2.8

**`internal/agent/firmware`** (beside `store.go`):

```go
type Manifest struct{ /* unexported */ }
func NewManifest(reg *drivers.Registry) (*Manifest, error)
func (m *Manifest) Lookup(load, name string) (drivers.FirmwareLoad, drivers.FirmwareFile, bool) // an upload's target
func (m *Manifest) Resolve(name string) (drivers.FirmwareLoad, drivers.FirmwareFile, bool)      // a device's request (Shared → default load)
func NewDisk(dir string, m *Manifest, log logr.Logger) *Disk // implements Store; does no I/O
func (d *Disk) Open(name string) (*File, error)
func (d *Disk) Status() api.FirmwareStore
func (d *Disk) Put(ctx context.Context, load, name string, body io.Reader) (int, api.FirmwareUploadResponse)
func NewUploadHandler(d *Disk, log logr.Logger) http.Handler // agent.Options.Firmware
```

Default loads: for each driver and each of its models, the profile's
`Provisioning.DefaultFirmware`, if that driver's manifest has it. A Shared
name resolves to the one default load carrying it; none or several → not
served (logged once). The upload handler reads `r.PathValue("load")` and
`("name")` and always answers a `FirmwareUploadResponse`: 404 (`Lookup`
fails); 412 (pending digest; the body is not read; nothing stored); 413
(`Content-Length` over `api.MaxFirmwareFileSize`, or the stream exceeds
it); else the body streams into `.incoming/<random>` (0600; `Put` creates
the store directory with `DirMode` and `.incoming` with 0700 when missing)
through SHA-256: a mismatch is 422 and the temp file is removed; a match
with the file already stored and verified is 200; otherwise fsync, chmod
`FileMode`, `MkdirAll` the load directory (`DirMode`), rename into place,
fsync the directory: 201. The manifest size is advisory, never enforced.
`Status` lists every manifest file in registry, load and file order;
`Open`/`Status` share one digest cache keyed by (device, inode, size,
modification time); only regular files count (a symlink never does); all
access through an `os.Root` on the store directory, opened lazily: while
the directory is missing the store is empty, and it is retried on the next
call.

Tests (fake drivers with generated file bytes; the e2e load under
`-tags e2e`): `TestResolve` (unique, Shared from the default, Shared with
no or two defaults, unknown); `TestUpload` (201; 200 on repeat; 404 load
and file; 412 stores nothing; 413 by header and by stream; 422 stores
nothing and leaves `.incoming` empty); `TestOpenServesOnlyVerified`
(pending never; a file planted with the right digest is served; a wrong
digest, a rewrite in place and a symlink are not); `TestDigestCache`
(a second `Status` hashes nothing; a changed mtime re-hashes);
`TestE2ELoad` (`//go:build e2e`: upload `cisco79x5.E2ELoadFile` bytes →
201, `Open` serves them).

**`internal/agent/fileserver`** (W2.3):

```go
type Config struct {
    LiveDir    string               // compile.LiveDir, mounted read-only
    Firmware   firmware.Store       // firmware.NewDisk(layout.FirmwareDir, …)
    TFTPListen string               // "<pbx address>:69"
    HTTPListen string               // "<pbx address>:6970"
    ReportSocket string             // layout.FetchSocket
    Recorder   sensor.FetchRecorder // nil: a Reporter on ReportSocket, started after the drop; tests inject
    UID, GID   int                  // layout.FileserverUID, layout.FileserverGID
    Drop       func(uid, gid int) error // nil: DropPrivileges
    Now        func() time.Time
    Log        logr.Logger
}
func Run(ctx context.Context, cfg Config) error       // bind both, Drop, then serve until ctx ends
func DropPrivileges(uid, gid int) error               // setgroups([]), setresgid, setresuid, CheckDropped(/proc/self/status)
func CheckDropped(procStatus []byte, uid, gid int) error
func NewReporter(socket string, log logr.Logger) *Reporter // sensor.FetchRecorder; no I/O until Run
func (r *Reporter) Run(ctx context.Context) error
```

`Run` opens no file and no socket but its two listeners before `Drop`
returns (its own `Reporter` connects only afterwards); a `Drop` error
closes them and is returned (the process exits non-zero). Name resolution, the same for both transports: strip one
leading `/` (HTTP: the unescaped URL path); a name with `\`, NUL, an empty,
`.` or `..` element, or over `sensor.MaxFileLength` is notFound and is
reported with `File` = `strconv.QuoteToASCII(name)` cut to 200 bytes;
otherwise `Firmware.Open(name)` first (served from the store, no
`BundleVersion`), then, if it says `ErrNotFound`, the live `tftp/` tree:
`compile.TreeOf("tftp/"+name)` must pass, `layout.Current` once,
`layout.OpenTree(…, compile.TreeTFTP)`, regular files only; `BundleVersion`
and `BundlePath` `tftp/<name>` are reported whenever a version is live.
Exactly one event per request: served, notFound or error (an HTTP HEAD is
answered but not reported). TFTP:
`pin/tftp/v3`, read requests only (write requests are refused and not
reported), `Server.Serve` on the UDP socket bound before the drop,
5 s timeout, 5 retries; the reply's source is the address the request
came to (`RequestPacketInfo.LocalIP`). HTTP :6970: GET and HEAD (405
otherwise), `http.ServeContent`, `text/xml; charset=utf-8` for `.xml`,
else `application/octet-stream`; one fixed 404 body; `ReadHeaderTimeout`
5 s, `WriteTimeout` 5 min, `MaxHeaderBytes` 8 KiB. The `Reporter` speaks
the `sensor` wire format: a queue of 1024 events (the oldest dropped first,
drops counted and logged), reconnecting about once a second, a 1 s write
deadline.

Tests: `TestTFTPServesTheLiveVersion` (a `pin/tftp` client against the
server on 127.0.0.1:0; bytes and event); `TestHTTP6970Parity` (the same
names give the same bytes and events but `http6970`);
`TestSwapMidTransfer` (the bytes and `BundleVersion` of the version
resolved at the start); `TestFirmwareFirst` (a fake `Store`);
`TestNoPathEscapes` (`../asterisk/pjsip.conf`, `/../x`, `%2e%2e/x`, an
absolute path, a symlink in `tftp/` to `../asterisk/` → notFound, never the
bytes); `TestOneEventPerRequest` (served, notFound, a client that stops
acking → error); `TestInvalidNameIsReportedQuoted`; `TestReplySource`
(a request to 127.0.0.2 with the server on 0.0.0.0 is answered from
127.0.0.2); `TestRunDropsBeforeServing` (an injected `Drop` sees both
listeners bound and the `Store` and `Recorder` unused);
`TestRunExitsWhenDropFails`; `TestCheckDropped` (sample status texts: each
field wrong once); `TestReporter` (a unix socket in `t.TempDir()`: order
kept; the listener restarts and events continue; overflow drops the
oldest).

**Files acceptance:** with
`P="./internal/agent/firmware/... ./internal/agent/fileserver/..."`, the
same commands as core; coverage ≥ 80% each.

#### 5.2.7 Stage web: W2.4, W2.7

**`internal/agent/devicehttp`** (W2.4):

```go
type Config struct {
    LiveDir  string               // compile.LiveDir
    Grants   provision.Source     // apply
    Recorder sensor.FetchRecorder // observe
    StateDir string               // layout.StateDir
    Now      func() time.Time
    Log      logr.Logger
}
func New(cfg Config) (*Server, error) // creates StateDir (layout.StateDirMode) if missing, loads SpentTokensFile, re-records each spend's event
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (s *Server) Run(ctx context.Context, addr string) error // "<pbx address>:8088"
```

GET and HEAD only (405 otherwise). `ProvisioningPathPrefix + token`: the
`provision` package rules. The token is the rest of the path; with a `/`
in it, or matching no grant, or a spent one, the answer is the one 404
(status, headers, body byte-identical to an unknown path; `Content-Type:
text/plain; charset=utf-8`, `X-Content-Type-Options: nosniff`, body
`404 page not found\n`). A match compares `sha256(token)` with every
grant's `sha256(Token)` using `subtle.ConstantTimeCompare`, all of them,
with no early exit. A first match, in this order: read `Grant.File` from
`v<Grant.BundleVersion>` via `layout.OpenTree` into memory (missing: the
one 404, nothing spent, recorded as notFound); persist the spend (the state
file rewritten whole: temp file, fsync, rename, fsync the directory; if
that fails: 503, nothing served, nothing spent, recorded as error); answer
200 `text/xml; charset=utf-8` with `Cache-Control: no-store`; record the
persisted event `{http8088, File = BundlePath = Grant.File, BundleVersion,
Device, Version, served}`, even if the client stopped reading: the token is
spent either way, and only a served fetch makes the controller rotate it
(a fresh QR) instead of leaving the softphone Issued on a dead URL. A
spent token: 404, recorded as notFound with `File` = `Grant.File` and
`Device`. No token matched: 404, recorded as notFound with `File =
sensor.ProvisioningUnknownFile`. Any other path `/<p>`: `layout.Current`,
`compile.TreeOf("http/"+p)` must pass, `layout.OpenTree(…,
compile.TreeHTTP)`, regular files only; `text/xml; charset=utf-8` for
`.xml`, else `application/octet-stream`; recorded with `File = p`,
`BundlePath = http/<p>`. Pruning: whenever the grants' bundle version
differs from the last prune (and is not empty), drop spends whose digest
no current grant has. A corrupt state file fails `New`. Timeouts:
`ReadHeaderTimeout` 5 s, `ReadTimeout` 10 s, `WriteTimeout` 30 s,
`MaxHeaderBytes` 8 KiB. No token in any log line, event, error or the
state file.

Tests: `TestServesTheHTTPTree`; `TestProvisioningOneUse` (200 with the
document of the grant's own version even after `current` moved on; then
404); `TestIdentical404s` (an unknown path, an unknown token, a spent
token, a token followed by `/x`, the bare prefix: byte-identical responses
but `Date`); `TestComparesEveryGrant` (a comparison hook, an unexported
package variable set from `export_test.go`, sees every grant compared on a
first-grant match); `TestServedProvisioningCarriesVersion`;
`TestSpendSurvivesRestart` (a second `Server` on the same `StateDir`: 404,
and the served event is recorded again with `Device` and `Version`);
`TestSpendPersistedBeforeServing` (a read-only `StateDir`: 503, no
document, the token still unspent afterwards); `TestPrune` (a rotated token
is pruned; the same token across bundle versions stays spent);
`TestNoTokenAnywhere` (logs via `funcr`, events, the state file);
`TestBeforeFirstApply` (no grants, no `current`: 404).

**`internal/agent/scrape`** (W2.7):

```go
type Config struct {
    Targets      sensor.ScrapeTargets  // observe
    Recorder     sensor.ScrapeRecorder // observe
    Client       *http.Client          // nil: 5 s timeout, no redirects, no proxy
    Port         int                   // 0: 80
    NotLiveEvery time.Duration         // 0: 60 s
    LiveEvery    time.Duration         // 0: 5 min
    Tick         time.Duration         // 0: 5 s
    Concurrency  int                   // 0: 4
    Now          func() time.Time
    Log          logr.Logger
}
func New(cfg Config) *Scraper
func (s *Scraper) Run(ctx context.Context) error
func (s *Scraper) Scrape(ctx context.Context, t sensor.ScrapeTarget) api.WebScrape // one scrape, merged per the rules; records nothing
// The pages' XML shapes, exported so the e2e fakephone (W3) serves exactly what this parses:
type DeviceInformationX struct{ /* xml tags */ }
type NetworkConfigurationX struct{ /* xml tags */ }
type DeviceLogX struct{ /* xml tags */ }
func ParseDeviceInformation(b []byte) (*api.DeviceInformation, error)
func ParseNetworkConfiguration(b []byte) (*api.NetworkConfiguration, error)
func ParseDeviceLog(b []byte) ([]string, error) // the last 20 lines, oldest first
```

A device is due when never scraped, when its address changed, or when its
interval (`Live` → `LiveEvery`, else `NotLiveEvery`) has passed since the
last attempt; devices gone from the targets are forgotten. Pages:
`http://<address>:<port>/DeviceInformationX`, `/NetworkConfigurationX`,
`/DeviceLogX`; 2xx only; bodies over 256 KiB fail; XML decoded with a
hand-written `CharsetReader` for `ISO-8859-1`/`latin1`, `US-ASCII` and
`UTF-8` (no new module). Element names are matched case-insensitively:
`versionID`, `appLoadID`, `bootLoadID`, `hardwareRevision`, `serialNumber`,
`modelNumber`; the Admin VLAN as `AdminVLANId` (or `AdminVLANID`); Unified
CM 1 as `CallManager1`, `UnifiedCM1` or `CUCM1`; log lines are the `status`
elements' text (every leaf's text if there are none), trimmed, empty ones
dropped. Which names a real 79x5 uses is a bench check (§8); the shapes
are written from the documented field lists and generated, never copied
from a phone. Addresses that are loopback, unspecified, multicast or
link-local are refused (a failed scrape) without a request. The merge is
`sensor.ScrapeRecorder`'s rule: a failed scrape adopts nothing.

Tests: `TestParse…` on generated fixtures (case variants, an ISO-8859-1
document with a Latin-1 byte, missing fields); `TestScrapeSuccess`
(httptest; every field; `At` is when the last page was read);
`TestScrapeFailureAdoptsNothing` (DeviceInformationX read, DeviceLogX 500:
`Error` set, the fields are the previous success's, nil the first time);
`TestSchedule` (60 s, 5 min, an address change, `Live` switching, removal);
`TestRefusedAddresses`; `TestBodyLimit`; `TestNoRedirects`; `TestTimeout`.

**Web acceptance:** with
`P="./internal/agent/devicehttp/... ./internal/agent/scrape/..."`, the same
commands as core; coverage ≥ 80% each.

#### 5.2.8 Integrator: W2.9, W2.10, cmd, e2e, docs

After the three stages, in one tree:

1. **Reconcile** reported deviations; add compile-time assertions in
   `cmd/patchbay`: `var _ ami.Client = (*ami.Conn)(nil)`,
   `_ intake.Applier = (*apply.Applier)(nil)`, `_ provision.Source =
   (*apply.Applier)(nil)`; `sensor.FetchRecorder`, `sensor.ScrapeRecorder`
   and `sensor.ScrapeTargets` each `= (*observe.Store)(nil)`;
   `_ firmware.Store = (*firmware.Disk)(nil)`, `_ agent.Actions =
   (*actions.Actions)(nil)`, `_ sensor.FetchRecorder =
   (*fileserver.Reporter)(nil)`, `_ http.Handler = (*devicehttp.Server)(nil)`.
   Run `go mod tidy` once.
2. **`internal/agent/sounds`** (W2.9): `Install(src, dst string) error`
   (both formats of `compile.SoundNoEmergency` and `SoundNotAvailable`,
   0644) and `Check(dst string) error`; `hack/gen-sounds.sh`, `make
   sounds`, espeak-ng in `flake.nix`, `!build/sounds/` in `.dockerignore`,
   `/build/` in `.gitignore`, the Dockerfile copies them to
   `/usr/share/patchbay/sounds/`, `make image` depends on `sounds` (§2(c)).
3. **`cmd/patchbay agent`**: flags `--namespace` ($POD_NAMESPACE),
   `--bundle-configmap`, `--secrets`, `--live-dir` (`compile.LiveDir`),
   `--firmware-dir`, `--state-dir`, `--sounds-dir`, `--sounds-src`,
   `--fetch-socket`, `--fs-group`, `--device-http-listen`, plus the
   existing `--listen`, `--token-file`. Wiring: `builtin.Registry()` →
   `firmware.NewManifest` → `NewDisk`; `ami.New`; `metrics.New`;
   `apply.New`; `observe.New`; `intake.New`; `devicehttp.New`;
   `scrape.New`; `actions.New`; `agent.NewHandler(Options{Ready: apply ∧
   sounds, Observations: obs.Document, Actions, Firmware:
   firmware.NewUploadHandler, Metrics: m.Handler()})`; run every `Run`
   and `ServeFetchSocket` under one errgroup. **`cmd/patchbay
   fileserver`**: `--live-dir`, `--firmware-dir`, `--tftp-listen`,
   `--http-listen`, `--fetch-socket`, `--uid`, `--gid`; it builds the
   manifest and `firmware.NewDisk` (no I/O) and calls `fileserver.Run`,
   whose own `Reporter` starts after the drop.
4. **Chart** (W2.10): the agent gets a projected service-account token
   (only the agent: the pod keeps `automountServiceAccountToken: false`),
   a Role and RoleBinding per 5.2.1, the flags above; a `fileserver`
   container (5.2.1 security context; the `tftp`/`cisco-http` container
   ports move to it; live volume and firmware read-only, the fetch socket
   emptyDir read-only); an `asterisk-modules` init container first (the
   Asterisk image, uid 1000, `sh -ec 'for m in "$@"; do cp
   /usr/lib/asterisk/modules/"$m" /var/lib/patchbay/asterisk-modules/;
   done' sh <modules…>`), its emptyDir mounted read-only at
   `/usr/lib/asterisk/modules` in Asterisk; the firmware PVC whole at
   `layout.PersistentDir` (agent rw, file server ro; an emptyDir without
   persistence); `sounds` and the fetch socket emptyDirs;
   every Asterisk probe with `-C`; a checksum annotation over the settings
   that compile into non-reloadable files; the PodMonitor scrapes the
   agent's `/metrics`; **`pbx.staticConfig` retired** (template, values,
   e2e values). Tests: `TestChartUsesTheLiveLayout` (three live mounts:
   agent, Asterisk and the file server read-only), a new
   `TestChartModulesMatchCompile`, and one that every `layout` path and
   uid appears in the chart.
5. **Build**: Dockerfile `ARG GO_TAGS` passed to `go build -tags`; `make
   e2e-images` builds with `GO_TAGS=e2e`; `make asterisk-check` also runs
   `./internal/agent/ami/` if core added its real-Asterisk test.
6. **e2e**, with the controller scaled to 0 (the harness compiles: §5's
   last paragraph; `AllowedNetworks` includes `10.244.0.0/16`;
   fakephone gains `--auth-user`): `TestPBXUp` also asserts the agent
   (65532, no capabilities) and the file server (65533:65533, no groups,
   all capability sets zero, `NoNewPrivs`) from `/proc`, and that the
   file server cannot open `current/asterisk/manager.conf`;
   `TestModulesNarrowed` (over AMI as the agent's account, from a
   host-network tools-image pod: fakephone gains an `ami` subcommand;
   `ModuleLoad` of `app_system.so`, `func_shell.so`,
   `res_agi.so`, `res_ari.so` and `../usr/lib/asterisk/modules/app_system.so`
   all fail; `module show` lists exactly `compile.Modules()`);
   `TestTFTPAndHTTP6970` (the SEP file, resolved, over both; a `.sgn` is
   notFound; the fetches attributed in `/v1/observations` with the
   device's version); `TestFirmwareUploadAndServe` (PUT the e2e load
   through the agent Service with the token: 201, then 200; a corrupted
   file 422; then served over TFTP and present in the store sensor);
   `TestRegister` and `TestEchoCall` on compiled config; `TestPrompts` (911
   and 0 answered with ≥ 2 s of non-silent audio); `TestRollback` (a broken
   ConfigMap: the old config still registers and echoes; the agent reports
   `LastApplyVersion` and the error); `TestProvisioningOneTime` (200, then
   404, then 404 again after the agent container is killed, with the
   served fetch back in the observations); `TestResyncNotify` (fakephone
   receives the service-control NOTIFY with the 5.2.5 body and refetches).
   Assertions say 401 for an endpoint ACL refusal and 403 only for the
   global ACL.
7. **Docs**: rewrite docs/e2e.md for compiled config, the file server and
   the module shadow; CLAUDE.md commands (`make sounds`).

#### 5.2.9 W2 acceptance

```sh
cd /home/chris/Repos/patchbay
nix develop -c make generate check-generated fmt vet lint test   # green
nix develop -c go mod tidy -diff                                 # empty
nix develop -c make check-hygiene
nix develop -c make asterisk-check
nix develop -c make e2e                                          # leaves no cluster
```

Done when every 5.2.5–5.2.8 test exists and passes, every carry-over in
5.2.2 is closed, and CLAUDE.md "Deviations" lists what W2 changed.

#### 5.2.10 Handed to W3

- The controller writes `pbx-secrets` (keys `ProjectedKey`, exactly
  `SecretRefs`, with exactly the values it compiled with: the agent checks
  them with `Bundle.VersionWith`, 5.2.11) before `pbx-config`, and mints the
  agent token; in W2 the e2e harness does both.
  The harness's stand-in is `test/e2e/bundle_test.go` (`applyBundle`); W3
  deletes it, or the two would fight over `pbx-config`.
- fakephone's scripted `run` mode, its fake web pages (built from
  `scrape`'s exported page types) and its Linphone mode; `TestPhoneLifecycle`
  and the other W3 rows of §9.
- `patchbay firmware load` and `status` (W2 tests the upload endpoint
  directly).
- The real-device questions stay §8's.

#### 5.2.11 W2 as built

Where W2 departs from 5.2.1–5.2.10, so W3 builds on what exists. The
stage-level deviations are the stages' reports, condensed; CLAUDE.md
records the departures from the design.

**Core** (intake, apply, ami, actions, observe, metrics)
- **Reloads use AMI's `Reload` action**, not `ModuleLoad` with `LoadType
  reload`: on 22.11 the latter answers "Module not found." for `manager`,
  `logger` and `indications` (checked against the pinned image; the
  contract comments in `ami.go` and `compile.AMIClasses` now say so).
  `res_pjsip_notify` sends its own `Subscription-State: terminated` ahead
  of the agent's `active`; e2e sees both (§8 bench row).
- **intake**: a ConfigMap whose version is unreadable or not 64 hex digits
  is keyed `configmap@<resourceVersion>` and refused with version `""`.
- **apply**: a failure rolls back to the last *verified* version; applying
  the live version again writes and reloads nothing (it records the grants
  and the AMI account an adopted version lacks); a first version that
  fails verification is retried once per new AMI session; rollback
  reloads retry on `ErrNotConnected` up to the verify timeout.
  `Grants()` is `("", nil)` for an adopted version until intake hands it
  over again, so device HTTP never prunes spends against an empty list.
  A real verify mismatch retries for the whole 30 s, holding intake's one
  worker (by design; e2e `TestRollback` waits for it).
- **ami**: a session that ends within 5 s backs off before the next login;
  a call without a deadline gets 30 s; "No endpoints found" and "No
  Contacts found" are empty lists.
- **observe**: a SEP/CTL/ITL file of a MAC no device has is attributed to
  nobody (never by source address); the registered-endpoints metric counts
  distinct contact owners; the fetch socket refuses a non-socket at its
  path and holds at most 8 connections.
- **actions**: the AOR fallback matches only a contact with no endpoint;
  of several, the one expiring last; a Call-ID with white space or control
  characters is refused like one with `{` or `}`.
- Open (core's report): a version swapped in while AMI was down that later
  fails verification counts as applied for intake (not retried until the
  ConfigMap or Secret changes); an AMI password change applied while AMI
  is down, or a rollback whose manager reload cannot run, needs an Asterisk
  restart to heal.

**Files** (firmware, fileserver)
- Names are stricter: anything but printable ASCII without spaces is
  reported quoted and never looked up, and `NewManifest` refuses manifest
  names the file server would never serve; the drivers' manifests are also
  validated together.
- HTTP :6970 ignores `Range` and `If-*` and sends no `Last-Modified`, so
  "served" always means the whole file.
- At most 128 TFTP transfers at once (more get "server busy" and an error
  event); uploads answer 405 for anything but PUT, 400 for a broken body,
  500 for a store failure; a 413/422 reports whether a verified copy is
  already stored; files over `api.MaxFirmwareFileSize` are never hashed;
  the first upload empties `.incoming`; the digest cache fills on the first
  `Status` (NewDisk does no I/O).
- Open: TFTP shutdown waits for transfers in flight (up to about 30 s, the
  pod's grace period); the control API's 30 s read/write timeouts bound a
  firmware upload (a real 4.6 MB file over a port-forward must fit).

**Web** (devicehttp, scrape)
- Spends are pruned only against a *complete* grant list (every device of
  `v<version>/bundle.json` with a token), so device HTTP reads the index.
- A token sent to any other path (`/<token>`, `//p/<token>`, ...) is
  recorded as `p/` without a bundle path; `HEAD /p/...` is always the 404
  and never spends; failures other than "missing" answer 503 (only a token
  holder sees one); the state file is strict (a corrupt one fails start,
  saying when deleting it is safe).
- The scraper also refuses broadcast and zoned addresses, sanitises
  `WebScrape.Error`, gives each page its own 5 s, and exports the page
  types and `MarshalPage` for the W3 fakephone. It requires the root
  elements `DeviceInformation(X)`, `NetworkConfiguration(X)`,
  `DeviceLog(X)` (a bench check, §8).
- Tokens the controller mints must be URL-safe (the server compares the
  decoded path); W3.2's 32 alphanumerics are.

**Integrator** (cmd, sounds, chart, build, e2e)
- `patchbay agent` has one flag beyond 5.2.8, `--ami-address` (default
  127.0.0.1:5038). The agent installs the prompts first and reports a
  failure through `/readyz` rather than exiting; any component's error
  stops the whole agent (the container restarts).
- The file server is **65533:65533** (5.2.1), so e2e asserts that, not the
  65532 of the first plan. Its **bounding set** keeps `NET_BIND_SERVICE`,
  `SETUID` and `SETGID` (`CapBnd 00000000000004c0`): clearing it needs
  `CAP_SETPCAP`; with `no_new_privs` and a static image they cannot return.
  The agent's groups are `1000 65532` (the fsGroup and its own gid).
- `TestChartUsesTheLiveLayout` moved from `internal/compile` to the new
  `test/chart`, which renders the chart with `helm template` (helm from the
  flake) and checks every layout path, uid, flag, mount, the module list,
  the probes, the agent's credentials, the restart checksum, no
  persistence and the PodMonitor. A third ci values file
  (`no-persistence-values.yaml`) lints the other branches.
- The restart checksum (`checksum/asterisk-restart`) covers `pbx.address`,
  the SIP port and the module list; all three are also in the pod spec
  today, the checksum keeps that true once W3's settings feed the bundle.
  The file server has a TCP liveness probe on `pbx.address:6970` (a
  connection without a request is not a fetch).
- **e2e**: the controller is *not* scaled to 0: it has no reconcilers yet,
  so it cannot fight the harness, and `TestController` keeps checking its
  leader election. W3 must stop the harness compiling when its reconciler
  lands. Tests that call the control API many times (observations, the
  firmware upload, the resync action) use a **port-forward** from the
  harness (the path `patchbay firmware load` takes), not tools pods;
  `TestPBXUp` still checks the Service path from the pod network. The file
  server's file access is shown by a tools pod running as 65533:65533
  with the PBX pod's live emptyDir mounted from the node (hostPath),
  next to `/proc` evidence that the file server has exactly that identity.
  The compiled dialplan answers every number, so the old "unrouted number
  is 404" assess became `TestPrompts`' `199`. `TestRegister` registers the
  Softphone over UDP, whose contact Asterisk rewrites to the source port
  (`rewrite_contact = yes`).
- **Finding**: `pods/portforward` on the PBX pod (host network) reaches
  the node's loopback, so AMI too: the AMI password stays the barrier, as
  for any process on the node.

**W2 review fixes** (three read-only reviews of W2; each fix has a test
that fails without it). Where they differ from 5.2.1–5.2.9 and from the
stage notes above, these hold.
- **A version is applied only with its own secret values.** The
  controller writes pbx-secrets before pbx-config and the two informers
  deliver independently, so intake saw the live version's ConfigMap with
  the next version's Secret: it re-applied the live version with a
  rotated AMI password (the client then logged in with a password
  Asterisk did not have yet, and with AMI down the next version could
  never reload `manager`), and refused the live version when a device's
  keys left the Secret. `compile.Bundle.VersionWith(values)` recomputes
  the version from the files, the index and the values (`Compile` calls
  it, so the two cannot drift); intake compares it with the version. A
  Secret ahead of the version last applied is ignored until its
  ConfigMap follows; a new version waits `intake.Config.SecretGrace`
  (`DefaultSecretGrace`, 30 s) for its own Secret and is refused only if
  it never comes (a missing value and other values alike). Applying the
  live version again never replaces its known account or grants. W3's
  controller must write pbx-secrets with exactly the values it compiled
  with (extra keys are ignored).
- **Spends are pruned only against settled grants.** Device HTTP pruned
  against the serving grants: a version swapped in unverified (AMI down)
  dropped the spends of the tokens it rotated, and a failed verification
  then rolled back to the previous version, granting a spent token again
  (its document, SIP password included, was served twice).
  `apply.Applier.SettledGrants` are the live version's grants once it is
  verified, its tokens are known and it was not restored by a rollback;
  `devicehttp.Config.Settled` (nil: never prune) takes them through the
  new `provision.SourceFunc`. After a rollback nothing is pruned until a
  newer version settles, so a failed version applied again (after an
  agent restart) still finds its spends.
- **The adopted version is a rollback target.** After an agent restart the
  adopted version was never rolled back to, so a version that had failed
  and been rolled back before the restart was applied again and stayed
  live. Now a version that fails rolls back to the adopted one, verified
  again or not; verifying a version a rollback restored is not an apply
  (the failed version stays `LastApplyVersion`, with its error).
- **verify reports the mismatch**, not a check the deadline cut short
  (`verify: context deadline exceeded` failed TestVerifyMismatchRollsBack
  4 times in 30, and could fail e2e TestRollback).
- **Provisioning names never reach the fetch log from the file server.**
  A softphone's URL sent to `:6970` or asked for over TFTP (`p/<token>`)
  was reported with its token, attributed by source and served in the
  observations. `sensor.ProvisioningName` (an element `p`, any case,
  followed by another) is the one rule: the file server never looks such
  a name up and reports `p/`; the fetch socket also drops any event with
  such a File, a bundle path outside `tftp/`, or a provisioning document
  as File (a compromised file server could otherwise claim a softphone's
  provisioning fetch and force a token rotation).
- **Connection caps** (new package `internal/agent/netlimit`): device HTTP
  holds at most 256 connections, 32 per source address; HTTP :6970 128
  and 16. A connection over a cap is closed when accepted. Idle
  keep-alive connections close after 15 s (was 60 s). Without caps one
  host could hold thousands of connections and OOM the agent (128Mi) or
  the file server (64Mi).
- **Tests:** TestFetchSocket no longer orders events across two
  connections (it failed under load); e2e TestRollback writes a broken
  bundle with its own `VersionWith` version, and its teardown writes a new
  good version (the laptop's token rotated) so the last apply is a success
  again (writing the live version back changed nothing, and a reused
  cluster's TestPBXUp then saw the broken version's error);
  TestProvisioningOneTime rotates the laptop's token first (pbx-secrets,
  then pbx-config, as the controller will) and waits for it to apply, so
  it also passes on a reused cluster; TestFirmwareUploadAndServe expects
  200 for files a reused cluster's store already holds.
- Still open: a version swapped in while AMI was down that later fails
  verification counts as applied for intake (each agent restart applies
  it again and rolls it back); an AMI password change applied while the
  AMI *session* is down but Asterisk runs (the Secret-first order no
  longer causes it), or a rollback whose manager reload cannot run, needs
  an Asterisk restart to heal; the rolled-back-to adopted version's AMI
  account is unknown until intake hands that version over.

`go mod tidy`: `golang.org/x/sync` (errgroup) became a direct requirement.

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

### 6.1 Open decisions in W3, with recommendations (resolved in §6.2.1)

| Decision | Recommendation |
|---|---|
| Where `status` gets unknown phones | Port-forward to the agent like `firmware load`; no extra object. |
| Event target for discovery | The `pbx-config` ConfigMap (always exists, namespaced). |
| What a failed action records | Nothing: memory keeps the previous `LastAction`, so the rate limit counts only sent NOTIFYs. |
| Rollback e2e without production fault-injection | Split (§9): the agent half in e2e by writing a broken ConfigMap with the controller scaled to 0; the controller half in envtest with a fake agent document. |
| QR library | A small pure-Go one (`rsc.io/qr` or `github.com/mdp/qrterminal/v3`, pinned in go.mod); whichever renders in a plain terminal at 80 columns. |

### 6.2 W3 task level

W3 runs as **five stages**: **controller-core**, **controller-devices**,
**cli** and **fakephone** in parallel, in one working tree, each in its own
packages; then the **integrator** wires them into `internal/controller`,
`cmd/patchbay` and the chart, moves the e2e harness onto the real
controller and writes the §9 W3 e2e tests. The seams between them landed
first as contracts (`0a1c0f8` the dependency, `0d86131`, `133d7bc`,
`6ae9a64`, `2d81b5c`), each pinned by a test. **The contract code is
authoritative;** this section says what to build on it.

#### 6.2.1 Decisions (§6.1 resolved, and new ones)

| Decision | Resolution | Why |
|---|---|---|
| **Where `status` gets unknown phones** (§6.1) | **Confirmed:** `GET /v1/observations` through a port-forward to the agent, as `firmware load`. `status` prints the resources first; if the agent cannot be reached it prints `Unknown phones: could not ask the agent: <err>` and still exits 0. | Events are emitted once per MAC per agent start and expire after an hour, so they cannot list a phone that is still asking; the agent's document is the live truth, and the controller keeps no discovery state. `status` is an overview: a missing agent is a line in it, not a failure. |
| **Event target for discovery** (§6.1) | **Confirmed:** the `pbx-config` ConfigMap, also for `ConfigWritten` and `CompileFailed`; while `pbx-config` does not exist yet, `CompileFailed` goes to the chart's `<fullname>-settings` ConfigMap (`install` events.go). | Both always exist once the install runs; the settings are the usual cause of a first compile failing. |
| **What a failed action records** (§6.1) | **Confirmed, made exact:** on an `Actor` error, `Sent=false`, or a token Secret write that fails, the status write keeps the previous `lastAction` **and** `handledRequest` (a requested resync stays pending and is tried again), writes the rest of the `StatusPatch` (the fold), emits `ActionFailed` (Warning) and requeues in 30 s. Later actions of the same Decision are not attempted. | The rate limit counts only NOTIFYs that went out; consuming a human's request on a transient failure would lose it. |
| **Rollback e2e** (§6.1) | **Confirmed, changed in one place:** the agent half in e2e with the controller scaled to 0; scaled back up, the controller rewrites its own version, and the test then reissues the laptop's token (a new version) because the agent does not clear its last apply error when the live version is written back (5.2.11). No device may show `ApplyFailed` for the broken version, which was never compiled. The controller half is envtest (controller-devices). | |
| **QR library** (§6.1) | **`rsc.io/qr` v0.2.0** (BSD-3, pure Go, no dependencies; `0a1c0f8`), drawn by our own renderer: half blocks (`▀`, one column per module, two module rows per line) with explicit ANSI black on bright white, level M, quiet zone 4. A provisioning URL (~60 bytes) is version 4: 33 modules + 8 = 41 columns, 21 lines. | `mdp/qrterminal` wraps the same encoder and adds `x/term`; explicit colours render the same on dark and light terminals; the renderer is ~30 lines and its output decodes back to the module grid in a unit test. |
| **Settings** (W3.1) | The chart renders `<fullname>-settings` (key `settings.json`); the controller reads the **mounted file** once at start (`settings.Load`: strict JSON, `Validate`), then `install.CheckSettings` (its `amiSecret` must be `<fullname>-ami/password`). A checksum annotation over the JSON restarts the controller on change. | No RBAC, no watch, no partial reload: a settings change rolls one stateless pod. |
| **How the device reconcilers read the compiled index** | From the cluster, every reconcile: `install.ReadCompiled` reads `pbx-config` (`bundle.json` only, `IndexOf`) and `pbx-secrets` from the cache. No component hands compiled state to another in memory. | The stateless option: it survives a controller restart without a warm-up, it is exactly what the agent and the CLI see, and the cost is decoding a few KB of JSON from an informer cache. An in-memory index would be empty after a restart until the bundle reconciler ran, and could drift from what was written. |
| **The provisioning URL shows the compiled token** | `status.provisioningURL` uses `Compiled.ProvisioningToken`: `pbx-secrets`' value, only while its `bundle-version` annotation equals `pbx-config`'s version; otherwise the previous URL stays. **Amended by the W3 review (6.2.12):** only while the Decision's state is `Issued` (design §8.4), the Decision does not rotate the token, and the device Secret still holds the compiled token; `""` otherwise. | The device Secret changes before the bundle is recompiled; showing its token at once would publish a URL the agent does not serve yet. `qr --reissue` additionally waits until `ConfigCurrent` is not `ApplyPending` (the bundle is live). |
| **Gating** | A Phone or Softphone reconciler writes no status before the poller's first attempt has ended (`Snapshot.Started`), or while the device is not in the compiled index; no reconciler (Person and Stream too) writes status while `pbx-config` does not exist (`ErrNotCompiled`). Each of those ends with an event that enqueues it (the poller enqueues every device after its first attempt; the `pbx-config` watch enqueues all). | A restarted controller must not flip every phone to Unknown for one poll; a device decided before it is compiled would decide on an empty `ConfigVersion`. |
| **No status hot loop** (W1 carry-over) | Status is written only when it differs (`equality.Semantic.DeepEqual` on the whole status); `For` watches use `predicate.Or(GenerationChangedPredicate, AnnotationChangedPredicate)`, so the controller's own status writes never enqueue. | |
| **First reconcile** | The bundle reconciler enqueues its singleton once when it starts (`source.Func`), besides its watches. | A fresh install with no resources still mints the agent token and writes a bundle (Asterisk with no devices), so the PBX pod starts. |
| **Secret writers** | The bundle reconciler (through `secrets.Manager.Ensure`) creates every Secret and fills a missing or empty key, never changes a non-empty one, and adopts a device Secret whose controller reference is missing or names another UID (the values stay). The softphone reconciler's `rotateToken` is the only other writer: it updates one key (`provisioning-token`) of an existing Secret, and treats a missing Secret as a failed action. | One creator; a recreated Phone keeps its password; GC removes a deleted resource's Secret by its ownerReference. |
| **Agent token** | Minted by the controller (`install.GenerateAgentToken`: 32 random bytes, unpadded base64url) into `<fullname>-agent-token`; the chart drops `pbx.agent.tokenSecretName` and its "create it before installing" note. The PBX pod waits in ContainerCreating until it exists (the kubelet retries the mount). | Design §4. Overriding the name would need a matching controller flag for no benefit. |
| **Metrics** (W3.8) | `patchbay_devices{kind,state}` (from the cache at scrape; state `""` is `Pending`), `patchbay_registered_endpoints`, `patchbay_config_version_info{version}`, `patchbay_apply_failures_total` (each distinct `LastApplyVersion`+`LastApplyAt` with an error, once), `patchbay_observation_age_seconds` (absent before the first success); added: `patchbay_compile_failures_total` and `patchbay_agent_reachable`. Tested through `promhttp.HandlerFor` text. | The added two back W3.3's "metric" and design §10's "agent unreachable" alert. `prometheus/testutil` would add a module (`kylelemons/godebug`). |
| **envtest** | Only `bundle`, `devices` and `numbers` (and the existing `internal/controller` root and `ctrltest`) start an API server; every other package uses the controller-runtime fake client. | Each envtest package runs its own kube-apiserver and etcd under `go test ./...`; five at once fit the workstation. |
| **Control API connection cap** (W2 carry-over) | `agent.Serve` wraps its listener in `netlimit` (64 connections, 16 per source) (integrator). | Same reason as device HTTP: one host could otherwise exhaust the agent's 128 MiB. The controller keeps one or two keep-alive connections; the CLI's port-forward arrives from 127.0.0.1 one request at a time. |
| **Fake phones in e2e** | `fakephone run` executes a `script.Script` (cisco or linphone mode); the harness drives it with signals and reads its `State` over the phone's own HTTP server through `pods/proxy`; the pod runs with the safe sysctl `net.ipv4.ip_unprivileged_port_start=0` so the server binds :80, where the agent scrapes. | A long-lived phone that answers NOTIFYs and calls is what the loop tests need; signals and state need nothing in the image and no port-forward per phone. |

#### 6.2.2 Carry-over → owner

| From | Item | Closed by |
|---|---|---|
| W1 | Claims come from `compile.ResolveClaims`, never re-derived | devices and numbers call `ResolveClaims` over every resource from the cache, `claims.Apply(&dev)`; `TestClaimsFromResolveClaims` (devices), `TestPersonNumberConflict` (numbers) |
| W1 | Write status only when it changed | devices and numbers (6.2.1 "No status hot loop"); `TestStatusWrittenOnlyWhenChanged` in each |
| W1 | `TestPhoneLifecycle` includes the Desk phone's factory-reset path | integrator: `TestPhoneLifecycle/reset` (6.2.9). In e2e the human action is **P6**, not P5: P5 needs the *default* load staged, and its digests are pending, so no e2e can stage it; the driver's `TestLifecycle` pins P5. Everything else on the row is asserted (never NeedsTrustReset again, Upgrading, Registered with FirmwareCurrent Unknown and no resync). |
| W1 | Linphone bench items stay §8's | nobody: e2e proves only that the fake reads the keys the renderer writes; §8 records nothing as answered |
| W2 | The controller writes `pbx-secrets` (exactly `SecretRefs`, the compiled values) before `pbx-config`, and mints the agent token | core: `bundle`, `secrets`; `install.ProjectedSecret` refuses values that do not give the bundle's version; `TestWritesSecretsThenConfigMap` |
| W2 | `test/e2e/bundle_test.go` (`applyBundle`) deleted | integrator |
| W2 | fakephone `run` mode, fake web pages from `scrape`'s page types, Linphone mode, blind transfer | fakephone stage (6.2.8) |
| W2 | `patchbay firmware load` and `status` | cli stage; e2e `TestPhoneLifecycle/firmware-load`, `TestDiscovery` (integrator) |
| W2 | After an agent restart and a rollback to the adopted version, its AMI account is unknown until intake hands it over again | **deferred to §11** (symptom and remedy there): it needs an agent restart *and* a version that fails verification, which the controller's compiled bundles do not produce in normal operation; the fix (parse the adopted `manager.conf`) belongs in a W2 package no W3 stage owns |
| W2 | An AMI password change applied while AMI is down needs an Asterisk restart | **deferred to §11**: only a deleted `<fullname>-ami` Secret coinciding with an AMI outage causes it |
| W2 | A version swapped in while AMI was down that later fails verification counts as applied for intake | **deferred to §11**: the failure is visible (`ApplyFailed` on every device) and any new version heals it |
| W2 | The control API (:9440) has no connection cap | integrator: `netlimit` in `agent.Serve`, `TestServeCapsConnections` |
| W2 | e2e on a reused cluster keeps the PBX pod (restart counts, the last apply) | integrator: every W3 test deletes and recreates its own resources at start, discovery uses a random MAC, assertions compare with what the test saw first, and `TestRollback` leaves a successful last apply |

#### 6.2.3 Package map and ownership

```text
internal/controller/install/      contract (133d7bc)                             nobody
internal/controller/agentview/    contract (133d7bc)                             nobody
internal/controller/ctrltest/     contract: envtest harness (6ae9a64)            nobody
internal/controller/settings/     W3.1                                           core
internal/controller/secrets/      W3.2                                           core
internal/controller/bundle/       W3.3                                           core
internal/controller/agentclient/  W3.4: control API client                       core
internal/controller/poller/       W3.4                                           core
internal/controller/metrics/      W3.8                                           core
internal/controller/devices/      W3.5                                           devices
internal/controller/numbers/      W3.6                                           devices
internal/controller/discovery/    W3.7                                           devices
internal/cli/{doc,env,env_test}.go contract (0a1c0f8, 2d81b5c)                   nobody
internal/cli/ (every other file)  W3.9                                           cli
test/e2e/fakephone/script/        contract (2d81b5c)                             nobody
test/e2e/fakephone/ (the rest, Dockerfile, Dockerfile.dockerignore)  W3.10       fakephone
internal/controller/*.go (the root package), cmd/patchbay/,
internal/agent/server.go (+ its test), charts/, test/chart/, test/e2e/*.go,
test/e2e/values.yaml, test/e2e/kind.yaml, Makefile, Dockerfile, .github/,
docs/, CLAUDE.md, README.md                                                      integrator
```

**Rules for the parallel stages** (core, devices, cli, fakephone):

1. Write only your own packages and files. Never edit a landed contract
   file (`internal/controller/{install,agentview,ctrltest}/*`,
   `internal/cli/{doc,env,env_test}.go`, `test/e2e/fakephone/script/*`) or
   anything under `api/`, `internal/compile`, `internal/drivers`,
   `internal/agent`, the root of `internal/controller`, `cmd`, `charts`,
   `test/chart`, `test/e2e/*.go`, `docs`, the Makefile, Dockerfiles other
   than fakephone's, `flake.*`, `go.mod`, `go.sum` or CLAUDE.md. If a
   contract is wrong, work around it inside your package and report the
   deviation; the integrator reconciles.
2. Import your own stage's packages, the contract packages and
   pre-existing ones (`api/v1alpha1`, `internal/compile`,
   `internal/drivers`, `internal/drivers/builtin`, `internal/agent/api`,
   `internal/agent/layout`, `internal/version`; the fakephone also
   `internal/agent/scrape`); never another stage's package, never
   `internal/controller` itself (it will import yours) or `cmd`.
   Interfaces you consume are in the contracts; the integrator adds the
   compile-time assertions that the other stages' types satisfy them.
3. No new modules: no `go get`, no `go mod tidy`. Everything W3 needs is
   required already (client-go `tools/portforward`, `transport/spdy`,
   `tools/events`, `tools/clientcmd`; controller-runtime `client/fake`,
   `client/interceptor`, `envtest`, `source`; `k8s.io/utils/clock` and
   `clock/testing`; `rsc.io/qr`; sipgo; pin/tftp). Not
   `prometheus/client_golang/prometheus/testutil` (it adds a module):
   test metrics through `promhttp.HandlerFor`'s text, as
   `internal/agent/metrics` does.
4. Verify your own packages only; `make fmt` reformats everyone's files.
   With `P` your packages:
   `nix develop -c golangci-lint fmt $P`, `nix develop -c go vet $P`,
   `nix develop -c go vet -tags e2e $P`, `nix develop -c golangci-lint run $P`,
   `nix develop -c go test -race -count=1 $P` and
   `nix develop -c go test -race -count=1 -short -tags e2e $P`. At the end
   run `nix develop -c make vet lint test-unit` once and report (never
   fix) a failure in another stage's package.
5. TDD, small commits, `git add` your own paths only, message body ending
   with the `Co-Authored-By` line. No cluster: only the integrator runs
   e2e. envtest only in `bundle`, `devices`, `numbers` (through
   `ctrltest.Main`); everything else uses the controller-runtime fake
   client, loopback listeners on port 0, `t.TempDir()` and fakes.
6. Never log, print, put in an Event or an error a secret value, a token
   or a provisioning URL (a test that captures the logr output asserts
   it where one is handled). The one exception is `patchbay softphone qr`,
   whose output is the URL it was asked for.
7. No test loads a kubeconfig. A CLI test that reaches `NewEnv` passes a
   `--kubeconfig` path that does not exist and sets `KUBECONFIG` and
   `HOME` to `t.TempDir()`.
8. Coverage ≥ 80% per package (`go test -cover`).

#### 6.2.4 Contracts landed (read the code; summary)

```go
// internal/controller/install (133d7bc)
type Names struct{ Fullname, ConfigMap, ProjectedSecret, AgentTokenSecret, AMISecret, Settings, AgentService string }
func NewNames(fullname string) Names            // <f>-pbx-config, -pbx-secrets, -agent-token, -ami, -settings, -agent
func (n Names) Validate() error; AMIRef() compile.SecretRef; AgentTokenRef() compile.SecretRef
const MaxFullnameLength = 50; AgentTokenKey = "token"; AMIPasswordKey = "password"; SettingsKey = "settings.json"
func CheckSettings(s compile.Settings, n Names) error
const LabelPartOf, PartOf, LabelManagedBy, ManagedBy = "app.kubernetes.io/part-of", "patchbay", "app.kubernetes.io/managed-by", "patchbay-controller"
const AnnotationBundleVersion = "patchbay.chrismiller.xyz/bundle-version"
func ManagedLabels() map[string]string
const PollInterval = 5 * time.Second; StaleAfter = 30 * time.Second
func DecideSettings(s compile.Settings) drivers.DecideSettings
func DeviceHTTPURL(s compile.Settings) string                  // http://<pbx>:<deviceHTTPPort>
func ProvisioningURL(s compile.Settings, token string) string  // + "/p/" + token
func AgentURL(n Names, namespace string, port int) string      // http://<f>-agent.<ns>.svc:<port>
var ErrNotCompiled error
func ConfigMap(n Names, namespace string, b compile.Bundle) (*corev1.ConfigMap, error)
func ProjectedSecret(n Names, namespace string, b compile.Bundle, values map[compile.SecretRef][]byte) (*corev1.Secret, error)
func IndexOf(cm *corev1.ConfigMap) (compile.Index, error)
func SecretValues(s *corev1.Secret) (map[compile.SecretRef][]byte, error)
type Compiled struct{ Index compile.Index; ConfigMap *corev1.ConfigMap; Secrets map[compile.SecretRef][]byte }
func ReadCompiled(ctx context.Context, c client.Reader, namespace string, n Names) (Compiled, error)
func (c Compiled) Device(id string) (compile.BundleDevice, bool)
func (c Compiled) ProvisioningToken(deviceID string) (string, bool)
const ProvisioningTokenLength = 32; AMIPasswordLength = 32; AgentTokenBytes = 32
func Generate(r io.Reader, length int, alphabet string) (string, error)   // r nil: crypto/rand
func GeneratePassword(r io.Reader, p drivers.PasswordPolicy) (string, error)
func GenerateProvisioningToken(r io.Reader) (string, error)
func GenerateAMIPassword(r io.Reader) (string, error)
func GenerateAgentToken(r io.Reader) (string, error)
const EventRecorderName = "patchbay-controller"
const EventReasonStateChanged, EventReasonActionSent, EventReasonActionFailed, EventReasonRequestIgnored,
      EventReasonApplyFailed, EventReasonUnknownPhone, EventReasonConfigWritten, EventReasonCompileFailed
const EventActionDecide, EventActionResync, EventActionRestart, EventActionRotateToken, EventActionDiscover, EventActionCompile
func UnknownPhoneMessage(mac, source string) string   // "unknown phone <MAC> requested config from <IP>"

// internal/controller/agentview (133d7bc)
type Snapshot struct{ Document agentapi.Observations; PolledAt time.Time; AgentReachable bool; Polls uint64; LastError string }
func (s Snapshot) Started() bool
func (s Snapshot) Device(id string) *agentapi.DeviceObservations
func (s Snapshot) For(deviceID, compiledVersion string) drivers.Observations
type Observer interface{ Snapshot() Snapshot }
type Holder struct{ /* RWMutex */ }; func (h *Holder) Snapshot() Snapshot; func (h *Holder) Set(s Snapshot)
type SnapshotFunc func(ctx context.Context, s Snapshot)
type Actor interface {
    Resync(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error)
    Restart(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error)
}
func ActionFailure(resp agentapi.ActionResponse, err error) string
const DefaultChangesBuffer = 1024
type Changes struct{ Phones chan event.TypedGenericEvent[*v1alpha1.Phone]; Softphones chan event.TypedGenericEvent[*v1alpha1.Softphone] }
func NewChanges(buffer int) Changes
func (c Changes) Notify(ctx context.Context, kind drivers.DeviceKind, namespace, name string) error

// internal/controller/ctrltest (6ae9a64), _test.go files only
type Env struct{ Config *rest.Config; Client client.Client; Scheme *runtime.Scheme }
func Main(m *testing.M, env **Env) int; Need(t, env); Namespace(t, env) string
func NewScheme() (*runtime.Scheme, error); CRDDir() (string, error)
func NewManager(t, env, namespace string) ctrl.Manager; StartManager(t, mgr)

// internal/cli (2d81b5c): env.go
type Env struct{ Kube client.Client; Namespace string; Agent AgentOpener; Registry *drivers.Registry; Clock clock.Clock; Out, Err io.Writer }
type ClientFlags struct{ Namespace, Instance string }
type AgentOpener interface{ OpenAgent(ctx context.Context) (*AgentConn, error) }
type AgentConn struct{ BaseURL, Token string; HTTP *http.Client; Close func() error }  // String/GoString redact Token
const LabelPartOf, PartOf, LabelComponent, ComponentPBX, LabelInstance, AgentContainer, ControlPortName, AgentTokenVolume

// test/e2e/fakephone/script (2d81b5c)
const EnvScript = "FAKEPHONE_SCRIPT"; PathState = "/fakephone/state"; PathSignalPrefix = "/fakephone/signal/"
type Script struct{ Mode; Server, MAC, ProvisioningURL, WebListen string; Web *Web; Answer *Answer; ToneHz float64; UserAgent string; Steps []Step; ExitWhenDone bool }
type Step struct{ Label string; FetchConfig, Fetch, SetWeb, Register, Unregister, StopRefreshing, Call, Hangup, Transfer, WaitNotify, WaitCall, WaitSignal, Sleep *… }
type State struct{ Mode; Step int; Label string; Done, Registered bool; Config *Config; Web Web; Fetches, Registrations, Notifies, Calls, Signals [] }
func Parse([]byte) (Script, error); (Script) Validate() error; (Script) Encode() (string, error); NewState(Mode) State
func Expand(file, mac, load string) string; SignalPath(name string) string; HzKey(hz float64) string

// api/v1alpha1 (0d86131)
const ReasonNumberConflict, ReasonNoConflict, ReasonRouted, ReasonMissingRingTargets, ReasonNoStreamBridge
```

#### 6.2.5 Stage controller-core: W3.1, W3.2, W3.3, W3.4, W3.8

Exported API (exact; the integrator wires these):

```go
// internal/controller/settings (W3.1)
func Load(path string) (compile.Settings, error) // reads the mounted settings.json, then Parse
func Parse(raw []byte) (compile.Settings, error) // strict JSON (unknown field, trailing data: error), then Validate

// internal/controller/secrets (W3.2)
type Config struct {
    Client    client.Client     // the manager's client: cached reads, writes to the API server
    Namespace string
    Names     install.Names
    Registry  *drivers.Registry // each model's Provisioning.Password policy
    Rand      io.Reader         // nil: crypto/rand.Reader
    Log       logr.Logger
}
type Manager struct{ /* … */ }
func New(cfg Config) (*Manager, error)
// Ensure creates or completes every Secret the install needs and returns
// the values Compile reads (compile.Inputs.Secrets): PasswordRef of every
// Phone and Softphone whose model a driver serves, ProvisioningTokenRef of
// every Softphone, and the AMI password. It also ensures the agent token.
func (m *Manager) Ensure(ctx context.Context, phones []v1alpha1.Phone, softphones []v1alpha1.Softphone) (map[compile.SecretRef][]byte, error)
// Tokens implements agentclient.TokenSource: the agent token in the cache
// and the one it replaced, if this process saw it change ("" otherwise).
func (m *Manager) Tokens(ctx context.Context) (current, previous string, err error)

// internal/controller/bundle (W3.3)
type Config struct {
    Client    client.Client
    Namespace string
    Names     install.Names
    Settings  compile.Settings
    Registry  *drivers.Registry
    Secrets   *secrets.Manager
    Recorder  events.EventRecorder // k8s.io/client-go/tools/events
    Metrics   *metrics.Metrics     // nil: none
    Log       logr.Logger
}
type Reconciler struct{ /* … */ }
func New(cfg Config) (*Reconciler, error)
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error

// internal/controller/agentclient (W3.4)
type TokenSource interface {
    Tokens(ctx context.Context) (current, previous string, err error)
}
type Config struct {
    URL    string       // install.AgentURL(names, namespace, api.DefaultPort)
    Tokens TokenSource
    HTTP   *http.Client // nil: no proxy, no redirects, a 5 s timeout per request
    Log    logr.Logger
}
type Client struct{ /* … */ }
func New(cfg Config) (*Client, error)
func (c *Client) Observations(ctx context.Context) (agentapi.Observations, error)
func (c *Client) Resync(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error)
func (c *Client) Restart(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error)

// internal/controller/poller (W3.4)
type Source interface {
    Observations(ctx context.Context) (agentapi.Observations, error)
}
type Config struct {
    Source     Source
    Reader     client.Reader // the manager's cached client (install.ReadCompiled)
    Namespace  string
    Names      install.Names
    Interval   time.Duration // 0: install.PollInterval
    Holder     *agentview.Holder
    Changes    agentview.Changes
    OnSnapshot []agentview.SnapshotFunc
    Metrics    *metrics.Metrics // nil: none
    Now        func() time.Time // nil: time.Now
    Log        logr.Logger
}
type Poller struct{ /* … */ }
func New(cfg Config) (*Poller, error)
func (p *Poller) Start(ctx context.Context) error // manager.Runnable: Poll now, then every Interval, until ctx ends (nil)
func (p *Poller) NeedLeaderElection() bool       // true
func (p *Poller) Poll(ctx context.Context)       // one attempt (exported for tests)
func (p *Poller) Snapshot() agentview.Snapshot   // agentview.Observer: Holder.Snapshot()

// internal/controller/metrics (W3.8); every method is a no-op on a nil *Metrics
type Config struct {
    Registerer prometheus.Registerer // controller-runtime's metrics.Registry in production
    Reader     client.Reader         // patchbay_devices, at scrape
    Namespace  string
    Observer   agentview.Observer    // registered endpoints, observation age, agent reachable
    Now        func() time.Time
}
type Metrics struct{ /* … */ }
func New(cfg Config) (*Metrics, error)
func (m *Metrics) CompileFailed()
func (m *Metrics) ConfigWritten(version string)        // config_version_info{version}=1, the previous series removed
func (m *Metrics) ObserveSnapshot(s agentview.Snapshot) // apply failures, counted once per (LastApplyVersion, LastApplyAt)
```

**W3.1 settings.** `Parse` decodes into `compile.Settings` with
`DisallowUnknownFields`, refuses trailing data, then `Validate`. The
integrator calls `install.CheckSettings` after `Load`. *Tests:*
`TestParseChartDefaults` (the JSON the chart must render with default
values, as a literal fixture: test/chart checks the chart against the same
document), `TestParseRejectsUnknownField`, `TestParseRejectsTrailingData`,
`TestParseValidates` (a bad port names the field), `TestLoad` (a file;
a missing file's error names the path).

**W3.2 secrets.** `Ensure`:
- per Phone and Softphone: ID `drivers.DeviceID(kind, name)`, Secret
  `compile.DeviceSecretName(id)`, labels `install.ManagedLabels()`, owner
  `controllerutil.SetControllerReference` (controller, blockOwnerDeletion;
  RBAC already grants `*/finalizers`). Keys: `sip-password` from
  `install.GeneratePassword` with the model's
  `Profile.Provisioning.Password` (a model no driver serves gets no
  password and no error: it is never rendered); Softphones also
  `provisioning-token` (`GenerateProvisioningToken`);
- `<fullname>-ami` (`password`, `GenerateAMIPassword`) and
  `<fullname>-agent-token` (`token`, `GenerateAgentToken`): no owner;
- create when missing; when present, add a missing or empty key, never
  change a non-empty one, and adopt (set the controller reference) when it
  is missing or names another UID; a Create that hits AlreadyExists (the
  cache lags) returns an error, so the next reconcile reads it;
- returns the stored values (never logs one).

`Tokens` reads the agent-token Secret from the cache; when its value
differs from the last one returned, that one becomes `previous`. Missing
Secret: an error. *Tests (fake client, `interceptor.Funcs` counting
writes):* `TestEnsureCreatesDeviceSecrets` (lengths 12, 24 + 32,
alphanumerics, labels, owner fields, returned = stored),
`TestEnsureWritesNothingWhenComplete`, `TestEnsureFillsMissingKey`,
`TestEnsureRegeneratesDeleted` (a new value), `TestEnsureAdopts` (value
kept), `TestInstallSecrets` (AMI 32 alphanumerics, token 43 base64url
characters, no owner), `TestTokens` (current, then previous after a
change, error when missing), `TestUnservedModelGetsNoPassword`,
`TestNoSecretLogged` (a logr sink capturing everything; no value
appears).

**W3.3 bundle.** One reconciler, singleton key `{Namespace,
Names.ConfigMap}` (every request maps to it; `MaxConcurrentReconciles`
1). `Reconcile`:
1. list Phones, Softphones, People, Streams (cache);
2. `values, err := Secrets.Ensure(phones, softphones)`;
3. `b, err := compile.Compile(Registry, compile.Inputs{…, Secrets: values}, Settings)`;
   then `install.ProjectedSecret(…, b, values)` and `install.ConfigMap(…, b)`;
   a compile or encoding error (`Compile`, `ErrBundleTooLarge`, the two
   builders) → `CompileFailed` Event (Warning, action `Compile`, the error
   as note) on `pbx-config`, or the settings ConfigMap while it does not
   exist (only a log line if neither does), `Metrics.CompileFailed()`, the
   old bundle untouched, `RequeueAfter: 5m` and no error (it needs a
   resource change); an API error (listing, `Ensure`, a write) is returned,
   so the reconcile is retried with backoff;
4. read the current `pbx-secrets` and `pbx-config` (cache). Write
   `pbx-secrets` (create, or update data, labels and annotation) unless its
   data and `bundle-version` already match; **then** write `pbx-config`
   unless its index version is `b.Version` and its annotation matches.
   Nothing written → nothing else happens (no Event). Written →
   `ConfigWritten` (Normal) on `pbx-config` ("version <v[:12]> written: <n>
   files, <d> devices"), `Metrics.ConfigWritten(v)`.
5. an Update conflict returns the error (requeue).

Watches (`SetupWithManager`, `Named("bundle")`): Phone, Softphone, Person,
Stream with `GenerationChangedPredicate` (create and delete always pass);
Secrets that are owned by a Phone or Softphone of this API group or named
`AMISecret`, `AgentTokenSecret` or `ProjectedSecret`; the ConfigMap named
`ConfigMap` (an edit or delete is rewritten); every event maps to the
singleton. Plus `source.Func` enqueueing the singleton once at start.
*Tests (envtest, `ctrltest`):* `TestWritesSecretsThenConfigMap` (an
`interceptor` client records writes: `pbx-secrets` before `pbx-config`;
keys exactly `SecretRefs`' projected keys; `VersionWith(projected) ==`
the index version; annotations; labels), `TestNoWritesWhenUnchanged` (a
second reconcile writes nothing), `TestResourceChangeRecompiles` (a label
patch: new version, Secret first again), `TestDeletedDeviceSecretHeals`
(new password, new version, projected value = new),
`TestEditedConfigMapRewritten`, `TestCompileErrorKeepsOldBundle` (a
registry whose `cisco-7945` driver's Render fails: `CompileFailed` on a
FakeRecorder, the metric, `pbx-config` unchanged),
`TestFirstReconcileWithNoResources` (the agent token, the AMI Secret and
a bundle with no devices), `TestWatchesTrigger` (a manager: a new Phone
reaches `pbx-config`; deleting its Secret makes a new version).

**W3.4 agentclient.** Every request: `Authorization: Bearer <current>`;
on 401 retry once with `previous` when it is non-empty and different; a
non-200 is an error quoting the status and the agent's `api.Error` (never
the token); bodies limited (observations 16 MiB, actions 64 KiB); action
requests time out at 10 s, observations at 5 s; no redirects. *Tests
(httptest):* bearer header; 401 then 200 with the previous token; 401
twice → error; 500 → error with the message; actions decode `Sent=false`
with a nil error; 400 → error; a slow server times out; `var _
agentview.Actor = (*Client)(nil)`; no token in any error or log.

**W3.4 poller.** `Poll`:
1. `doc, err := Source.Observations(ctx)`; `now := Now()`;
2. next snapshot: success → `{doc, now, true, Polls+1, ""}`; failure →
   `{previous Document, previous PolledAt, false, Polls+1, err.Error()}`;
   `Holder.Set(next)`; `Metrics.ObserveSnapshot(next)`;
3. what to enqueue: the *agent fingerprint* is a hash of
   `AgentReachable`, `doc.Agent` (all fields) and `doc.Firmware`; each
   device's fingerprint a hash of its `DeviceObservations` JSON. First
   attempt, or a changed agent fingerprint → every device; otherwise each
   device whose fingerprint appeared, changed or disappeared;
4. devices are named through `install.ReadCompiled(Reader, …)`: an ID in
   the index → `Changes.Notify(ctx, kind, Namespace, name)`; an ID not in
   it is skipped; `ErrNotCompiled` → nothing enqueued, not an error;
5. after a success only, each `OnSnapshot(ctx, next)` in order.

Fingerprints are the poller's memory (disposable). *Tests (fake Source,
fake client holding a compiled `pbx-config` built with `install.ConfigMap`,
`Changes` read in the test):* `TestFirstPollEnqueuesAll`,
`TestUnchangedEnqueuesNothing`, `TestDeviceChangeEnqueuesOne`,
`TestAgentChangeEnqueuesAll` (AsteriskUp, AppliedVersion, a firmware
file), `TestFailureKeepsDocument` (AgentReachable false, document and
PolledAt kept, LastError without the token, all enqueued once, a second
failure enqueues nothing), `TestRecoveryEnqueuesAll`,
`TestOnSnapshotOnSuccessOnly`, `TestUnknownIDSkipped`,
`TestNotCompiled`, `TestStartPollsAtOnceAndStops`,
`TestNeedsLeaderElection`.

**W3.8 metrics** as 6.2.1. *Tests:* every name and label set in the
`promhttp` text; `patchbay_devices` from a fake client's Phones and
Softphones (state "" → `Pending`); age absent before the first success;
apply failures counted once per distinct failure; `ConfigWritten` drops
the previous version's series; nil-receiver calls do nothing.

**Core acceptance:** `P="./internal/controller/settings/... ./internal/controller/secrets/... ./internal/controller/bundle/... ./internal/controller/agentclient/... ./internal/controller/poller/... ./internal/controller/metrics/..."`, the commands of rule 4; coverage ≥ 80% each.

#### 6.2.6 Stage controller-devices: W3.5, W3.6, W3.7

```go
// internal/controller/devices (W3.5)
type Config struct {
    Client    client.Client        // the manager's client
    Namespace string
    Names     install.Names
    Settings  compile.Settings
    Registry  *drivers.Registry
    Observer  agentview.Observer
    Actor     agentview.Actor
    Changes   agentview.Changes
    Recorder  events.EventRecorder
    Rand      io.Reader            // nil: crypto/rand.Reader (rotateToken)
    Now       func() time.Time     // nil: time.Now
    Log       logr.Logger
}
func Setup(mgr ctrl.Manager, cfg Config) error // both reconcilers
type PhoneReconciler struct{ /* … */ }
func NewPhoneReconciler(cfg Config) (*PhoneReconciler, error)
func (r *PhoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
type SoftphoneReconciler struct{ /* … */ }
func NewSoftphoneReconciler(cfg Config) (*SoftphoneReconciler, error)
func (r *SoftphoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
const ActionRetryAfter = 30 * time.Second

// internal/controller/numbers (W3.6)
type Config struct {
    Client    client.Client
    Namespace string
    Names     install.Names
    Settings  compile.Settings
    Recorder  events.EventRecorder
    Now       func() time.Time
    Log       logr.Logger
}
func Setup(mgr ctrl.Manager, cfg Config) error
type PersonReconciler struct{ /* … */ }; func NewPersonReconciler(cfg Config) (*PersonReconciler, error)
type StreamReconciler struct{ /* … */ }; func NewStreamReconciler(cfg Config) (*StreamReconciler, error)
// each with Reconcile(ctx, req) (ctrl.Result, error)

// internal/controller/discovery (W3.7)
type Config struct {
    Reader    client.Reader
    Namespace string
    Names     install.Names
    Recorder  events.EventRecorder
    Log       logr.Logger
}
type Reporter struct{ /* … */ }
func New(cfg Config) *Reporter
func (r *Reporter) Report(ctx context.Context, s agentview.Snapshot) // an agentview.SnapshotFunc
```

**W3.5 Phone and Softphone reconcilers.** `Reconcile`:
1. get the resource (cache); NotFound → done;
2. `snap := Observer.Snapshot()`; `!snap.Started()` → done, no status;
3. `c, err := install.ReadCompiled(…)`; `ErrNotCompiled` → done; `bd, ok
   := c.Device(id)`; `!ok` → done;
4. list every Phone, Softphone, Person and Stream; `claims :=
   compile.ResolveClaims(inputs, Settings.NumberPlan)`;
5. `dev := drivers.PhoneDevice(p)` (`SoftphoneDevice`); `claims.Apply(&dev)`;
   `dev.ConfigVersion = bd.Version`; the driver is
   `Registry.ForModel(dev.Model)` (no driver: log and stop; the CRD enum
   admits only served models);
6. `d := drv.Decide(dev, snap.For(id, c.Index.Version),
   install.DecideSettings(Settings), now)`;
7. actions in order: `resync` → `Actor.Resync({Device: id, Version:
   a.Version})`, `restart` → `Actor.Restart`, `rotateToken` → a new
   `install.GenerateProvisioningToken(Rand)` written to the existing
   device Secret's `provisioning-token` (Update, the Secret from the
   cache; missing Secret = failure). Success → `ActionSent`; failure
   (`agentview.ActionFailure != ""`, or the write's error) → the 6.2.1
   rule, `ActionFailed`, `RequeueAfter = ActionRetryAfter` (or the
   Decision's, if sooner), stop;
8. the new status, from the old: `State = d.State`; `ObservedGeneration
   = metadata.generation`; each `d.Conditions` entry gets
   `ObservedGeneration` and `LastTransitionTime = now` truncated to the
   second, then `meta.SetStatusCondition`; `patch.ToPhoneStatus` /
   `ToSoftphoneStatus`; `HumanAction = d.HumanAction`; Softphone
   `ProvisioningURL`: `install.ProvisioningURL(Settings, token)` when
   `c.ProvisioningToken(id)` is ok, `""` when `bd.ProvisioningToken` is
   nil (not rendered), else unchanged (amended by 6.2.12: `""` unless the
   state is `Issued`, no `rotateToken` is in the Decision and the device
   Secret holds the compiled token);
9. `Status().Update` only when the status differs
   (`equality.Semantic.DeepEqual`); a conflict returns the error;
10. after a write: `StateChanged` when `state` changed (old `""` written
    `None`); `RequestIgnored` when `handledRequest` advanced to
    `dev.Request` and the Decision held no `resync` (Phone) /
    `rotateToken` (Softphone); `ApplyFailed` when `ConfigCurrent` became
    False/`ApplyFailed` from anything else (notes as in `install`
    events.go);
11. `ctrl.Result{RequeueAfter: d.RequeueAfter}` (or step 7's).

Watches (`Setup`): `For` with
`predicate.Or(GenerationChangedPredicate{}, AnnotationChangedPredicate{})`;
the ConfigMap named `Names.ConfigMap` → every resource of the kind;
`WatchesRawSource(source.Channel(cfg.Changes.Phones,
&handler.TypedEnqueueRequestForObject[*v1alpha1.Phone]{}))` (Softphones
likewise). Controller names `phone`, `softphone`.

*Tests (envtest; the test writes `pbx-config` and `pbx-secrets` with
`compile.Compile` + `install.ConfigMap`/`ProjectedSecret` as core would;
an `agentview.Holder`; a fake Actor recording calls; `events.NewFakeRecorder`;
a fixed clock):*
`TestPhoneUnseen` (state, every condition with ObservedGeneration, P2,
requeue 10 min), `TestStatusWrittenOnlyWhenChanged` (second reconcile: no
Update, resourceVersion unchanged), `TestNothingBeforeFirstPoll`,
`TestNothingBeforeCompiled` (no `pbx-config`; device not in the index),
`TestClaimsFromResolveClaims` (two Phones on 100: the newer is Invalid
with the older named; a MAC conflict; number 200 → NumberNotInPlan; the
same resources compiled give the same holders),
`TestResyncSentAndRecorded` (ConfigStale → one `Resync{phone-desk, V}`,
`lastAction{resync, V, now, 1}`, ActionSent),
`TestResyncFailureKeepsLastAction` (Sent=false and an error, each: no
`lastAction`, `handledRequest` unchanged with a pending request,
ActionFailed Warning, requeue 30 s), `TestRequestIgnored` (Unseen +
annotation), `TestApplyFailedControllerHalf` (§9 rollback: an agent
document with `LastApplyVersion` = the compiled version and an error →
`ConfigCurrent=False ApplyFailed` with the agent's message, one
`ApplyFailed` Event, none on a second reconcile; an error about another
version → no ApplyFailed), `TestNoResyncDuringCall` (§10.5: `Calls=1` →
no Actor call, ConfigCurrent False Stale "waiting for the call to end",
requeue 30 s; `Calls=0` → exactly one Resync),
`TestUnknownKeepsState` (unreachable agent: state kept, Ready Unknown
AgentUnreachable), `TestSoftphoneIssuedURL` (URL from the compiled
token; a `pbx-secrets` written ahead keeps the old URL),
`TestSoftphoneRotatesToken` (served provisioning fetch → the Secret's
token changes, 32 alphanumerics, `lastAction rotateToken`; Secret missing
→ ActionFailed), `TestSoftphoneReissue` (annotation → rotate, Issued,
`handledRequest`), `TestStateChangedEvent`, `TestSetupTriggers` (a
manager: a `Changes` event and a `pbx-config` update each reconcile; a
status-only update does not).

**W3.6 Person and Stream.** Gating as 6.2.1. Claims from
`ResolveClaims`. Conditions, in order Ready, NumberConflict, each with
ObservedGeneration:
- NumberConflict → NumberConflict True `NumberConflict` (claim message),
  Ready False `NumberConflict`, state `Invalid`;
- NumberOutOfPlan → NumberConflict False `NoConflict`, Ready False
  `NumberNotInPlan` (claim message), state `Invalid`;
- Person holding its number: each ring target exists (cache) and is
  routed (its index entry has a `Number`). All → Ready True `Routed`
  ("rings phone desk, softphone laptop"), state `Ready`; some →
  Ready False `MissingRingTargets` ("rings phone desk; not softphone x
  (does not exist), phone y (not routed)"), state `Incomplete`; none →
  the same reason, state `Unavailable` ("…: dialling 301 plays \"not
  available\"");
- Stream holding its number: Ready False `NoStreamBridge` ("release A has
  no stream bridge: dialling 705 plays \"not available\""), state
  `Unavailable`.

Write only on change; `StateChanged` Events. Watches: `For` with
`GenerationChangedPredicate`; the `pbx-config` ConfigMap → all of the
kind (any resource change that matters recompiles it). *Tests (envtest):*
`TestPersonRouted`, `TestPersonMissingTargets` (one missing, one
unrouted by a number conflict, then none left), `TestPersonNumberConflict`
(two Persons on 301), `TestPersonNotInPlan` (Person on 100),
`TestStreamNoBridge`, `TestStreamConflict`,
`TestStatusWrittenOnlyWhenChanged`, `TestNothingBeforeCompiled`.

**W3.7 discovery.** `Report`: for each `s.Document.UnknownDevices` entry
not yet reported for this `Document.Agent.StartedAt`: Get the `pbx-config`
ConfigMap (missing → skip, *not* marked, so it is reported once the
ConfigMap exists), Event Normal `UnknownPhone`, action `Discover`, note
`install.UnknownPhoneMessage(MAC, Source)`, regarding the ConfigMap; mark
it. A new `StartedAt` forgets the old set. It never blocks (the recorder
is asynchronous). *Tests (fake client, FakeRecorder):* once per MAC; the
same snapshot again: none; a new StartedAt: again; no ConfigMap: none,
then reported; exact note.

**Devices acceptance:** `P="./internal/controller/devices/... ./internal/controller/numbers/... ./internal/controller/discovery/..."`, rule 4's commands; coverage ≥ 80% each.

#### 6.2.7 Stage cli: W3.9

```go
// internal/cli, beside the landed env.go
func NewEnv(kubeconfig string, f ClientFlags, out, errOut io.Writer) (*Env, error)
func Status(ctx context.Context, env *Env) error
func SoftphoneQR(ctx context.Context, env *Env, name string, reissue bool) error
func PhoneResync(ctx context.Context, env *Env, name string) error
func FirmwareLoad(ctx context.Context, env *Env, zipPath string) error
func Models(w io.Writer, reg *drivers.Registry) error
func RenderQR(w io.Writer, text string) error
const ReissueWait = 30 * time.Second
const ReissuePoll = time.Second
```

**NewEnv.** `kubeconfig != ""` → that file only (`clientcmd` with
`ExplicitPath`; a missing file is an error naming it, never a fallback);
`""` → client-go's default rules (`$KUBECONFIG`, then `~/.kube/config`),
never the in-cluster config (the deferred loader falls back to it when
nothing is found: use the loaded raw config, not that fallback). Namespace: `f.Namespace`, else the context's, else
`default`. `Kube`: a controller-runtime client with the built-in types and
v1alpha1. `Agent`: the port-forward opener (`AgentOpener` docs: the
Running pod with the labels, exactly one or an error naming the count and
`--instance`; the token from the Secret its `agent-token` volume names,
key `install.AgentTokenKey`, trimmed; the port named `control` on the
`agent` init container; `portforward.NewOnAddresses` on 127.0.0.1 port 0
through `spdy.RoundTripperFor`; `Close` stops it and waits). `Registry`:
`builtin.Registry()`. `Clock`: `clock.RealClock{}`.

**Status.** Out, in order:
1. `Agent: <agent version>, Asterisk up (<version>) | Asterisk down,
   config <applied[:12]> applied[, last apply of <v[:12]> failed: <err>]`,
   or `Agent: could not ask the agent: <err>`;
2. per kind (Phone, Softphone, Person, Stream; names sorted; a kind with
   none is left out): `<Kind> <name> (<number>): <state or Pending>`, then
   two-space-indented lines: every condition not in its healthy status
   (True for Ready, FirmwareAvailable, FirmwareCurrent, Provisioned,
   Registered, ConfigCurrent; False for NumberConflict, MACConflict,
   Unsupported, LabelShortened) as `<Type>=<Status> <Reason>: <Message>`;
   `human action: <text>` when set; a Softphone with a URL: `provisioning:
   patchbay softphone qr <name>` (status never prints the URL);
3. `Unknown phones: none`, or `Unknown phones:` and one line each:
   `  <MAC> from <source>, first seen <RFC3339>, last seen <RFC3339>,
   asked for <file>`, or `Unknown phones: could not ask the agent: <err>`.
Exit 0 whenever the resources could be listed.

**SoftphoneQR.** Get the Softphone (NotFound: an error naming the
namespace). With `reissue`: value `Clock.Now().UTC()` in RFC 3339 with
nanoseconds, a JSON merge patch of
`metadata.annotations["patchbay.chrismiller.xyz/reissue-requested"]`, then
every `ReissuePoll` for at most `ReissueWait` until `handledRequest ==
value`, `provisioningURL` non-empty and different from before, and
`ConfigCurrent`'s reason is not `ApplyPending` (the bundle is live);
timeout → an error naming the state. With that new URL and
`ConfigCurrent` False `ApplyFailed` (the agent refused and rolled back the
bundle that grants it, so the URL answers 404) it fails at once, quoting
the condition's message (the agent's error; never the URL) (W3 review,
6.2.12). Without it: an empty URL is an error naming the state. Out: the
URL, a blank line, `RenderQR`, a blank line, `This code works once;
patchbay softphone qr <name> --reissue makes a new one.`

**PhoneResync.** Get the Phone; value as above; merge patch of
`…/resync-requested`; when `status.state` is not `Registered` or
`ConfigStale`, Err: `warning: phone <name> is <state>; the controller
resyncs only a Registered or ConfigStale phone, so it will mark this
request handled without a resync`; Out: `resync requested for phone <name>
(<value>)`.

**RenderQR.** `qr.Encode(text, qr.M)`; quiet zone 4; module rows paired
top/bottom (an odd last row's bottom is light); each cell is
`"\x1b[" + fg + ";" + bg + "m▀"` with fg 30 (dark) / 97 (light) for the
top module and bg 40 / 107 for the bottom one; each line ends
`"\x1b[0m\n"`.

**FirmwareLoad** (W3.9, Review Focus 3). Open the zip (never extracted to
disk). For each entry, sorted by name: skip directories; base name = the
part after the last `/` or `\`; refuse locally an entry over
`api.MaxFirmwareFileSize` (by its header and by reading at most that +1
bytes); hash it. Match the base name, case-sensitively, against every
file of every load of every driver in `env.Registry`:
- no match: `"<entry>: skipped: not in any firmware manifest"` (info);
  but a name equal to a manifest name ignoring case: `"<entry>: not
  uploaded: differs only in case from <name> (load <load>)"` (failure);
- one or more loads carry the name: the non-pending ones whose digest
  equals the hash get it (a Shared name thus goes only to the load it
  belongs to; equal digests in several loads go to each); none and all
  candidates pending → `"<name>: not uploaded: the manifest has no SHA-256
  for it yet (load <load>); its SHA-256 is <hex>"` (failure); none
  otherwise → `"<name>: not uploaded: its SHA-256 <hex> is not the
  manifest's for load <load>"` (failure);
- a (load, name) already matched by an earlier entry is not uploaded
  twice.
If anything matched, `OpenAgent` once, then PUT each match, in (load,
name) order, to `BaseURL + api.FirmwarePath(load, name)` with the token
and a Content-Length, and print `"<load>/<name>: stored (201)"`,
`"…: already stored (200)"` or `"…: refused by the agent (<code>):
<error>"` (failure). Then, per load touched, `"load <load>: <k> of <n>
files stored"`. Return an error when any failure happened or no entry
matched (`"no file in <zip> is in a firmware manifest"`).

**Models.** For each driver (registry order), each model (sorted): the
profile (hardware, provisioning with the password length, endpoint
options, features per backend), then each firmware load with its source,
models and files (`name  size  sha256 | pending`). A golden file pins it.

*Tests (no cluster; fake client; an `httptest.Server` agent behind a fake
`AgentOpener` that checks the bearer token and the manifest digests;
`clock/testing.FakeClock`; a test registry of fake drivers whose loads
are generated: `TESTA.1-0-0S` with `a.sbn`, `TESTA.1-0-0S.loads` and a
Shared `term45.default.loads`, `TESTB.2-0-0S` with `b.sbn` and its own
Shared `term45.default.loads`, and `TESTP.0-0-0S` whose `p.sbn` is
pending):* `TestFirmwareLoadLayouts` with generated zips: flat; under a
top-level folder; with extra files (readme, `.cop.sgn`); a wrong-case name
(failure, nothing uploaded for it); another load's files; the Shared name
of each load (uploaded only to its own); a pending file (failure naming
its SHA-256); a corrupted file (failure, not uploaded); no match
(failure); a duplicate entry; a directory entry; an oversized entry;
`../` and `\` names (base name only). `TestFirmwareLoadAgentFailures`
(422, 412, 500 → failure, exit error, the rest still uploaded).
`TestStatus` (golden, with unknown phones), `TestStatusAgentUnreachable`.
`TestSoftphoneQR`, `TestSoftphoneQRReissue` (the fake clock; a goroutine
plays the controller), `TestSoftphoneQRReissueTimesOut`, `TestPhoneResync`
(+ the warning), `TestRenderQR` (decodes back to `qr.Encode`'s grid; ≤ 80
columns for a 60-byte URL), `TestModels` (golden; `//go:build !e2e`, as
the e2e load changes it), `TestOpenAgent` (fake client: pod, Secret,
port; zero or two pods; `--instance`), `TestNewEnvNeverFallsBack` (rule
7).

**CLI acceptance:** `P="./internal/cli/..."`, rule 4's commands; coverage
≥ 80%.

#### 6.2.8 Stage fakephone: W3.10

`fakephone run [--script FILE]` (else `$FAKEPHONE_SCRIPT`) executes a
`script.Script` exactly as the `script` package documents (the automatic
behaviours in its package doc, each step type's doc). Beyond that:

- **SEP parser** (its own `encoding/xml`, never `cisco79x5`):
  `processNodeName`, `ports/sipPort`, `sipStack/timerRegisterExpires`,
  `phoneLabel`, line 1's `name`, `authName`, `authPassword`,
  `displayName`, `loadInformation`, `versionStamp`,
  `transportLayerProtocol` (1 TCP, 2 UDP, anything else an error).
  **lpconfig parser:** `proxy_0/reg_proxy` (`<sip:host:port;transport=…>`),
  `reg_identity`, `reg_expires`, `auth_info_0/username`, `userid`,
  `passwd`, `misc/config-uri` (cleared means present, empty, `overwrite`).
  Both tested on the drivers' render goldens
  (`internal/drivers/cisco79x5/testdata/render/*`,
  `internal/drivers/softphone/testdata/render/*`, read-only), so a render
  change that would break the fake phone fails here.
- **Web pages:** `scrape.DeviceInformationX`, `NetworkConfigurationX`,
  `DeviceLogX` and `scrape.MarshalPage`; tests parse them back with
  `scrape.Parse*`.
- **Calls:** answering per `script.Answer` (180, 200 with a PCMU answer
  SDP, `ToneHz` out, Goertzel counts per `DetectHz` in, the INVITE's From
  display name, `Remote-Party-ID` and `P-Asserted-Identity` recorded);
  `Call` reuses `invite`; `Transfer` sends an in-dialog REFER
  (`Refer-To: <sip:<to>@<pbx>>`) through the dialog session's `Do`,
  answering a digest challenge, then answers the `refer` NOTIFYs and
  leaves the call.
- **`call` gains `--expect-hz`** (default `--tone-hz`, the echo): the
  frequency whose packets `--min-tone-frames` counts, so a one-shot caller
  can check the far phone's tone.
- **Dockerfile:** `fakephone` now imports `internal/agent/scrape` (and
  through it `api/` and `internal/`), so the build stage copies `api/` and
  `internal/` and `Dockerfile.dockerignore` admits them (still without
  `*_test.go` and `testdata/`).
- **Logging:** a JSON line per step and per record (the State's records);
  never a password, the provisioning URL or a token.

*Tests (no cluster: a TFTP server from `pin/tftp` on 127.0.0.1:0, the
existing fake registrar, `httptest` for the provisioning URL, a test UAC
and UAS on loopback):* `TestParseSEP` / `TestParseLPConfig` (goldens; a
password placeholder replaced; `transportLayerProtocol` 3 refused),
`TestRunCiscoLifecycle` (fetch, `{load}.loads` retried until served,
firmware fetch, web down/up, signals, register with the parsed
credentials and expiry), `TestNotifyRefetchesAndReregisters` (new
password after the refetch → a REGISTER with it at once),
`TestRunLinphone` (GET once, 404 recorded, UDP register),
`TestWebPagesParse`, `TestSignalBeforeStep`, `TestStateJSON` (no password,
no URL), `TestAnswerRecordsCallerID`, `TestCallExpectTone`,
`TestTransferSendsRefer`, `TestStopRefreshingAndUnregister`,
`TestStepFailureExits` (exit 1, the step named), `TestRunNeverLogsSecrets`.
Also `docker build -f test/e2e/fakephone/Dockerfile -t
ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e .` succeeds (docker, no
cluster).

**Fakephone acceptance:** `P="./test/e2e/fakephone/"`, rule 4's commands,
the docker build; coverage ≥ 80%.

#### 6.2.9 Integrator: wiring, chart, e2e, docs

1. **Reconcile** reported deviations. **Compile-time assertions** in
   `internal/controller`: `var _ agentview.Actor = (*agentclient.Client)(nil)`,
   `_ poller.Source = (*agentclient.Client)(nil)`,
   `_ agentclient.TokenSource = (*secrets.Manager)(nil)`,
   `_ agentview.Observer = (*poller.Poller)(nil)`,
   `_ manager.LeaderElectionRunnable = (*poller.Poller)(nil)`,
   `_ agentview.SnapshotFunc = (*discovery.Reporter)(nil).Report`.
   Remove `internal/cli/doc.go`'s blank `rsc.io/qr` import once the
   renderer imports it; run `go mod tidy -diff` (empty).
2. **`internal/controller`** (root): `Options` gains `Fullname`,
   `SettingsFile`, `AgentURL`; `func Setup(mgr ctrl.Manager, opts Options,
   s compile.Settings, reg *drivers.Registry) error` builds: `names :=
   install.NewNames(opts.Fullname)` (Validate), a `Holder`,
   `NewChanges(DefaultChangesBuffer)`, the recorder
   `mgr.GetEventRecorder(install.EventRecorderName)`, `metrics.New`
   (controller-runtime's `metrics.Registry`; Observer: the `Holder`),
   `secrets.New`, `agentclient.New` (Tokens: the secrets manager),
   `discovery.New`, `poller.New` (Holder, Metrics, OnSnapshot: the
   reporter) then `mgr.Add(poller)`, `bundle.New(…).SetupWithManager`,
   `devices.Setup` (Observer: the `Holder`, which the poller publishes
   through; this order has no cycle), `numbers.Setup`. envtest `TestSetupWiresEverything` (a
   Phone compiles into `pbx-config`; a fake agent behind `httptest`
   makes it Unseen).
3. **`cmd/patchbay controller`**: flags `--fullname` (required),
   `--settings-file` (default `/etc/patchbay/settings/settings.json`),
   `--agent-url` (default `install.AgentURL(names, namespace,
   api.DefaultPort)`); `settings.Load`, `install.CheckSettings`,
   `builtin.Registry()`, `NewManager`, `Setup`. **Client commands**: the
   stubs become thin cobra glue over `internal/cli` (`-n/--namespace`,
   `--instance` on each client command; `models` needs no kubeconfig);
   `main_test` keeps only `stream-bridge` as a stub and runs each command
   through cobra with a `--kubeconfig` that does not exist and
   `KUBECONFIG`/`HOME` in `t.TempDir()`.
4. **`internal/agent/server.go`**: `Serve` caps connections with
   `netlimit` (`MaxControlConns = 64`, `MaxControlConnsPerSource = 16`);
   `TestServeCapsConnections`.
5. **Chart**: values `timezone` (default `UTC`), `ntpServer`
   (`192.168.20.1`), `pbx.signalling` (`pjsip`), `numberPlan` (§1.1),
   `allowedNetworks` (`192.168.20.0/24`, `192.168.0.0/24`); a
   `<fullname>-settings` ConfigMap rendering `settings.json` from them and
   `pbx.address`/`pbx.ports` (`amiSecret` = `<fullname>-ami/password`,
   `streamBridge` empty); the controller mounts it read-only at
   `/etc/patchbay/settings`, gets `--fullname` and `--settings-file`, and a
   `checksum/settings` annotation over the JSON; `pbx.agent.tokenSecretName`
   and the "create it before installing" note go (NOTES.txt says the
   controller mints the token). The controller's Role is unchanged.
   *test/chart:* each ci values file's `settings.json` passes
   `settings.Parse` and `install.CheckSettings`, the default one equals
   core's `TestParseChartDefaults` fixture; the controller's flags, mount
   and checksum (which changes with a setting); `internal/cli`'s constants
   match the PBX pod (init container `agent`, port `control`, volume
   `agent-token`, labels).
6. **Build**: `make e2e-images` also builds `.e2e/bin/patchbay` with
   `-tags e2e` (the CLI the e2e runs, with the e2e load); the fakephone
   image build picks up its new Dockerfile.
7. **e2e harness** (the real controller; `bundle_test.go` and its
   `applyBundle`, `writeBundle`, `ensureSecretKey`, `newSecretKey`,
   `rotateProvisioningToken` deleted, `secretValue`/`upsert*` kept where
   still used):
   - install: namespace, `helm upgrade --install` (runtime values add
     `allowedNetworks: [<podCIDR>]`), wait for the CRDs, then create the
     **site resources**: Phone `desk` (100, MAC `0200E2E00001`, label
     `Desk`), Phone `kitchen` (101, `0200E2E00002`, `Kitchen`), Softphone
     `laptop` (150, `Laptop`), Person `chris` (301 → desk, laptop); wait
     for both rollouts (the PBX pod waits for the controller's token and
     bundle); read the agent token from the controller's Secret;
   - helpers: `startPhone(name, script.Script, extraEnv) pod` (a run-mode
     tools pod with `securityContext.sysctls:
     net.ipv4.ip_unprivileged_port_start=0` and `FAKEPHONE_SCRIPT`),
     `signal(pod, name)` and `phoneState(pod) script.State` through
     `pods/proxy` on port 80, `waitDevice(kind, name, what, timeout,
     func(status) (bool, string))` watching status, `patchbay(args…)`
     running `.e2e/bin/patchbay --kubeconfig <.e2e/kubeconfig> -n
     patchbay-e2e`, `wipeE2ELoad()` (`docker exec <node> rm -rf` the e2e
     load's directory on the firmware PV's host path);
   - W2 tests move off the harness's bundle: they read the compiled
     bundle with `install.ReadCompiled` (not `h.bundle`) and use `kitchen`
     (never `desk` or `laptop`, which `TestDeskLoop` owns):
     `TestTFTPAndHTTP6970`, `TestRegister`, `TestEchoCall`,
     `TestResyncNotify`, `TestPrompts`, `TestRollback`'s register and echo;
     `TestProvisioningOneTime` (sequential, before `TestDeskLoop`) takes the
     URL from `laptop`'s status (waiting for `ConfigCurrent` not
     `ApplyPending`) instead of rotating the token itself;
   - `TestController` also asserts: `pbx-config` and `pbx-secrets` carry
     the same `bundle-version`, the agent applied it, the agent token
     Secret has the controller's labels.
8. **e2e tests.** *Sequential* (Go runs these first, in file order):
   `TestCRDs`, `TestPBXUp`, `TestController`, `TestFirmwareUploadAndServe`,
   `TestProvisioningOneTime`, `TestRollback`. *Parallel* (`t.Parallel()`
   before `testenv.Test`, or one `testenv.TestInParallel` if the framework
   refuses): every other test, each on its own devices.

   | Test | Steps | Assertions |
   |---|---|---|
   | `TestRollback` (§9 rollback, agent half) | scale the controller to 0 (wait for its pod to go); read `install.ReadCompiled`; write a broken bundle (`kitchen`'s endpoint renamed in `pjsip.conf`, version from `VersionWith`) with `install.ProjectedSecret` then `install.ConfigMap`; wait; register and echo as kitchen; scale to 1; `patchbay softphone qr laptop --reissue` | `LastApplyVersion` = broken with a verify error naming kitchen, `AppliedVersion` the good one, registers and echoes; after scale-up `pbx-config` holds the compiled version; after the reissue the agent's last apply is that new version with no error; no device ever showed `ApplyFailed` |
   | `TestPhoneLifecycle/lifecycle` (§9 "Desk walks to Registered") | `wipeE2ELoad`; recreate Phone `lifecycle` (111, `0200E2E00011`, firmware `E2E45.0-0-1S`, label `Lifecycle`) → Unseen; start a cisco phone: web `SCCP45.9-4-2SR4-3S`/`jar45sccp…`, `fetchConfig`, `fetch {load}.loads` every 3 s within 3 min, `fetch appsE2E.0-0-1.sbn` every 1 s within 30 s (the CLI stores the `.loads` a moment before the `.sbn`), web down, `waitSignal boot`, web `E2E45.0-0-1S`, `fetchConfig`, `waitSignal register`, `register` | Unseen (Ready False Unseen, P2); NeedsFirmware (FirmwareAvailable False NotStaged, FirmwareCurrent False OtherLoad, P3 listing both files); after the firmware load, FirmwareAvailable True; Upgrading (P7, `upgradeStartedAt` set); signal `boot`; Registering at about `lastFw` + 3 min (Provisioned True, HumanAction ""); signal `register`; Registered: Ready, FirmwareAvailable, FirmwareCurrent (Running), Provisioned, Registered, ConfigCurrent all True, HumanAction "" |
   | `TestPhoneLifecycle/firmware-load` (§9 `TestFirmwareLoad`, both rows) | before the good load, while lifecycle is NeedsFirmware: `patchbay firmware load` of a generated zip (under a top-level folder, with a readme) holding only a corrupted `.sbn` (a good `.loads` in it would be stored and start the upgrade); then the good zip of both files | the corrupted run exits non-zero naming the file and stores nothing for it (the store sensor still lacks it, lifecycle still NeedsFirmware); the good run exits 0 with `stored (201)` per file (or `already stored (200)`) |
   | `TestPhoneLifecycle/reset` (§9, the Desk phone's own path) | recreate Phone `reset` (112, `0200E2E00012`, firmware `E2E45.0-0-1S`); start a cisco phone: web `E2E45.0-0-1S` with log `Trust List update failed`, `fetch SEP{mac}.cnf.xml.sgn` (notFound), `waitSignal reset` (sent once firmware-load has staged the load, so the downloads are served at once), web down, `fetch term45.default.loads` (notFound), `fetch E2E45.0-0-1S.loads` and `appsE2E.0-0-1.sbn` (retried until served), `fetchConfig`, `register` | NeedsTrustReset with **P6** (6.2.2); after `reset`: Upgrading and never NeedsTrustReset again (every state the watch saw); then Registered with FirmwareCurrent Unknown `NotReported`, no `lastAction`, no NOTIFY in the phone's state |
   | `TestDeskLoop/softphone` (§9 Softphone, Review Focus 4) | `patchbay softphone qr laptop --reissue` (parse the URL from its output); start a linphone phone with it: `fetchConfig`, `register`; a one-shot `fakephone http GET` of the same URL | laptop Issued with that URL (HumanAction S1); Provisioned (`lastAction rotateToken`, a new URL; since 6.2.12 no URL is published outside Issued); Registered (Ready True); the second GET is 404 and laptop stays Registered; its State's `configURICleared` |
   | `TestDeskLoop/provision` | start a cisco phone as desk: `fetchConfig`, `register`; it answers calls (600 Hz, detects 800 and 1000) | desk Registered, ConfigCurrent True, its State's `phoneLabel` `Desk` |
   | `TestDeskLoop/calls` (§9 `TestCalls`) | desk signal → `call 150` held 4 s expecting 800 Hz; laptop (answers with 800 Hz, detects 600) signal → `call 100` held 4 s expecting 600 Hz | both calls pass; laptop's record from desk: `fromDisplay` or `pAssertedIdentity` holds `Desk`, ≥ 50 packets of 600 Hz; desk's record from laptop: `remotePartyID` holds `Laptop`, ≥ 50 packets of 800 Hz |
   | `TestDeskLoop/person` (§9 `301`) | one-shot `call 301` as kitchen, 3 s, `--expect-hz 600`; laptop's answer delay 3 s, desk's 1 s | both phones' States show the INVITE from 101; desk answered, laptop's was cancelled; kitchen heard 600 Hz |
   | `TestDeskLoop/transfer` (§9 `TestTransfer`) | one-shot `call 100` as kitchen (1000 Hz, 12 s, no echo check); desk signal → `waitCall`, `transfer 150` | desk's REFER got 202; laptop answered a call from kitchen and counted ≥ 50 packets of 1000 Hz |
   | `TestDeskLoop/label` (§9 `TestLabelLoop`) | patch desk's label to `Desk Two` | a new compiled version; one resync NOTIFY in desk's State; it refetched; `phoneLabel` `DeskTwo`; desk Registered with `lastConfigFetch.version` = its compiled version |
   | `TestDeskLoop/change-during-call` (§10.5) | desk signal → `call 600` open; patch the label to `Desk Three`; wait 20 s; signal → `hangup` | while the call is up: ConfigStale, ConfigCurrent False Stale "waiting for the call to end", no new NOTIFY; after the BYE exactly one NOTIFY, then Registered |
   | `TestDeskLoop/secret-heals` (§9 `TestSecretHeals`) | delete Secret `patchbay-phone-desk`; afterwards a one-shot REGISTER as desk with the old password | a new Secret (new UID, new value); a new version; one NOTIFY; desk's `passwordSHA256` = the new value's; desk Registered; the old password gets 401 |
   | `TestDiscovery` (§9 second phone) | a random MAC `0200E2E1xxxx`; one-shot `tftp get SEP<mac>.cnf.xml`; `patchbay status`; create Phone `discovered` (113, that MAC); start a cisco phone: `fetchConfig`, `register` | an `UnknownPhone` Event on `pbx-config` with the exact note and the pod's IP; `status` lists the MAC with that source; then Registered (FirmwareCurrent Unknown) |
   | `TestPrompts` (W2, extended: §9 reserved numbers) | as W2, also `999` and `205` | each answered with ≥ 2 s of non-silent audio |

   Order inside `TestPhoneLifecycle` (subtests share its two phones):
   wipe the e2e load; create `lifecycle`, assert Unseen, start its phone;
   create `reset`, start its phone, assert NeedsTrustReset; assert
   lifecycle NeedsFirmware; firmware-load (corrupted, then good); signal
   `reset`; assert both Upgrading; signal `boot`; wait for reset Registered
   and lifecycle Registering; signal `register`; lifecycle Registered.
   Order inside `TestDeskLoop`: softphone, provision, calls, person,
   transfer, label, change-during-call, secret-heals. Desk's and laptop's
   phones are started once (in softphone and provision) with their whole
   scripts: every later action is a step behind a named signal (`call-150`,
   `call-100`, `transfer`, `call-600`, `hangup`), and both answer calls
   throughout (desk after 1 s with 600 Hz, laptop after 3 s with 800 Hz;
   both detect 600, 800 and 1000 Hz). Both test functions delete their
   phone pods and per-test resources at the end, and recreate per-test
   resources at the start (a reused cluster).

   **Duration budget** (fresh cluster, images cached; target ≤ ~7 min):
   kind create and image load 60 s; install, token, compile and rollout 40
   s; sequential tests 100 s (TestRollback 60 s of it); the parallel group
   225 s, set by `TestPhoneLifecycle` (the Upgrading window is a fixed 3
   min after the last firmware fetch: lifecycle and reset fetch theirs
   within ~30 s of each other) with `TestDeskLoop` (~180 s) and the rest
   under it; teardown 15 s. Each test's own timeouts: states 60 s
   (Registering and the final Registered: 5 min), Events 30 s, CLI runs 60
   s. `E2E_TIMEOUT` stays 30m. Report the measured times in "W3 as built".
9. **Docs**: docs/e2e.md rewritten for the real controller (site
   resources, scripted phones, signals and state through pods/proxy, the
   parallel group, the budget); CLAUDE.md deviations for W3's departures;
   the plan's "W3 as built".

#### 6.2.10 W3 acceptance

```sh
cd /home/chris/Repos/patchbay
nix develop -c make generate check-generated fmt vet lint test   # green
nix develop -c go mod tidy -diff                                 # empty
nix develop -c make check-hygiene
nix develop -c make asterisk-check
nix develop -c make e2e                                          # leaves no cluster; ≤ ~7 min
```

Done when every 6.2.5–6.2.9 test exists and passes, every carry-over in
6.2.2 is closed or in §11, every §9 row marked W3 has its test, and
CLAUDE.md "Deviations" lists what W3 changed.

#### 6.2.11 W3 as built

Where W3 departs from 6.2.1–6.2.10. The stage-level deviations are the
stages' reports, condensed; CLAUDE.md records the departures from the
design.

**Core** (settings, secrets, bundle, agentclient, poller, metrics)
- **bundle compares the whole of `pbx-config`** (data, binary data, the
  `bundle-version` annotation, the managed labels), not only the index
  version: a hand-edited file key is written back. `pbx-secrets` is
  compared on data, annotation and labels. The ConfigWritten Event follows
  any write; `Metrics.ConfigWritten` follows every reconcile that leaves
  `pbx-config` at a version, so `patchbay_config_version_info` survives a
  controller restart that writes nothing.
- **`secrets.ErrInvalid`**: a value outside its generator's alphabet, a
  Secret that is not Opaque, an immutable one lacking a key, or a device
  Secret controlled by another object is refused (messages name the Secret
  and key, never the value), handled like a compile failure (CompileFailed,
  metric, 5 min requeue): the agent resolves values into config files
  unescaped. The agent token is trimmed and any printable non-space ASCII
  is accepted (an old hand-made token survives). Adopted Secrets get the
  managed labels added; an existing label value is kept. A Phone whose
  model no driver serves gets no device Secret; every Softphone gets a
  provisioning token.
- agentclient sets per-call timeouts (5 s observations, 10 s actions, 5 s
  dial), never a client-wide one, and copies a caller's client to forbid
  redirects. metrics: `patchbay_registered_endpoints` and the observation
  age are absent before the first good poll, `patchbay_agent_reachable`
  before the first attempt. poller: an attempt cut short by shutdown
  publishes nothing; a failed index read keeps the old fingerprints (the
  next poll enqueues again); attempts and enqueues are bounded at twice the
  interval; no backoff (a backoff would delay noticing the agent past
  StaleAfter).

**Devices** (devices, numbers, discovery)
- The Softphone controller also watches `pbx-secrets` (its informer can
  deliver after `pbx-config`'s, and the URL needs both).
- **Stale-cache guard**: each reconciler remembers the resourceVersion it
  read before its last status write; a cached copy at exactly that version
  is requeued after 250 ms, not decided (it re-sent a recorded resync and
  then lost the write to a conflict).
- Status-write errors are wrapped so an Invalid error quotes nothing from
  the object (it could quote `provisioningURL`); ActionFailed for
  rotateToken names no version; ApplyFailed names the compiled bundle's
  version (the one the agent refused).
- Open: a label with `@@` is collapsed by Compile but not in the
  LabelShortened condition (status text only); on a failed action the
  Decision's conditions still say "resync sent" while `lastAction` is kept;
  an action is sent before its status write, so a real write conflict
  (not cache lag) sends it again.

**CLI**
- `firmware load` matches names before reading (a large non-firmware entry
  never fails a run), names a refused file by its path in the zip, and
  prints a few lines 6.2.7 does not list (duplicates, non-regular files,
  read errors, connection errors); `status` prints a blank line between
  sections and `no config applied yet`; `NewEnv("")` uses `$KUBECONFIG`,
  then `~/.kube/config`, without client-go's migration and never the
  in-cluster config; `--reissue` prints one progress line (no URL) on
  stderr; `models` marks shared files and a default load missing from the
  manifest.

**Fakephone**
- A check-sync NOTIFY also refetches; `MinToneFrames` 0 means at least one
  packet; `run` has `--local-ip` and `--sip-port`; a Register step that
  expects a refusal uses its own Call-ID and leaves the registration alone;
  the config parsers refuse an unresolved placeholder; one-shot commands
  answer an INVITE with 486; after a 202 to its REFER the phone waits up to
  10 s for the final NOTIFY before its BYE.

**Integrator** (wiring, cmd, chart, build, e2e)
- `controller.Setup` as 6.2.9, plus `Options.Registerer` (nil:
  controller-runtime's registry; a process registers once). The
  **leader-election Lease is per install**,
  `<fullname>-controller.patchbay.chrismiller.xyz` (the same name as
  before for the default release), so the Role can name it and two
  installs in a namespace never share a leader.
- **The controller's Role is least privilege** (6.2.9 said unchanged; the
  task asked for it): read its resources, `update` only their status, and
  phones'/softphones' finalizers (`blockOwnerDeletion`); ConfigMaps
  `get/list/watch/create`, `update` by name for `pbx-config` only; Secrets
  `get/list/watch/create/update` (device Secret names are per resource);
  Events in both APIs (leader election uses the core one); Leases `create`,
  `get/update` by name. No `delete`, no `patch`. test/chart pins it rule
  by rule.
- The chart's settings render every number-plan entry as a string
  whatever YAML made of it; the controller gets `--agent-url` from
  `pbx.ports.control` (the default would ignore an override).
- `make test` also runs controller-devices' e2e-tagged envtest
  (`TestPhoneLifecycleFirmware`), which no target ran.
- **e2e, against 6.2.9's design**:
  - `TestRegister` registers kitchen over TCP only; the Softphone over UDP
    is `TestDeskLoop`'s laptop (its registration from the Linphone pod).
    The W2 tests run in the parallel group; `TestRegister` and
    `TestResyncNotify` take turns on kitchen's one contact.
  - **`TestPhoneLifecycle/reset` reaches NeedsTrustReset by the 3 min .sgn
    rule**, not the trust log the plan scripted: the agent scrapes only a
    phone it has served or that registered (`observe.ScrapeTargets`), and
    a phone asking only for its `.sgn` is neither, so its web page is never
    read. That is the real Desk phone's path too (§11). After the reset the
    phone takes its config before the firmware: a firmware fetch from an
    address the agent has not served a config to is attributed to nobody.
    The reset path is therefore 3 min + the 3 min download window, and the
    parallel group takes about 6 min, not 225 s.
  - **`TestRollback`**: a restarted controller reads `pbx-config` as it
    finds it (`install.ReadCompiled`, 6.2.1's stateless choice), so until
    its own compile replaces the test's broken bundle a device may report
    `ApplyFailed` for that version; the test asserts that no ApplyFailed
    names any other version and that none shows it at the end. And the
    controller's `pbx-secrets` rewrite (only its annotation changes) makes
    the agent apply the refused version again, another 30 s of
    verification (§11), so the first `--reissue` can time out; the test
    reissues once more.
  - `TestDeskLoop/transfer`: desk waits 2 s after answering before its
    REFER. A REFER at once is accepted (202) but fails before Asterisk has
    bridged the call (`SIP/2.0 400 Bad Request` in the refer NOTIFY); a
    person always talks first. The test also checks the final NOTIFY says
    200.
  - `TestProvisioningOneTime` scales the controller to 0 around the agent
    restart: with the controller up, it rotates the spent token at once,
    the rotated version settles, and the agent then forgets the spend (it
    guards a token no bundle grants any more), so a restart has no served
    fetch to remember (the first fresh-cluster run failed so). The test
    checks the case that matters: the agent restarting before the
    controller acted on the fetch; scaled back, the controller rotates.
  - Scripted phones' scripts travel in Secrets (a provisioning URL holds a
    token), never pod args or plain env; CLI runs are bounded at 60 s.
- **First runs against real Asterisk** (fakephone's open items): the
  in-dialog REFER works on stock `res_pjsip_refer` (202, then sipfrag
  NOTIFYs `100 Trying`, then `200 OK`; Asterisk sends the transferee to 150
  in `patchbay-devices`, where laptop hears it); the resync NOTIFY reaches
  a run-mode phone over TCP; TFTP and HTTP :6970 fetches attribute as W2
  documented.

**Measured** (a fresh cluster, images cached, this workstation; `make e2e`
took 558 s wall, `go test` 551 s), against 6.2.9's budget of about 7 min:

| Phase | Budget | Measured |
|---|---|---|
| kind create and image load | 60 s | 37 s + 24 s |
| install, token, compile, rollout | 40 s | 10 s (helm 1 s, the controller 2 s, the PBX pod 6 s, its first bundle already applied) |
| sequential tests | 100 s (TestRollback 60 s) | 111 s: TestPBXUp 12, TestController 1, TestFirmwareUploadAndServe 4, TestProvisioningOneTime 16, TestRollback 79 |
| parallel group | 225 s | 368 s, set by TestPhoneLifecycle (the reset path's 3 min `.sgn` rule, then its 3 min download window); TestDeskLoop 127 s, the rest under 40 s |
| teardown | 15 s | about 5 s |
| total | about 7 min | about 9.3 min |

The overrun is the reset path above: the plan scripted a trust-log scrape
that cannot happen. TestRollback is 79 s: the broken version's 30 s
verification, the agent's second one after the controller's
`pbx-secrets` rewrite (§11), and a reissue.

#### 6.2.12 W3 review fixes

Three independent read-only reviews of W3 (security, correctness,
robustness) raised ten findings. Each was reproduced first (a failing
test where one could be written); none was rejected, one was narrower
than stated (3). What changed, and what pins it:

| # | Finding | Fix | Pinned by |
|---|---|---|---|
| 1 | (major) Every provisioned softphone kept a live, unspent provisioning URL in `status.provisioningURL` (the driver rotates the spent token; the shell published the new one in every state), so anyone able to read Softphones but not Secrets could take the SIP password | `status.provisioningURL` only while the Decision's state is `Issued` (design §8.4), never in the reconcile whose Decision rotates the token, and only while the device Secret still holds the compiled token (a reissue's old token is never shown between the rotation and the recompile, where `qr --reissue` would print it); amends 6.2.1 and 6.2.6 step 8 | `TestSoftphoneURLOnlyWhileIssued`, `TestSoftphoneRotatesToken`; e2e `TestDeskLoop` (no URL after the spend, none while Registered), `TestProvisioningOneTime` (reissues first when the laptop has no URL) |
| 2 | (major) Text a VLAN-20 host controls reached Phone status unbounded: a 40 kB `versionID` made every status write fail as Invalid, freezing the Phone's state; CR and C1 controls reached `patchbay status` | the scraper keeps each field ≤ 128 bytes and each log line ≤ 256, control characters as spaces (`scrape.MaxField`, `MaxLogLine`); the controller bounds condition messages and `humanAction` (2048) and `observed.firmware`/`address` (128) with `install.Text`; `status` prints each control character but a newline as a space | `TestParseBoundsWhatThePhoneSays`, `TestHostileTextBounded`, `TestText`, `TestStatusPrintsNoEscapes` |
| 3 | (major) client-go's events broadcaster joins Events by type, reason, action, regarding (resourceVersion included) and related, not the note: every UnknownPhone regards one pbx-config, so a second unknown phone within 6 min was lost, and a host kept the series alive | each UnknownPhone names its MAC in `related` (`install.UnknownPhoneRelated`). Narrower than reported: a device's Events after a status write regard its new resourceVersion and stay distinct; what still joins is an ActionFailed repeated before the status changes and a CompileFailed repeated while pbx-config is unchanged (first note kept; §11) | `TestEventSeries` (the real broadcaster), `TestEachMACItsOwnEvent`, `TestSetupWiresEverything` (the API server takes the reference) |
| 4 | (minor) Discovery's reported set grew without bound under a flood of fresh MACs (~70 MiB a day) | it keeps only the MACs the agent still lists (≤ 64); and since each MAC is now its own Event, at most 16 Events at once and one per 30 s (`discovery.ReportBurst`, `ReportEvery`), a held-back MAC reported later while listed | `TestFloodOfFreshMACs` |
| 5 | (minor) `softphone qr --reissue` printed a new URL whose bundle the agent had refused (ApplyFailed; it answers 404) | fails at once quoting the condition's message (6.2.7 amended) | `TestSoftphoneQRReissueApplyFailed` |
| 6 | (minor) Device and Person/Stream Event notes over events.k8s.io's 1024 bytes were refused and lost (ApplyFailed with a long verify error) | every reconciler passes notes through `install.EventNote` (the bundle's own 1000-byte cut is gone) | `TestEventNote`, `TestLongNoteBounded`, `TestHostileTextBounded` |
| 7 | (minor) e2e race: `TestDiscovery` and `TestPhoneLifecycle/reset` created a Phone, then `waitApplied` returned at once on the previous bundle | `waitCompiled` waits for the bundle that holds the device first | e2e |
| 8 | (minor) A reused cluster passed `TestPhoneLifecycle` only because `TestProvisioningOneTime` had restarted the agent: the agent keeps a device's fetch log by ID after the device leaves the bundle | the lifecycle and reset Phones take a random suffix per run, and Setup deletes an interrupted run's; the agent side is §11 | e2e |
| 9 | (minor) Cache lag was a reconcile error: the bundle reconciler's create (AlreadyExists) and update (Conflict) from a stale copy, the Person/Stream status writes (Conflict), each an ERROR, counted, backed off | a requeue after 1 s with a V(1) line (`bundle.StaleRetry`); other API errors are still returned. The device reconcilers keep their stale-cache guard (6.2.11), and a real conflict there is still an error | `TestCacheLagIsNotAnError`, numbers `TestStatusWriteError` |
| 10 | (nit) `go test -count=2 ./internal/controller/` failed: controller-runtime's process-wide controller-name check | the package's tests turn it off (`export_test.go`); the binary keeps it | `go test -count=2` |

Verified after the fixes (this workstation, images cached): `make e2e` from
scratch 15/15 in 607 s wall (`go test` 586 s; TestPhoneLifecycle 376 s,
TestDeskLoop 123 s, TestRollback 78 s, TestProvisioningOneTime 17 s), no
cluster before or after. Then the reused-cluster case of finding 8:
`make e2e-up`, and `TestPhoneLifecycle` alone twice on that cluster with
`PATCHBAY_E2E_REUSE=1` (the agent not restarted between): both passed
(362 s each).

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
| *(W3)* After a factory reset, does the 79x5 fetch its config before its firmware? | none: the agent attributes a load's files by source address, and only to a device that fetched its config (or registered) from that address (`observe`) | as is (the e2e reset phone fetches its config first) | if it downloads first, its download is nobody's and the controller never shows Upgrading: attribute a load's files to the devices whose SEP names that load |

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
| Desk walks to `Registered` with FirmwareCurrent and ConfigCurrent, no other human step | e2e `TestPhoneLifecycle`: fakephone scripted Unseen → NeedsFirmware → (load staged) Upgrading → Registering → Registered, asserting state, conditions and human action at each step (W3). *Added in the W1 review,* the Desk phone's own path (design §2, §11 "after a factory reset"): fakephone asks only for the `.sgn` → NeedsTrustReset (P5) → (factory reset: `term45.default.loads` and the load's files served while its web page is down and the agent still holds the old trust log) Upgrading, never P5 again → config fetch and REGISTER → Registered with no resync before any successful scrape (FirmwareCurrent Unknown until one). Driver-level twin: cisco79x5 `TestLifecycle` (W1) | `[real]` 7945 |
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

**Status after W3.** Every automated test in this table exists and passes
(`make test`, `make e2e`); the `[real]` column is still open. The W3 rows,
by test: `firmware load` accepts the zip, and rejects a wrong checksum:
`TestPhoneLifecycle/firmware-load` (the CLI refuses a corrupted file
locally and stores the good zip) with the CLI's `TestFirmwareLoadLayouts`,
and `TestFirmwareUploadAndServe` (a forced PUT gets 422). Desk walks to
`Registered`: `TestPhoneLifecycle` (lifecycle; and reset, the Desk phone's
own path, which reaches NeedsTrustReset by the 3 min `.sgn` rule with P6,
6.2.11). Softphone: `TestDeskLoop/softphone`. `100`↔`150`:
`TestDeskLoop/calls`. `301`: `TestDeskLoop/person`. `911` and the
reserved numbers: `TestPrompts` (`911`; `0`, `999`, `205`; `199`
unassigned). Label change: `TestDeskLoop/label`, and Review Focus 5
`TestDeskLoop/change-during-call`. Deleting desk's Secret:
`TestDeskLoop/secret-heals`. Rollback: `TestRollback` and the envtest
`TestApplyFailedControllerHalf`. Second phone: `TestDiscovery`. Transfer:
`TestDeskLoop/transfer`.

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
- *(W3, from W2's open items: §6.2.2)* **After an agent restart, a
  bundle that fails verification leaves the agent without AMI.** The
  adopted version it rolls back to has no AMI account until intake hands
  that version over again, so the agent cannot log in: every device shows
  `Registered=False` `PBXDown` and softphone provisioning URLs answer 404,
  while calls keep working. Any new version heals it: fix or revert the
  change that broke the bundle (or `patchbay softphone qr <name>
  --reissue`, which makes one). It needs an agent restart and a failing
  version together; compiled bundles do not fail verification in normal
  operation.
- *(W3, from W2)* **An AMI password change while the AMI session is down**
  (the `<fullname>-ami` Secret deleted during an Asterisk outage), or a
  rollback whose `manager` reload cannot run, leaves the agent logging in
  with a password Asterisk does not have: `PBXDown` on every device until
  Asterisk restarts (`kubectl -n <namespace> delete pod -l
  app.kubernetes.io/component=pbx`; calls in progress drop).
- *(W3, from W2)* **A version swapped in while AMI was down that then
  fails verification is not retried** by the agent until its ConfigMap or
  Secret changes, and each agent restart applies it again and rolls it
  back. Devices show `ConfigCurrent=False` `ApplyFailed` with the agent's
  error until a new version arrives.
- *(W3)* **The controller's objects outlive `helm uninstall`**:
  `pbx-config`, `pbx-secrets`, `<fullname>-ami` and `<fullname>-agent-token`
  have no owner (device Secrets go with their resources). Delete them by
  name (NOTES prints the commands), never by the managed-by label: every
  device Secret carries it too, and with the Phones kept (the CRDs survive
  uninstall) a reinstall would then re-provision every phone (final
  review).
- *(W3)* **A controller restart reports each unknown phone again**, once:
  discovery remembers which MACs it reported for the agent's current
  process in memory only, and only while the agent lists them (its 64
  most recent): a phone that drops out of that list and asks again is
  reported again (6.2.12).
- *(W3 review)* **A flood of unknown MACs delays real ones.** A host
  requesting `SEP<random MAC>.cnf.xml` in a loop fills the agent's list;
  discovery emits at most 16 UnknownPhone Events at once and one per 30 s,
  so a real phone may be reported minutes late, or not at all if the
  flood pushes it out of the agent's 64. `patchbay status` lists the
  agent's live list.
- *(W3 review)* **Some repeated Events keep their first note.** client-go's
  events broadcaster joins Events with the same type, reason, action,
  regarding object (resourceVersion included) and related object into one
  series for six minutes after the last: an ActionFailed repeated before
  the device's status changes, and a CompileFailed repeated while
  `pbx-config` is unchanged (the 5 min retry keeps the series alive, so a
  new compile error shows the first one's note). The controller log has
  every one.
- *(W3 review; narrowed in the final review)* **The agent keeps a device's
  history by its ID** while the device stays in the applied index: its
  store forgets a device once a version without it, or with another MAC
  for it, is applied (`TestDeviceLeavingTheIndexLosesItsHistory`). A Phone
  deleted and created again so quickly that the agent never applies a
  version without it keeps the old one's fetches (never Unseen; the
  status, keyed by the new resource, starts over anyway).
  `TestPhoneLifecycle` still uses random names.
- *(W3 review)* **Long text is cut**: a condition message or human action
  over 2048 bytes, an Event note over 1024 (events.k8s.io's limit), a
  scraped field over 128 bytes or a log line over 256 ends in an ellipsis;
  control characters become spaces.
- *(W3, found in e2e)* **The Desk phone's trust log is never read.** The
  agent scrapes only a phone it has served a file to or that holds a
  registration, and a phone with its old CUCM's trust list asks only for
  `SEP<MAC>.cnf.xml.sgn`, which is never served. So the cisco79x5 rule's
  trust-log clause cannot fire for it; the other clause does: after 3 min
  of only `.sgn` requests the Phone is NeedsTrustReset, with P6 (stage the
  default load first) until that load's digests are filled in and it is
  staged, then P5. `TestPhoneLifecycle/reset` walks this path.
- *(W3, found in e2e)* **Firmware fetched before any config fetch is
  nobody's.** The agent attributes a file without a MAC in its name (a
  load's `.loads` and `.sbn`) by its source address, and only to a device
  that fetched its config, or registered, from that address. A phone that
  downloads firmware before it fetches its config would not be seen
  Upgrading. The 79x5 reads its config first to learn its load (bench: §8).
- *(W3)* **A restarted controller reports what it finds in `pbx-config`**
  (6.2.1, "how the device reconcilers read the compiled index"): if that
  bundle was written by hand and refused, devices show `ApplyFailed` for
  it until the controller's own compile replaces it, moments later.
