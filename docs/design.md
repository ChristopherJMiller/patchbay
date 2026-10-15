# patchbay: a Kubernetes-native PBX — design

**Status:** approved design (Chris, 2026-10-14) · **Date:** 2026-10-14 · **Repo (to be created):**
`github.com/ChristopherJMiller/patchbay` · **Supersedes:** stages 2–3 of
`2026-10-02-telephony-lab-design.md` (its stage 1, the voice VLAN, is built and
stays as is).

## 1. Intent

**What Chris asked for.**
- Continue the telephony lab now that the core switch carries the voice VLAN.
- Build it as its own project, `patchbay`, with a Helm chart, in a new repo
  under `ChristopherJMiller`.
- Make it product-grade: clean public interfaces and custom resources, but
  **scoped to the hardware in the house.** The audience is Chris.
- Adding a softphone should be as simple as creating one resource.
- Build the operator **as stateless and state-machine-driven as possible**.
  Phones are physical systems: we know the state we want them in, so the code
  should name the states, the edges between them, and the work on each edge.
- Write it in **Go**.
- The north star, from 2026-10-02: a general way to connect any PBX number to
  any audio system.

**Releases, in order:**
- **A: core calling.**
- **B: streams.**
- **C: phone extras.**

**Assumptions Chris confirmed.**
- Softphones work at home only (LAN or Wi-Fi). Nothing is exposed to the
  internet.
- There's no outside line (PSTN) and no AI in A–C.
- The repo is public, so Cisco firmware and secrets never land in it, even
  encrypted.

**Done when** (detail in §11):
- **A.** The Desk phone (`100`) and a softphone are each declared by one
  resource and reach `Registered` without hand edits. They call each other,
  with audio both ways and caller ID names. A change in git reaches the
  phone's screen without anyone touching the phone.
- **B.** Dialling a stream number plays live HDHomeRun audio, and holds a tuner
  only while someone listens.
- **C.** Wallpaper, ringtones, a speed dial, a Services menu and push
  messages on the Desk phone, with alerts (§10).

## 2. Starting state (2026-10-14)

| Thing | State |
|---|---|
| Voice VLAN | Built in stage 1. `eth2.20` is `192.168.20.0/24` with DHCP option 66 → `192.168.0.240`. The forward filter lets VOICE reach `.240` on 5060 tcp/udp, 10000–10099 udp, 69 udp and 8088 tcp, and drops everything else. Since 2026-10-13 the core switch carries it: port 1 is tagged 20, port 6 is untagged 20. |
| Desk phone | CP-7945G `SEPD0C78915E8C8` on switch port 6, at `192.168.20.100`. Still on **SCCP** `jar45sccp.9-4-2SR4-3`. It answers ping. Its web UI went quiet after one response; it is probably restarting while looking for config at `.240`. |
| Second phone | CP-7945G `SEPD0C78915EBB5`, still on the main LAN (`.61`). Chris will connect it near the end of release A. |
| Cluster | No PBX deployed yet. Node `top` (`.240`) has no Talos host firewall, so ports on the host network are reachable. |
| Firmware | Not obtained yet. See §12. |

**Correction to the lab spec.** Lab spec §7 step 3 says to set the phone's
*Admin. VLAN ID* to `20`. With the switch, that is wrong: port 6 is an untagged
access port, so the phone must **not** tag and Admin VLAN stays blank. The lab
spec's §4.1 "interim weakness" (any phone can choose its own VLAN) is resolved,
because the switch now assigns the VLAN per port.

## 3. Decisions

| Decision | Choice | Why, and what was rejected |
|---|---|---|
| Language/framework | **Go + controller-runtime** (kubebuilder layout) | The framework's contract is one level-triggered `Reconcile`: observe, decide, act, requeue. That is the shape this design wants, and it's what Metal3 and Cluster API are built on. Go also has libraries for TFTP with per-transfer hooks and for AudioSocket. CRDs are generated from the Go types, `envtest` tests against a real API server, and the result is a small static binary. **Rust/kube-rs** has better enum exhaustiveness but no `envtest`, and thin SIP/AudioSocket/AMI libraries. **Python/kopf** is organised around events rather than level-triggering, with schemas written by hand. |
| Topology | **Controller + agent** (§5) | Exactly one component decides and writes status; the agent senses and acts. **Everything in the Asterisk pod** puts the operator in the call path, with cluster credentials on a host-network pod, and it goes down with Asterisk. **Coordinating only through resources** means two writers per status, and actions-by-field get awkward. |
| How config reaches Asterisk | **Generated files plus reload** | Reloads don't drop calls, and the result is easy to inspect. **Realtime/DB** would need Postgres on the SMR disks. **ARI dynamic push** keeps state in Asterisk's memory, so it's lost on every restart. |
| Who deploys Asterisk | **The Helm chart** | There is one PBX per install, so a `PBX` resource would be ceremony. |
| PBX engine | **Asterisk 22 LTS, PJSIP** (`andrius/asterisk`, digest-pinned) | Carried over from the lab spec §3, which also records why kubevoip, FreeSWITCH, FreePBX and SCCP were rejected. |
| State in the API | **Conditions; `phase` for display only** | Kubernetes API conventions: a published phase list becomes a contract that new states break. |

## 4. Resources

API group `patchbay.chrismiller.xyz/v1alpha1`. Every resource is namespaced, in
the PBX's namespace; the controller watches only that namespace. The rule is
**one resource per dialable thing, and it carries its own number.**

```yaml
kind: Phone                      # provisioned hardware
metadata: {name: desk}
spec:
  model: cisco-7945
  mac: D0C78915E8C8
  number: "100"
  label: Desk
status:                          # controller-written only
  state: Registered              # display summary; tools read conditions
  conditions: [Provisioned, FirmwareCurrent, Registered, ConfigCurrent]
  observed:
    firmware: SIP45.9-4-2SR4-3S
    address: 192.168.20.100
    lastConfigFetch: {version: 7c1e…, at: …}
---
kind: Softphone
metadata: {name: chris-laptop}
spec: {number: "150", label: "Chris (laptop)"}
status:
  provisioningURL: http://192.168.0.240:8088/p/<one-time token>
  conditions: [Registered]
---
kind: Person                     # dialling it rings all of that person's phones
metadata: {name: chris}
spec: {number: "301", name: Chris, rings: [{phone: desk}, {softphone: chris-laptop}]}
---
kind: Stream                     # release B (§9)
metadata: {name: king5}
spec: {number: "705", name: KING 5, source: http://192.168.0.58:5004/auto/v5.1}
```

**Numbers.**
- A number is a string of digits, quoted in YAML (`number: "100"`). A string
  keeps leading zeros, and YAML 1.1 would read a bare `010` as octal 8. The API
  server rejects a bare integer.
- On every pass the controller builds the full number table from all
  resources. That build is a pure function.
- If two resources claim a number, the older one (by `creationTimestamp`) keeps
  it. The newer one gets `NumberConflict=True` and is not routed.
- A separate `Extension` resource (number → target reference) was considered
  and deferred. It doubles the objects in the common case, and nothing needs
  several numbers for one target yet.

**Number plan.** This lives in chart values and the controller enforces it. The
CRD schema only checks that a number is digits.

| Range | Meaning | Release |
|---|---|---|
| `1XX` | Phones and softphones (Desk = `100`, softphone = `150`) | A |
| `2XX` | Intercom / auto-answer | reserved |
| `3XX` | People | A |
| `600` | Echo test (built in) | A |
| `7XX` | Streams | B |
| `0`, `9…`, `*8` | Assistant, outside line, page-all | reserved: plays "not available" |
| `911` | Plays "this phone cannot call emergency services" (built in). No PSTN exists, and a clear message beats a dead line. | A |

**Secrets are generated, not committed.**
- The controller creates the secrets, each one in a Secret that the
  resource owns:
  - one random SIP password per `Phone` and per `Softphone`. The length is a
    driver rule. The 79x5 reportedly fails with 32-character passwords and is
    known to work at 12 or fewer, so `cisco79x5` uses **12 alphanumerics**
    (about 71 bits; LAN-only, no trunk). `softphone` uses 24. The exact Cisco
    limit is a bench test (§8.6);
  - each softphone's provisioning token;
  - the shared AMI password and agent token.
- Git holds intent; the cluster holds what's derived from it. There is no
  `sign.sh` or SealedSecret step.
- **A lost Secret heals itself.** The controller generates a new password, the
  config version changes, and the phone is re-provisioned with no human
  involved (a softphone needs its QR code re-scanned).

**Models are drivers in Go code, not data.** `spec.model` selects a driver:

```go
type Driver interface {
    Models() []string                                      // "cisco-7945", "cisco-7965", …
    Render(dev Device, ctx RenderContext) (Files, error)   // pure
    Decide(dev Device, obs Observations, now time.Time) Decision  // pure, §8
    Firmware() []FirmwareFile                              // name + SHA-256 manifest
}
```

The first drivers are `cisco79x5` (7945/7965/7975, one firmware family) and
`softphone` (Linphone remote provisioning). Another vendor later means another
driver, with no resource changes. Phone extras (buttons, speed dials, wallpaper,
ringtones) are added to `Phone.spec` in release C (§10).

**Each driver carries a capability profile as Go data, not a CRD.** Nobody
authors phone models, so a `PhoneModel` resource would be ceremony.

The profile records:
- **Hardware:** line keys, softkeys, display and XML-service area, wallpaper
  size, ringtone format, codecs.
- **Provisioning facts:** file names, the firmware load name, label limits,
  the password policy.
- **A feature table per signalling backend** (§8.5). A spec that asks for a
  feature the active backend can't deliver gets `Unsupported`, never a dead
  key.

`patchbay models` prints the profiles.

**The 7945 facts that shape release A** (from the 2026-10-14 research; sources
are linked in the research notes in `docs/research/`):
- **Labels.** `phoneLabel` is at most 12 characters with no spaces. The
  driver derives it from `label`: spaces are removed, then it's truncated to
  12; a longer or spaced label gets a warning condition. The line label and
  caller ID name use `label` as written (≤ 30).
- **Registration** needs line `proxy=USECALLMANAGER`, `registerWithProxy`,
  **TCP** (`transportLayerProtocol=1`), and on the Asterisk side
  `force_rport=no` and `rewrite_contact=no`. Caller ID updates need
  `remotePartyID=true` with `send_rpid=yes`.
- **Time.** NTP by **IP address** (`192.168.20.1`). The time zone is a
  Windows-style name; the driver maps the IANA zone from settings, e.g.
  `America/Los_Angeles` → `Pacific Standard/Daylight Time`.
- **Codecs.** G.722 between phones (`advertiseG722Codec`), G.711µ otherwise.
  DTMF is RFC 4733 on payload 101.
- **PC port is off by default** (`pcPort=1`). A computer behind the phone
  would land in VLAN 20 on the untagged access port.
- **Access.** Web UI on (read-only, and it's a sensor, §8.2); SSH off;
  settings menu restricted.
- **Dial template.** A `dialplan.xml` is generated from the number plan, so
  complete numbers dial at once instead of after the phone's 15 s timeout.

## 5. Config and observation flow

```
  resources (Phone, Softphone, Person, Stream)
        │
        ▼
  controller ── Deployment, any node, the only decider and the only status writer
     │  • Compile() → ConfigMap `pbx-config` (placeholders, no secrets)
     │  • per resource: driver.Decide() → status, actions
     │  ▲ GET /v1/observations         │ POST /v1/actions/…
  ───┼─┼──────────── PBX pod on `top`, hostNetwork ───┼─────────────
     │ agent (`patchbay agent`, native sidecar) ◀──────┘
     └▶  • watches pbx-config and its Secrets; applies (below)
         • TFTP :69 and HTTP :6970 (the same files), logging every transfer
         • device HTTP :8088 (directory, softphone provisioning, XML apps)
         • scrapes each phone's own web pages by IP (§8.2)
         • control API :9440 (token)
         • AMI client on 127.0.0.1 → asterisk
       asterisk: SIP 192.168.0.240:5060 udp/tcp, RTP 10000–10099
              ▲
         phones on VLAN 20, softphones on the LAN
```

**Compile.** `Compile(resources, settings) → Bundle` is a pure function.
- The bundle holds every file:
  - **every Asterisk config file it loads.** The compiler owns
    `/etc/asterisk` outright, `modules.conf` uses `autoload=no` with an
    explicit module list, and none of the image's stock configs are used;
  - each phone's files (`SEP<MAC>.cnf.xml`);
  - the directory XML;
  - each softphone's provisioning XML.
- Passwords appear only as `@@secret:<name>/<key>@@`, so the ConfigMap is safe
  to read in full.
- `version` = SHA-256 over the files plus the values of the secrets they
  reference. A regenerated password produces a new version even though no text
  changed.
- Tested with golden files.

**Apply (agent).** It works like a commit-confirm:
1. Resolve the placeholders into a fresh directory. If any placeholder can't be
   resolved, refuse the whole version.
2. Swap the directory in atomically (symlink).
3. Over AMI: `module reload res_pjsip.so`, then `dialplan reload`.
4. **Verify** that the endpoints and dialplan contexts Asterisk reports match
   the bundle.
5. On a mismatch, swap the previous directory back, reload, and report
   `applyFailed: <reason>`.

The last good config keeps running. TFTP and HTTP serve from the same live
directory.

**Startup ordering.** The agent is a native sidecar: an init container with
`restartPolicy: Always`, GA since Kubernetes 1.29. Its startup probe passes once
the first version has been applied, so Asterisk starts only after valid config
exists.

**Observations.** `GET /v1/observations` returns:
- **the agent:** applied version, last apply error, whether Asterisk is up, and
  the Asterisk version;
- **each device:** a ring buffer of TFTP and HTTP fetches (file, time, result,
  source IP), and its registration from AMI (contact address, User-Agent,
  expiry);
- **unknown MACs** that requested `SEP<MAC>.cnf.xml`.

The controller polls every few seconds and enqueues a reconcile for every
resource whose observations changed. Anything that must survive goes into
resource status, written by the controller: last config fetch, firmware seen,
last action with its time and target version. The agent's memory is disposable.

**Actions.**
- `POST /v1/actions/resync {device}` sends Cisco's
  `NOTIFY Event: service-control` with body `action=restart` and the contact's
  `RegisterCallId`, via `PJSIPNotify`. This is CUCM's "apply config": the phone
  re-reads its files.
- `POST /v1/actions/restart {device}` sends the same with `action=reset`, a
  full reboot.
- That stock PJSIP can send this is a bench test (§8.6). If it can't, the
  state machine falls back to asking a human.
- Rate limits are derived from status. For example, no second resync of the
  same version within 5 minutes.

**Discovery.** An unknown MAC fetching config gives a Kubernetes Event:
`unknown phone D0C7… requested config from 192.168.20.101`. It is also listed
by `patchbay status`.

**Security.**
- AMI binds `127.0.0.1` only, with a generated password. Asterisk's HTTP and
  ARI are disabled.
- The control API needs the generated token and carries no secrets. It uses
  plain HTTP inside the LAN for now.
- The PJSIP ACL accepts registrations only from `192.168.20.0/24` and
  `192.168.0.0/24`. There is no anonymous endpoint, no NAT rule and no trunk.
- **Known Cisco limitation, accepted:** `SEP<MAC>.cnf.xml` carries the line
  password in clear over TFTP, which only VOICE and the LAN can reach.
  Revisit before any trunk exists.

## 6. Repo, packaging, CLI, and luma-homeops

**Repo layout.**

```
cmd/patchbay/                one binary: controller | agent | stream-bridge | CLI
api/v1alpha1/                resource types → generated CRDs
internal/compile/            pure: resources + settings → Bundle   (golden tests)
internal/drivers/cisco79x5/  templates, firmware manifest, Decide  (table tests)
internal/drivers/softphone/
internal/agent/              TFTP, device HTTP, AMI client, apply/verify/rollback
internal/controller/         controller-runtime reconcilers        (envtest)
internal/streambridge/       release B
charts/patchbay/             CRDs (in templates/ so upgrades apply them), controller,
                             PBX pod, stream-bridge, RBAC, PodMonitor
test/e2e/                    kind + real Asterisk + simulated phone
docs/                        this spec, runbooks, Cisco migration guide
```

**Simulated phone** (`test/e2e/fakephone`). A Go test client that behaves like
a 79x5 at the protocol level:
- fetches `SEP<MAC>.cnf.xml` over TFTP;
- registers with the credentials in it;
- answers a `service-control` NOTIFY (`action=restart`) by fetching again;
- can serve fake `/DeviceInformationX` and `/NetworkConfigurationX` pages, so
  the agent's scraper has a target;
- can place and answer calls.

Each fake phone can be scripted into the failure modes the state machine
handles: SCCP firmware that never registers, asking for `.sgn`, a stalled
upgrade, going silent. That way every `Decide` row also gets an end-to-end
test, not only a table test.

The e2e suite drives the whole loop in kind: resource → compile → agent →
Asterisk → `Decide` → status. The real 7945 is only the final acceptance test.

**CI and releases** (GitHub Actions).
- On every push: `go test`, `envtest`, `golangci-lint` with the `exhaustive`
  linter (state switches must be complete), and e2e in kind.
- Image: `ghcr.io/christopherjmiller/patchbay` for amd64 and arm64.
- Chart: published to GitHub Pages by chart-releaser, the setup `helm-mega-media`
  already uses with this Argo.

**CLI.** The same binary as a client, with only a kubeconfig needed. It can be
installed as `kubectl-patchbay`.

| Command | Does |
|---|---|
| `patchbay firmware load <zip>` | Checks each file in the zip against the driver manifest (SHA-256) locally. Uploads only matching files to the agent through the Kubernetes API (pods/portforward), so no LAN access is needed. The agent re-verifies before writing to the firmware volume. |
| `patchbay status` | Every device: state, conditions, any human action needed, and unknown phones. |
| `patchbay softphone qr <name>` | Prints the provisioning QR code in the terminal. `--reissue` requests a fresh token. |
| `patchbay phone resync <name>` | Sets a request annotation that the controller acts on. Only the controller decides. |

**Chart contents.**
- **Controller:** Deployment, 1 replica with leader election.
- **PBX:** Deployment, `replicas: 1`, `strategy: Recreate`, `hostNetwork: true`,
  `dnsPolicy: ClusterFirstWithHostNet`, with a `nodeSelector` from values.
  - containers: `asterisk` plus the `agent` native sidecar;
  - a shared emptyDir for live config, written by the agent and read by
    Asterisk;
  - an `fsGroup` so the `asterisk` user can read it;
  - a Service selecting the PBX pod for the control API.
- **Firmware PVC:** `firmware`, 1 Gi, RWO. The pod is pinned, so RWO is fine.
- **Ports bound on the node:** 5060, 10000–10099, 69, 6970, 8088 and 9440.
  None of them clashes with Talos or Kubernetes (kubelet and the control plane
  use 10248–10259). The plan verifies this on `top` before the first sync.
- **About 6970:** 9.x Cisco phones may try HTTP on TCP 6970 before falling
  back to TFTP. Stage 1's firewall silently drops 6970, which would make every
  file wait out a timeout.
  - If the router's drop log shows the phone trying 6970, add one Ansible rule
    allowing VOICE → `.240` tcp 6970, deployed with commit-confirm.
  - Otherwise, change that drop to a reject.

  This is decided by bench test (§8.6).

**In luma-homeops.**
- **Argo Application `telephony`**, multi-source:
  - source 1 is the chart from `https://christopherjmiller.github.io/patchbay`
    (chart-releaser's GitHub Pages index), with site values:
    - `pbx.nodeName: top`, `pbx.address: 192.168.0.240`;
    - `timezone: America/Los_Angeles`;
    - NTP `192.168.20.1` for the voice VLAN;
    - the number plan;
  - source 2 is the path `cluster/telephony/`, holding the resources:
    `Phone desk`, `Softphone chris-laptop`, `Person chris`.
- **Namespace** `telephony`, with `pod-security.kubernetes.io/enforce:
  privileged` via `managedNamespaceMetadata`, as for `music-streaming`.
- **Router.** Stage 1's rules already cover 5060, RTP, 69 and 8088. The
  control API (9440) is LAN-only, because the VOICE forward filter drops it.
  - Traffic from PBX to phone (phone RTP 16384–32766, HTTP 80 for scraping and
    push, RTPRx) is LAN → VOICE, which the forward default already allows.
  - The only possible router change is tcp 6970 (§6, chart contents).
- **The lab spec** gets a "superseded by patchbay" header.

## 7. When things fail

| Down | Calls | What status shows |
|---|---|---|
| controller | keep working on the last applied config | stops updating; new resources wait |
| agent | keep working (Asterisk keeps its files) | `Registered=Unknown`, reason `AgentUnreachable`. A rebooting phone uses its cached config if it has one. |
| Asterisk | none | `Registered=False`, reason `PBXDown`, on every device |
| node `top` | none | as for Asterisk until the pod returns. Accepted: phones depend on `top`, including during Talos upgrade hops. |
| bad config | unaffected (rollback, §5) | `ConfigCurrent=False`, reason `ApplyFailed`, plus an Event |

- **Calls survive** config changes, which reload rather than restart.
- **Calls in progress drop** when the chart or Asterisk is upgraded, because
  the PBX pod is recreated (host ports can't overlap). Fine for a house.
- **Metrics.** Both the controller and the agent export Prometheus metrics:
  - devices by state;
  - registered endpoints;
  - applied config version;
  - apply failures;
  - observation age.
- **Scraping.** The chart's PodMonitor carries `release: prometheus`, without
  which this Prometheus ignores it.
- **Alerts.** These ship as `PrometheusRule` resources in release C.

## 8. Device lifecycle: the `cisco79x5` and `softphone` state machines

### 8.1 Shape

```go
Decide(spec, observations, status, settings, now) → Decision{
    State, Conditions, Actions, StatusPatch, HumanAction, RequeueAfter }
```

- `Decide` is pure and has no I/O.
- Each driver's machine is an **ordered rule table**, held as data: a Go slice
  of `{state, predicate, outcome}`. The first match wins.
- Bench findings refine predicates without restructuring code. Every row is a
  table-test case, and the `exhaustive` linter keeps state switches complete.
- **Memory lives in status fields only:**
  - `observed.firstSeen`, `observed.firmware`;
  - `observed.lastConfigFetch{version, at}`;
  - `observed.lastRegistered`, `observed.upgradeStartedAt`;
  - `lastAction{kind, version, at, attempts}`.
- **Conditions:**
  - `Ready` (the summary);
  - `FirmwareAvailable`, `FirmwareCurrent`;
  - `Provisioned` (it fetched the current config);
  - `Registered`, `ConfigCurrent`;
  - plus `Unsupported` / `NumberConflict` when they apply.
- `status.state` is for display. `status.humanAction` holds the exact
  instruction whenever a person must act. `patchbay status` shows both.

### 8.2 Sensors

Every sensor runs in the agent; the controller sees them only as
observations.

1. **Fetch log.** TFTP 69, HTTP 6970 and HTTP 8088. Records file, result
   (served / not found / error), time and source IP.
2. **AMI.** Contact, User-Agent (`Cisco-CP7945G/9.4…` identifies SIP
   firmware), Call-ID (needed for service-control) and expiry. Also qualify
   status, if 79x5 answers OPTIONS (bench).
3. **The phone's own web pages,** fetched by the agent from the phone's IP.
   The IP comes from the fetch log, so this works before the phone registers.
   - `/DeviceInformationX`: app/boot load ID, hardware revision, serial.
   - `/NetworkConfigurationX`: admin VLAN (it **must be empty** on the
     untagged port), and "Unified CM 1: … Active".
   - `/DeviceLogX`: status messages. Trust and config errors surface here.

   Polled every 60 s while not Ready, every 5 min once Ready. This is how
   SCCP firmware is told apart from SIP: an SCCP phone never sends a SIP
   REGISTER.
4. **Firmware store.** Which files in the driver manifest are present on the
   PVC with a matching SHA-256.

### 8.3 `cisco79x5`: rows in order (first match wins)

| State | Predicate | Outcome |
|---|---|---|
| `Invalid` | The spec breaks a driver rule (unknown model, unsupported feature for the backend), or `NumberConflict` | No actions. Conditions say why. |
| `Unknown` | Agent unreachable, observations stale, or Asterisk down | No actions. `Registered=Unknown`, reason `AgentUnreachable` / `PBXDown`. Last known state kept. |
| `Unseen` | No fetch from this MAC ever, and no contact | humanAction: "connect it to a VLAN 20 switch port; it will request SEP\<MAC\>.cnf.xml from 192.168.0.240". Requeue slowly. |
| `NeedsFirmware` | It has fetched config; its web page shows a load other than the target (e.g. `jar45sccp…`); `FirmwareAvailable=False` | humanAction: "run `patchbay firmware load <zip>`", plus the list of missing files. |
| `NeedsTrustReset` | It requests `SEP<MAC>.cnf.xml.sgn` (signed config) and makes no progress within 3 min, or `/DeviceLogX` shows a trust/verification error | **If `FirmwareAvailable`:** "factory reset: hold # while powering on, then 123456789\*0#". **Otherwise:** "stage firmware first". A factory reset erases the phone application and boots whatever `term45.default.loads` TFTP serves, so a reset with nothing to download leaves the phone stuck. Staging the SIP bundle's `term45.default.loads` makes the reset land straight on SIP. |
| `Upgrading` | A firmware file (`.sbn`/`.loads`) was fetched within 3 min; `upgradeStartedAt` is recorded on first sight | humanAction: "upgrading: do not unplug". No actions; never resync or restart mid-upgrade. After 20 min → humanAction "upgrade stuck: check the phone's screen". |
| `Registering` | The current config has been fetched, the target load is running, and there is no contact | Wait. After 5 min → humanAction with the latest `/DeviceLogX` messages. |
| `ConfigStale` | Registered, but `lastConfigFetch.version` ≠ this phone's compiled version | Action `resync`, at most once per version per 5 min. After 2 attempts → humanAction "press Settings then \*\*#\*\*, or power-cycle it". |
| `Registered` | Registered, config current, firmware current | `Ready=True`. Requeue slowly. |
| `Unreachable` | It was registered, the contact has expired (or qualify fails), past a 10 min grace | humanAction: "not responding; power-cycle it". Some users report 7945 SIP firmware locking up after about 2 days. |

The target load is `Phone.spec.firmware` when set, otherwise the driver default
`SIP45.9-4-2SR4-3S`.

### 8.4 `softphone`

| State | Predicate | Outcome |
|---|---|---|
| `Invalid` | `NumberConflict` | Says why. |
| `Unknown` | Agent unreachable or Asterisk down | As for phones. |
| `Issued` | A token exists and hasn't been fetched | `status.provisioningURL` is set; `patchbay softphone qr` shows it. |
| `Provisioned` | The token's URL was fetched and there is no contact yet | Action `rotateToken`, so each QR works once. Waiting to register. |
| `Registered` | Contact present | `Ready=True`. |
| `Offline` | It was registered and has no contact now | Normal: mobile apps sleep. `Ready=True`, `Registered=False` with reason `Offline`. It is not an error. |

`--reissue` (an annotation) mints a new token and moves the softphone back to
`Issued`.

### 8.5 Signalling backend

- One install-wide setting, `pbx.signalling`. Only `pjsip` (stock Asterisk 22
  LTS) is implemented.
- **What needs Cisco-proprietary SIP that stock PJSIP lacks**, all marked
  `pjsip: unsupported` in the 79x5 profile:
  - BLF lights;
  - the Park and Conference softkeys (out-of-dialog `x-cisco-remotecc`
    REFERs);
  - DND and call-forward sync with the phone (PUBLISH);
  - auto-answer intercom;
  - registering a second line (CUCM mode registers line 1 only).
- **Works on stock PJSIP:**
  - hold and resume;
  - transfer (bench);
  - pickup and MeetMe via dialplan (`x-cisco-serviceuri-*` INVITEs);
  - distinctive ring via `Alert-Info`;
  - G.722, RFC 4733 DTMF, and caller ID via Remote-Party-ID.
- **Possible later backend:** the usecallmanager.nz patch, which re-adds a
  modernised `chan_sip` with all of the above. Its Asterisk 22 build is "ready
  for testing" and exists only for exact upstream versions, so it's not
  adopted. Because the backend is a setting, adopting it later changes no
  resource.

### 8.6 Bench tests

These run in the first real-phone session of release A. Each answers a
question the documentation leaves open, and the answer goes back into the rule
table or the templates.

| Test | Decides |
|---|---|
| Does the phone request `.cnf.xml.sgn` (an old trust list)? | Whether `NeedsTrustReset` fires; the reset path |
| Does it try tcp 6970 first (router drop log)? | Router allow vs reject (§6) |
| Does service-control NOTIFY via `PJSIPNotify` make it re-read config? | Whether `ConfigStale` can self-heal |
| Highest password length that registers | `cisco79x5` password policy |
| Does it answer OPTIONS (qualify)? | `Unreachable` detection speed |
| Blind and attended transfer on stock PJSIP | Release A exit criterion |
| `RTPRx` push on SIP 9.4(2) | Whether release C gets audio without a call |

## 9. Release B: streams

- **Routing.** A `Stream`'s number compiles to
  `Answer()` → `AudioSocket(<uuid>, patchbay-stream-bridge.<ns>.svc:9092)` →
  "unavailable" if the bridge refuses. The UUID is
  `uuid5(fixed namespace, stream name)`, so it stays stable across renders. The
  bundle carries `streams.json` (`uuid → {name, source}`); the bridge never
  reads resources.
- **The bridge.** `patchbay stream-bridge` is an ordinary Deployment (not
  hostNetwork) behind ClusterIP Service port 9092. It works as lab spec §6
  described:
  - an unknown UUID gets the connection closed;
  - **one ffmpeg per source**, shared by everyone listening:
    `ffmpeg -i <source> -vn -ac 1 -ar 8000 -f s16le pipe:1`;
  - 320-byte frames fanned out to **bounded** per-caller queues; a slow caller
    drops frames rather than stalling the others;
  - per-caller pacing at 20 ms on a monotonic clock;
  - audio from Asterisk is discarded;
  - **ffmpeg is killed 5 s after the last caller leaves**, which frees the
    tuner;
  - if the source fails (both tuners busy, say), the session's callers are
    closed and the dialplan plays "unavailable".
- **Status.** The bridge exposes observations (listeners per source, last start
  error). The controller decides each `Stream`'s state (`Idle`,
  `Playing (n listeners)`, `Failing: <reason>`). The controller never probes a
  source, because a probe would tie up a tuner.
- **Audio.** AudioSocket's baseline is 8 kHz narrowband. The plan checks whether
  Asterisk 22 carries higher sample rates over AudioSocket before assuming
  either way. Expect 1–3 s of silence while a tuner locks.
- **Extension point, not built.** A future `AudioService {number, address}`
  routes a number to any AudioSocket service, two-way. That's where an AI agent
  plugs in. `Stream` is the special case served by patchbay's own bridge.
- **Tests.**
  - AudioSocket frame encode/decode (ID, audio, hangup, error kinds);
  - session lifecycle against a fake source: join, fan-out, grace teardown,
    source failure;
  - pacing within tolerance;
  - e2e: the simulated phone dials a stream fed by a test tone and receives
    audio.

## 10. Release C: phone extras

Everything here is validated against the driver's capability profile and the
active backend (§8.5).

- **`Phone.spec.buttons[]`** is a typed union:
  - `line` (button 1 only on PJSIP);
  - `speedDial {number}`;
  - `serviceURL {url}`;
  - `feature {name}` (Redial, DND, Pickup, …).

  The 7945 has 2 line keys. Patch-only kinds (`blf`, `park`, `intercom`)
  exist in the schema, but they are refused with `Unsupported` on `pjsip`.
- **Wallpaper.** `spec.wallpaper` references an image (ConfigMap or URL). The
  compiler renders the 320×212 PNG and its 80×53 thumbnail, plus
  `Desktops/320x212x16/List.xml`.
- **Ringtones.** `spec.ringtones` references audio. ffmpeg converts it to raw
  µ-law, 8 kHz mono, ≤ 2 s; the compiler writes the ring list. The exact list
  filename the 9.4(2) firmware asks for is read from the fetch log.
  Distinctive ring per caller uses `Alert-Info`.
- **Services menu** (`servicesURL`, served by the agent on 8088):
  - Streams: pick one and it dials;
  - phone status;
  - people.

  XML services need `text/xml`. A pushed object can be at most 512 bytes;
  anything larger is pushed as an Execute pointing at a URL.
- **Push.** `patchbay phone message desk "Dinner!"` →
  `POST /CGI/Execute`. The phone checks credentials against
  `authenticationURL`, which the agent answers.
- **Audio without a call.** `RTPRx` / `startMedia` push for announcements and
  paging, **only if the bench test passes** (§8.6).
- **Call quality.** The agent scrapes `/StreamingStatisticsX` during calls and
  exports it as metrics.
- **Alerts** as `PrometheusRule`:
  - phone not Ready for more than 15 min;
  - apply failures;
  - agent unreachable.

**Exit criteria (C):**
- The Desk phone shows the wallpaper and plays a custom ringtone.
- Button 2 is a working speed dial.
- The Services menu dials a stream.
- `patchbay phone message` shows on screen.
- Alerts fire in a forced-failure test.
- RTPRx paging works, if the bench test passed.

## 11. Releases and exit criteria

Each release ends at a check, and nothing moves on until it passes.

**A: core calling.**
- **CI.** Green: unit, golden, table, envtest, and the e2e loop with the
  simulated phone.
- **Deployed.** The Argo app `telephony` is Healthy and the PBX pod is Running
  on `top`.
- **Firmware.** `patchbay firmware load` accepts the Cisco zip and rejects a
  file with a wrong checksum.
- **Bench tests.** Every test in §8.6 has been run, and its answer is
  recorded in the rule table, the templates or the router.
- **Dial template.** Dialling `600` on the Desk phone connects at once, with
  no 15 s wait.
- **The Desk phone**, after any human step `patchbay status` asks for
  (staging firmware, a factory reset):
  - walks the state machine to `Registered` with `FirmwareCurrent` and
    `ConfigCurrent` and no other human step;
  - shows the label "Desk" and the correct time.
- **Softphone.** Scanning `patchbay softphone qr chris-laptop` in Linphone
  registers it. The token is then spent: the same URL no longer serves.
- **Calls.**
  - `100`↔`150` works both ways, with two-way audio and caller ID names;
  - `301` rings both;
  - `600` echoes;
  - `911` plays its announcement;
  - reserved numbers play "not available".
- **The loop.**
  - Changing `desk`'s `label` in git puts the new label on the phone's screen
    with no hands on the phone.
  - Deleting `desk`'s Secret gives a new password, an automatic re-provision,
    and the phone re-registers.
- **Rollback.** Breaking the compiled config on purpose (in e2e) leaves the old
  config live and reports `ApplyFailed`.
- **Second phone.** Plugged into a VLAN 20 switch port, it appears as
  "unknown phone" before any resource exists. After a `Phone` resource is
  written, it reaches `Registered`. Phone↔phone calls and transfer work.

**B: streams.** These are lab spec §8 stage 3, unchanged.
- Dialling `705` plays KING 5 within about 3 s.
- Two phones on `705` use **one** tuner (`http://192.168.0.58/status.json`).
- Both hang up, and the tuner is free within about 10 s.
- With both tuners busy, the caller hears "unavailable" and the `Stream` shows
  `Failing`.

**C: phone extras.** See §10.

## 12. Firmware

Researched 2026-10-14.
- **Target load:** `cmterm-7945_7965-sip.9-4-2SR4-3` (`SIP45.9-4-2SR4-3S`,
  8 files, about 10.5 MB). `9-4-2SR3-1` is an acceptable fallback.
- **Path from SCCP:** the phone can go straight from SCCP `9.4.2SR4-3` to SIP
  `9.4.2`, with no intermediate load.
- **Triggering it:**
  - the SEP file's `<loadInformation>` names the load, without `.loads`;
  - after a factory reset, the phone fetches `term45.default.loads`, so the
    SIP zip's copy must be served;
  - the reset also clears the old CUCM's ITL/CTL trust lists.
- **Integrity:** 79x5 phones verify Cisco's signature on every image, so a
  tampered file fails with "Load Authentication Failed". The driver manifest
  still pins SHA-256s so we serve exactly the expected load.
- **Getting it:**
  - Contracts for these phones ended in 2022 and support in 2023, and used
    equipment was never eligible.
  - A free Cisco account worked in 2019. Whether it still works after end of
    support is unverified, and Chris is trying it.
  - Unofficial copies exist, but they are a licensing grey area and Chris's
    call.
- **Storage:** never in git, encrypted or not. Git-crypt in a public repo
  still publishes the ciphertext, path and size permanently. The firmware lives
  only on the `firmware` PVC, and the repo carries only the manifest.

## 13. Out of scope

- PSTN trunk;
- AI agents (the `AudioService` extension point in §9 is where they'd plug in);
- voicemail and MWI;
- paging and intercom (`2XX`, `*8`);
- TLS/SRTP;
- softphones away from home;
- more than one PBX per install;
- moving the PBX off a single pinned node;
- a Grafana dashboard.
