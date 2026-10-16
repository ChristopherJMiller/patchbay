# Release B ("streams"): implementation plan

**Status:** plan, 2026-10-16. **Spec:** [docs/design.md](../design.md) §9
(release B), §11 (B's exit criteria), §4 (the Stream resource, `7XX`).
**Binding decisions by Chris (2026-10-16):** the house's sources, the tuner
policy, the time limit, and that the AudioSocket sample rate is answered by
test (§1.2). **Contracts landed with this plan:** commits `467ac09`,
`b60841b`, `9a07b6b`, `1b802ef`, `c011896`, `f0ddfbc`, `838541e` (§4).
Read [CLAUDE.md](../../CLAUDE.md) first; release A's plan,
[docs/plans/release-a.md](release-a.md), is the model this one follows.

Release B is one workflow: three stages in parallel, in one working tree,
each in its own files, then the integrator. Each stage is implement →
review → fix.

| Stage | Builds | Touches |
|---|---|---|
| **bridge** | `patchbay stream-bridge`: the AudioSocket server, the session manager (one ffmpeg per source, shared), pacing, the tuner limit, observations, metrics | `internal/streambridge/` only; no cluster |
| **routing** | compile's stream dialplan and streams.json, the stream settings' validation, the controller writing `<fullname>-streams`, the bridge client and poller, the Stream reconciler and its state table | `internal/compile`, `internal/controller/{bundle,settings,numbers,metrics}`, new `internal/controller/{bridgeclient,streamstate}` |
| **packaging + harness** | the bridge image (ffmpeg), the chart's bridge objects, CI and Renovate for the image, the e2e fake stream source and fakephone's stream checks, e2e helpers | `Dockerfile`, `Makefile`, `.github/`, `renovate.json`, `hack/`, `charts/`, `test/chart/`, `test/e2e/fakephone/`, `test/e2e/streamkit_test.go`, `test/e2e/values.yaml`, `docs/releasing.md` |
| **integrator** | wiring (`cmd/patchbay`, the controller root), `patchbay status`, the e2e tests, docs, CLAUDE.md, the luma-homeops manifests in docs | everything else |

Contents: [1 Goals](#1-goals-and-exit-criteria) ·
[2 Decisions](#2-decisions-made-now) ·
[3 The AudioSocket check](#3-the-audiosocket-sample-rate-check) ·
[4 Contracts](#4-contracts-landed) ·
[5 Packages and rules](#5-package-map-ownership-and-rules) ·
[6 Stage tasks](#6-stage-tasks) ·
[7 Interfaces](#7-interfaces-on-the-wire-and-in-the-cluster) ·
[8 Bench](#8-bench-what-only-the-real-hdhomerun-phone-and-somafm-answer) ·
[9 Exit criteria → tests](#9-exit-criteria--acceptance-tests) ·
[10 Review Focus](#10-review-focus) ·
[11 Known limitations](#11-known-limitations-accepted-for-b) ·
[12 The house's manifests](#12-the-houses-manifests-luma-homeops)

---

## 1. Goals and exit criteria

### 1.1 Design §11, B (verbatim)

- Dialling `705` plays KING 5 within about 3 s.
- Two phones on `705` use **one** tuner (`http://192.168.0.58/status.json`).
- Both hang up, and the tuner is free within about 10 s.
- With both tuners busy, the caller hears "unavailable" and the `Stream`
  shows `Failing`.

### 1.2 Chris's decisions (2026-10-16, binding)

- **Sources.** The HDHomeRun FLEX DUO (HDFX-2US) at `192.168.0.58`, with
  **2 tuners that Plex DVR also uses**. Stream URLs are
  `http://192.168.0.58:5004/auto/v<channel>`. The house ships `704` KOMO
  (4.1), `705` KING (5.1), `707` KIRO (7.1), `713` FOX13 (13.1): the number
  is 7 + the major channel. Plus **SomaFM internet radio** (no tuner), six
  popular channels on `75X`. *(Checked 2026-10-16, read only:
  `/discover.json` reports `HDFX-2US`, firmware `20250623`, `TunerCount`
  2; SomaFM's channels.json gives `.pls` playlists whose entries are direct
  Icecast mounts such as `https://ice2.somafm.com/groovesalad-128-mp3`; each
  of the six mounts in §12 answered 206 `audio/mpeg`.)* The site Streams
  belong in luma-homeops, not in patchbay's chart: patchbay ships samples
  and docs, and the integrator writes the exact manifests into
  docs/streams.md for the lead to copy (§12).
- **Tuner policy.** Phone streams hold **at most 1 tuner** at a time (the
  DVR always keeps one). Everyone on one channel shares one ffmpeg (§9). A
  call to a second, different tuner channel while one plays hears "not
  available", and that Stream reports why.
- **A per-call time limit**, default 2 h, a setting: Asterisk ends the
  call, so a phone left off the hook cannot hold the tuner for ever.
  Internet radio uses no tuner and has no tuner limit; the time limit still
  applies.
- **The AudioSocket sample-rate question** (§9) is answered by testing the
  pinned image, not assumed: §3.

### 1.3 Release B is done when

1. Every §1.1 criterion holds on the house's hardware ([real], §9), with
   §1.2's policy: "both tuners busy" is the DVR holding both (the phones
   can hold only one).
2. A call to a second HDHomeRun channel while one plays hears "not
   available"; that Stream shows `Busy` (Ready False `TunerBusy`, naming
   what holds the tuner) and keeps the refusal in `status.lastFailure`.
3. Every stream call ends at the time limit.
4. SomaFM plays on `750`–`755`, several at once, with no tuner limit.
5. CI is green (`ci-ok`), e2e included; the bridge image is published for
   amd64 and arm64 and the released chart runs it by digest.
6. docs/streams.md holds the site manifests (§12), and CLAUDE.md the
   deviations B makes from the design.

### 1.4 Fixed values

| What | Value | From |
|---|---|---|
| Stream numbers | the plan's `7XX` (`compile.NumberPlan.Streams`) | design §4 |
| Stream UUID | `uuid5(streams.Namespace, metadata.name)`; `streams.Namespace` = `uuid5(URL, "https://github.com/ChristopherJMiller/patchbay/streams")` = `211a92d2-913d-5c00-af69-82af81c3daf0` | design §9; plan |
| AudioSocket port | 9092 (`streambridge/api.DefaultAudioSocketPort`) | design §9 |
| Bridge control API | 9093 (`DefaultControlPort`): `/v1/observations`, `/healthz`, `/readyz`, `/metrics` | plan |
| Audio | 16 kHz signed linear mono, 640-byte 20 ms frames (`streams.Codec` `slin16`, `SampleRate`, `FrameBytes`) | §3 |
| Grace before ffmpeg is killed | 5 s (`streambridge.Grace`) | design §9 |
| Per-caller queue | 50 frames, 1 s, drop oldest (`QueueFrames`) | plan |
| Time limit | default 2 h, 10 s to 24 h, whole seconds (`compile.DefaultCallLimit`) | Chris; plan |
| House tuner pool | `hdhomerun`: host `192.168.0.58`, max 1 | Chris |
| Observation poll | 5 s; stale after 30 s (`install.PollInterval`, `StaleAfter`) | release A |
| Bridge objects | `<fullname>-streams`: Deployment, Service, ServiceAccount, and the ConfigMap holding streams.json | §2(i) |

### 1.5 Out of scope for B

The `AudioService` extension point (§9, not built); the Services menu that
dials a stream (release C); the stream's name on the phone's screen; a
prompt at the time limit; resolving `.pls`/`.m3u` playlists; recording,
rewinding or pausing; a second bridge replica; video.

---

## 2. Decisions made now

Each: the decision, why, and what it costs if it is wrong.

### (a) Streams reach the bridge through AudioSocket's channel driver, at 16 kHz

**Decision (landed in `1b802ef` and `467ac09`):** a Stream's number runs
`Dial(AudioSocket/<bridge>/<uuid>/c(slin16),,g)`, not design §9's
`AudioSocket(<uuid>, <bridge>)` application. Audio is 16 kHz signed
linear both ways (AudioSocket kind `0x12`, 640 bytes per 20 ms).
`chan_audiosocket.so` replaces `app_audiosocket.so` in the module list;
`codec_resample.so` and `func_timeout.so` join it.

*Why:* §3's test against the pinned image. The channel driver carries 16
kHz both ways, as documented, and over a G.722-only call a 6 kHz component
from the bridge arrives. The application sends 8 kHz to the server and
documents only 8 kHz; it did play the bridge's 16 kHz frames over a G.722
call, by switching the channel's write format, but that is not behaviour
it promises. The house's 79x5s talk G.722 to Asterisk (release A's
profile), so TV and radio can be wideband on the Desk phone (bench B5).
With `Dial(…,,g)` a refusal, a close, an error frame and an unreachable
bridge all continue the dialplan, `DIALSTATUS` saying which; the
application continues only after a hangup frame or a close, and needs
`TryExec` for the rest. *Cost if wrong:* the application is a one-line
dialplan change and a module swap (the bridge already sends 16 kHz, which
the application also takes).

### (b) streams.json is its own ConfigMap, mounted into the bridge

**Decision (landed in `c011896`):** compile puts the sealed
`streams.Document` in `Bundle.Streams` (not in `Files`, not in `Version`);
the bundle reconciler writes it as `<fullname>-streams` (key
`streams.json`) **before** pbx-secrets and pbx-config; the bridge mounts it
read-only and reads the file every 2 s. The bridge has **no** Kubernetes
credentials (`automountServiceAccountToken: false`).

*Why:* the bridge decodes untrusted network input with ffmpeg; it should
hold nothing worth stealing. A separate object means a source URL change
never rewrites pbx-config (no PBX reload) and the agent never learns about
streams. *Cost:* the kubelet updates a mounted ConfigMap on its sync
(usually under a minute, up to about two): a new Stream is `Pending`
(`BridgePending`) and plays "not available" until then. A watch would be
instant but needs an API token in the bridge; a push from the controller
needs a token of its own. Both are later options with no API change.

### (c) Tuner pools are settings, matched by a source's host

**Decision (landed in `1b802ef`, `f0ddfbc`):** settings
`streams.tunerPools: [{name, hosts, max}]`. A Stream whose source URL's
host (any port, case-insensitive) is one of a pool's hosts counts against
that pool; the bridge runs at most `max` of the pool's **sources** at once
(sessions, not calls); `status.tunerPool` and `kubectl get streams -o wide`
(column Pool) show the match. No field on Stream.

*Why:* the limit is a property of the device, not of each channel: four
Streams on one HDHomeRun share one limit, and a new channel needs nothing
but its URL. The pool host is where the tuners are, which is what a
Stream's URL already says. *Cost:* a Stream that names the device by
another host (a DNS name for the IP) escapes the pool. The Pool column and
docs/streams.md make it visible; listing both forms fixes it.

### (d) A source is a direct http(s) stream URL; playlists are refused

**Decision (landed in `b60841b`, `467ac09`):** `streams.CheckSource`, and
the same rules as CEL on `spec.source`: http or https only, a host, no
white space or control characters, no credentials (`user@`), and not a
playlist (a path ending `.pls` or `.m3u`, any case; HLS `.m3u8` is a
stream to ffmpeg and is allowed). The bridge never fetches playlists.

*Why:* ffmpeg reads streams, not `.pls` files, and resolving playlists
means a second HTTP client and parser on untrusted input for little gain:
SomaFM's `.pls` lists interchangeable `ice*` hosts of the same mount. The
CRD refuses the likeliest mistake (pasting channels.json's `.pls` link) at
`kubectl apply`, not after a failed call. *Cost:* if SomaFM retires the
host a Stream names, that Stream is `Failing` until its URL is edited.
Resolving `.pls` later is a bridge-only change plus relaxing one CEL rule
(ratcheting allows it).

### (e) The time limit is Asterisk's `TIMEOUT(absolute)`, with no prompt

**Decision:** the stream extension sets `TIMEOUT(absolute)` to the limit
(whole seconds) right after `Answer()`. *(Checked §3: with a 4 s limit
both the application and `Dial` end the call at 4.0 s and the dialplan
never falls through to "not available"; after `Dial` the `T` extension
runs if there is one.)*
patchbay defines no `T` extension, so the call simply ends.

*Why:* "Asterisk ends the call" (Chris); one place, no bridge state.
*Cost:* the listener gets no warning. A prompt is a new sound in every
voice (docs/sounds.md), a later change.

### (f) The bridge image is a second target of the one Dockerfile, on Alpine

**Decision:** the Dockerfile gains a `stream-bridge` stage before the
final (patchbay) stage: `alpine:3.24.<n>@sha256:…` + `apk add --no-cache
ffmpeg ca-certificates`, the same static patchbay binary, user 65532,
published as `ghcr.io/christopherjmiller/patchbay-stream-bridge` with the
same tags as patchbay, amd64 and arm64.

Measured 2026-10-16 (not installed in the repo):

| Base | ffmpeg | Added size | Notes |
|---|---|---|---|
| `alpine:3.24.2` | 8.1.2 (`--enable-gpl --enable-version3`, no nonfree, OpenSSL) | ~130 MB of libraries | chosen |
| `debian:trixie-slim` | 7.1.5 | 470 MB, 209 packages | GnuTLS |
| nixpkgs `ffmpeg-headless` (flake's pin) | 9.0.1 | 305 MiB closure | reproducible, but needs nix image builds and an arm64 builder |
| `alpine:3.22` | 6.1.2 | ~125 MB | too old |

*Why:* the smallest image with a current ffmpeg and a distribution's
security fixes; a plain `apk add` that the Dockerfile manager and Renovate
already understand; the corresponding source is Alpine's (aports at the
package's commit, and its distfiles), which docs/streams.md points to, as
GPL-3.0 §6 asks of whoever redistributes the image (who stays responsible
for it being available). One
Dockerfile keeps one Go builder pin (`make check-versions` unchanged). The
patchbay image stays distroless and ffmpeg-free (the PBX pod is on the host
network). *Cost:* the arm64 `apk add` runs under QEMU in CI (an unpack, a
few seconds); ffmpeg's patch level floats with the 3.24 branch between
builds (the published digest pins what runs, and the bridge reports
`ffmpeg -version`); musl's resolver resolves the sources (e2e covers a
Service name, the bench SomaFM's public names). Switching base is one stage.

*Licence:* the image is Apache-2.0 (patchbay) **and** GPL-3.0-or-later
(ffmpeg as Alpine builds it), with its libraries' own licences. patchbay
runs ffmpeg as a separate process and links nothing of it, so patchbay
stays Apache-2.0. The image's `org.opencontainers.image.licenses` is
`Apache-2.0 AND GPL-3.0-or-later`; docs/streams.md says where the sources
are. CI refuses an ffmpeg built with `--enable-nonfree` (not
redistributable).

### (g) The bridge's observations: a read-only control API, no token

**Decision (landed in `9a07b6b`):** `GET :9093/v1/observations`
(`streambridge/api.Observations`), polled by the controller every 5 s
through `<fullname>-streams.<ns>.svc:9093`. No token.

*Why:* it is read only and carries no secret: names, numbers, sources
(which may not hold credentials), counts and bounded error text. A token
would be a Secret to mint, mount and rotate for nothing to protect.
*Cost:* anyone in the cluster can see what is playing. Adding a token
later changes one client and one server.

### (h) One bridge replica, `strategy: Recreate`

*Why:* two bridges would run two ffmpegs for one channel, two tuners: the
policy breaks. *Cost:* stream calls drop when the bridge restarts or is
upgraded; streams (only) depend on one pod.

### (i) The bridge's objects are `<fullname>-streams`

*Why:* `<fullname>-stream-bridge` (design §9's `patchbay-stream-bridge`)
is a 64-character Service name for a 50-character fullname, over the DNS
label limit; `-streams` fits every fullname the chart makes. The
ConfigMap shares the name (another kind). A deviation to record.

### (j) A Stream with an unacceptable source is left out, never fails the compile

A Stream created before B's CEL rules (an `rtsp://` or a `.pls` source)
would make `streams.Document.Validate` fail and so every compile. Compile
leaves such a Stream out of the dialplan routing and the document (its
number plays "not available"), and the Stream is Invalid with Ready False
`InvalidSource` and CheckSource's message.

### (k) The tuner limit: sessions per pool, idle sessions give way

A session is one source playing (one ffmpeg), shared by every caller of
every Stream with that source. A call that needs a new session in a pool
already running `max`:
- if one of the pool's sessions is `Draining` (its last caller left less
  than 5 s ago), the bridge stops it, waits `PreemptSettle` (1 s, a bench
  switch, §8) and starts the new one: the caller is already answered and
  hears silence meanwhile;
- otherwise the call is refused: hangup, the caller hears "not
  available", and the refused Stream's `lastFailure` is `TunerBusy`:
  "pool hdhomerun (1 at a time) is playing kiro7 (707)".

The check and the start are one critical section: two phones dialling two
channels at once start exactly one.

### (l) Each caller is paced, with silence when there is no audio

Frame *n* of a caller is due at *t0* + *n* × 20 ms on the monotonic clock;
at each due time the bridge sends the oldest queued frame, or a silent
one if the queue is empty (a tuner locking, an underrun). A caller more
than 5 frames behind (a slow TCP peer) skips ahead rather than bursts.
Callers never wait on each other: the session pushes into each queue
without blocking and a full queue drops its oldest frame. *Why:* Asterisk
gets a steady stream (no gaps it would have to conceal), latency stays at
most 1 s even when Icecast bursts its first seconds, and one slow caller
cannot stall the rest.

### (m) The ffmpeg command line is a contract: `streambridge.FFmpegArgs`

```
ffmpeg -nostdin -hide_banner -loglevel error
  -protocol_whitelist http,https,tcp,tls,crypto
  -format_whitelist mpegts,mp3,aac,ogg,flac,wav,hls,mov
  -rw_timeout 10000000 -probesize 1000000 -analyzeduration 1000000
  [-tls_verify 1 [-ca_file <file>]]        # https sources only
  -i <source> -vn -sn -dn -ac 1 -ar 16000 -acodec pcm_s16le -f s16le pipe:1
```

*Checked 2026-10-16* with the dev shell's ffmpeg 9.0.1 on a generated
MPEG-TS (MPEG-2 video, AC-3 stereo, like an HDHomeRun's) and an MP3, each
served over HTTP at real time: the first PCM frame came after 2.8 s with
ffmpeg's default probing and 0.15 s with these limits (MP3: 0.70 s and
0.58 s); `file:` and an unlisted demuxer are refused with clear errors;
and `-tls_verify` on an `http://` input is fatal ("Option tls_verify not
found"), so only https gets it. *Why:* only network protocols, even
through a redirect or an HLS playlist; only the demuxers a tuner, Icecast
or HLS needs; a stall fails in 10 s; the probe limits are what makes
"about 3 s" reachable on a tuner. *Cost:* a source in a demuxer not
listed fails with "Format not on whitelist" (the bridge says "not an audio
stream the bridge reads"); the probe limits may be too small for a real
channel's audio track (bench B1). Both are one-line changes.

### (n) A Stream's state is a pure rule table

Like the drivers' `Decide`, the controller decides a Stream's status with a
pure function of the Stream (its status is the memory), the compiled
document, the bridge snapshot and `now` (§6.2, RT.5). Ready is True when
dialling it now should play.

### (o) No new Go modules

Everything B needs is required already: `k8s.io/utils/clock` (and
`clock/testing`), `prometheus/client_golang`, `golang.org/x/sync`,
`go-logr/logr`, controller-runtime, client-go, `pion/rtp` (fakephone).
UUID version 5 is 20 lines over `crypto/sha1` (`streams.NameUUID`, pinned
by RFC 9562's example and Python's `uuid.uuid5`), AudioSocket's codec is a
3-byte header. `go mod tidy -diff` is empty.

---

## 3. The AudioSocket sample-rate check

Design §9: "The plan checks whether Asterisk 22 carries higher sample rates
over AudioSocket before assuming either way."

**Method (2026-10-16).** The chart's pinned image,
`andrius/asterisk:22.11.0_debian-trixie@sha256:1817983370bf…`, run as the
PBX pod runs it (`--network none`, uid 1000, read-only root, no
capabilities), with a minimal config (`autoload = no`; res_audiosocket,
app_audiosocket, chan_audiosocket, pbx_config, app_dial, chan_pjsip and
what they need) and a throwaway AudioSocket server inside the container
that logs every frame's kind and size, measures 1 kHz and 6 kHz with
Goertzel, and, by UUID, plays a tone (8 or 16 kHz), hangs up, closes,
sends an error, or stays silent. Calls were originated through Local
channels, and over a PJSIP endpoint dialling Asterisk itself with
`allow = g722` only (then `ulaw` only) to get a real codec in the path.
Nothing of the probe is in the repo; its findings are pinned where they
matter (§4, RT.1's `TestRealAsteriskStreams`).

**Findings.**

| Question | Answer |
|---|---|
| What the image ships | `res_audiosocket.so` (knows slin, slin12 … slin192), `app_audiosocket.so` (references only 8 kHz slin; its documentation: "transmits channel audio as 16-bit, 8KHz mono PCM … other codecs available via the channel driver interface"), `chan_audiosocket.so` (a codec option) |
| `AudioSocket()` → server | kind `0x10`, 320 bytes per 20 ms, on a G.722 call too |
| `Dial(AudioSocket/…/c(slin16))` → server | kind `0x12`, 640 bytes per 20 ms |
| Server → caller, G.722-only SIP call, 1 kHz + 6 kHz sent as `0x12` | both tones arrive, 149 of 149 frames, through `Dial(…c(slin16))`; through `AudioSocket()` too (Asterisk takes any kind and switches the write format), though the application's documentation promises only 8 kHz |
| Server → caller, ulaw-only call | 1 kHz arrives (narrowband, as G.711 is) |
| slin16 → ulaw path | with `codec_resample`: slin16 → slin → ulaw; without it: slin16 → **G.722** → slin → ulaw |
| DTMF | kind `0x03`, a one-byte payload (`SendDTMF 1234#`: five frames) |
| Asterisk ends the call | closes the TCP connection; no hangup frame |
| Server sends hangup `0x00`, or closes | the application returns 0 and the dialplan continues; `Dial` returns `DIALSTATUS=ANSWER` and, with `g`, continues |
| Server sends error `0xff` | logged as an error; the application fails (the channel hangs up, unless wrapped in `TryExec`); `Dial` continues with `ANSWER` |
| Server unreachable | `TryExec(AudioSocket())`: `TRYSTATUS=FAILED`, continues; `Dial`: `DIALSTATUS=CHANUNAVAIL`, continues |
| A host name | resolved; each address tried in turn (`localhost`: `[::1]` refused, then `127.0.0.1`) |
| `TIMEOUT(absolute)=4` | both end the call at 4.0 s and never reach the next priority; after `Dial` the `T` extension ran, the application logged "Reached timeout after 2000 ms of no activity" and hung up |
| Silence from the server | with the far side sending audio, the application ran on for 6 s, and so did the channel driver; the application's "no activity" timeout (2000 ms in its log) was seen only at the time limit, so the bridge sends silence rather than nothing |

**Conclusion:** Asterisk 22.11 carries 16 kHz over AudioSocket through the
channel driver (decision (a)). The application could be made to play 16
kHz, but it sends 8 kHz, documents 8 kHz, and hangs up without continuing
on a refused connection unless wrapped in `TryExec`. The e2e fakephone
speaks G.711 only, so CI pins the wire format and refusal paths, not the
wideband sound: that is bench B5.

---

## 4. Contracts landed

Types, helpers and docs only, each pinned by a test; `make generate
check-generated fmt vet lint test`, `go mod tidy -diff`, `make
check-hygiene` and `make asterisk-check` are green at `838541e`. **The
contract code is authoritative;** this section summarises it.

| Commit | Package | Added or changed |
|---|---|---|
| `467ac09` | `internal/streams` (new) | `NamespaceName`, `Namespace`, `UUID(name)`, `NameUUID`, `FormatUUID`, `ParseUUID`; `Codec`, `SampleRate`, `FrameDuration`, `FrameSamples`, `FrameBytes`; `FileName`, `MaxSourceLength`, `MaxPoolMax`, `MaxStreams`; `Document{Version, Pools, Streams map[uuid]Stream}`, `Pool{Name, Max}`, `Stream{Name, Number, Source, Pool}`; `CheckSource`; `Document.Validate`, `.Sealed`, `.Encode`; `Decode` (strict) |
| `b60841b` | `api/v1alpha1` | `StreamStatus.{Listeners, TunerPool, LastPlayed, LastFailure}`, `StreamFailure{At, Reason, Message}`, `StreamFailureReason` (`SourceFailed`, `SourceEnded`, `TunerBusy`, an enum); reasons `ReasonIdle`, `ReasonPlaying`, `ReasonTunerBusy`, `ReasonSourceFailed`, `ReasonInvalidSource`, `ReasonBridgePending`, `ReasonBridgeUnreachable` (and `ReasonNoStreamBridge` re-documented); `spec.source` CEL: http(s), no credentials, not a playlist, no white space; columns Listeners, Ready, Reason, Pool (wide); the CRD regenerated; envtest `TestSchemaRejects` cases and `TestSchemaStreamSources` |
| `9a07b6b` | `internal/streambridge/audiosocket` (new, `frame.go`) | `Kind` and every kind byte, `KindAudio` (= `KindSlin16`), `HeaderSize`, `MaxPayload`, `UUIDSize`, `Frame{Kind, Payload}`, `Kind.IsAudio`, `.SampleRate`, `.String`, `AudioKind(rate)`; the package doc records §3's facts |
| `9a07b6b` | `internal/streambridge/api` (new) | ports, paths, `MaxMessageBytes`; `Observations{GeneratedAt, Bridge, Streams, Pools}`, `BridgeStatus`, `StreamObservations`, `SessionState`, `SessionObservations`, `FailureReason` (= the v1alpha1 strings, pinned), `Failure`, `PoolObservations`, `PoolSession` |
| `9a07b6b` | `internal/streambridge` (`doc.go`, `options.go`, `source.go`) | `Options`, `Flag*`, `Default*`, the timing and bounds (`Grace` … `StopTimeout`), `Source`, `Decoder`, `FFmpegArgs` |
| `1b802ef` | `internal/compile` | `Settings.Streams` (`StreamSettings{CallLimit, TunerPools}`, `TunerPool{Name, Hosts, Max}`), `DefaultCallLimit`/`MinCallLimit`/`MaxCallLimit`, `StreamSettings.Limit`; `Bundle.Streams`; modules: `chan_audiosocket`, `codec_resample`, `func_timeout` in, `app_audiosocket` out (the chart's list too); goldens' `modules.conf` and versions; `streams_contract_test.go` |
| `c011896` | `internal/controller/install` | `Names.StreamsConfigMap`, `Names.StreamBridge` (both `<fullname>-streams`), `StreamsKey`, `StreamBridgeAddress`, `StreamBridgeURL`, `AnnotationStreamsVersion`, `ErrStreamsNotCompiled`, `StreamsConfigMap`, `StreamsFromConfigMap`, `ReadStreams`; `EventReasonSourceFailed`, `EventReasonCallRefused`, `EventActionPlay` |
| `f0ddfbc` | chart, `internal/controller/settings` | values `streamBridge.enabled` (true), `streamBridge.ports.{audioSocket, control}`, `streams.callLimit` (`2h`), `streams.tunerPools` (`[]`); `patchbay.settingsJSON` renders `streamBridge` (while enabled) and `streams` (pools only when set; a pool missing a field fails `helm template`); the shared fixture `chart-defaults.json` and `TestParseChartDefaults` |
| `838541e` | `test/e2e/fakephone/script` (`source.go`) | the fake stream source: paths, parameters, ports, the HDHomeRun 503 header, `SourceStatus{Tuners, Occupied, Open, Opened, Refused}`, `OpenTotal` |

```go
// internal/streams
const NamespaceName = "https://github.com/ChristopherJMiller/patchbay/streams"
const Namespace = "211a92d2-913d-5c00-af69-82af81c3daf0"
func UUID(name string) string                                  // canonical, lower-case
func NameUUID(ns [16]byte, name string) [16]byte
func FormatUUID(u [16]byte) string
func ParseUUID(s string) ([16]byte, error)                     // canonical form only
const Codec = "slin16"; SampleRate = 16000; FrameDuration = 20 * time.Millisecond; FrameSamples = 320; FrameBytes = 640
const FileName = "streams.json"; MaxSourceLength = 2048; MaxPoolMax = 16; MaxStreams = 1000
type Document struct{ Version string; Pools []Pool; Streams map[string]Stream }  // json: version, pools, streams
type Pool struct{ Name string; Max int }                                          // json: name, max
type Stream struct{ Name, Number, Source, Pool string }                           // json: name, number, source, pool,omitempty
func CheckSource(source string) error
func (d Document) Validate() error
func (d Document) Sealed() (Document, error)  // nil -> empty, pools sorted, Version set, Validate
func (d Document) Encode() ([]byte, error)    // Validate, then indented JSON + "\n"
func Decode(data []byte) (Document, error)    // strict, then Validate

// internal/streambridge
type Options struct {
	Listen, ControlListen, StreamsFile, FFmpeg, CAFile string
	Source     Source
	Clock      clock.WithTicker
	Registerer prometheus.Registerer
	Gatherer   prometheus.Gatherer
	Log        logr.Logger
}
const FlagListen = "listen"; FlagControlListen = "control-listen"; FlagStreamsFile = "streams-file"; FlagFFmpeg = "ffmpeg"
var DefaultListen = ":9092"; DefaultControlListen = ":9093"
const DefaultStreamsDir = "/etc/patchbay/streams"; DefaultStreamsFile = DefaultStreamsDir + "/streams.json"; DefaultFFmpeg = "/usr/bin/ffmpeg"
const Grace = 5 * time.Second; PreemptSettle = time.Second; StartTimeout = 15 * time.Second; StallTimeout = 10 * time.Second
const IDTimeout = 5 * time.Second; QueueFrames = 50; MaxCalls = 64; MaxSessions = 8; StreamsPollInterval = 2 * time.Second; StopTimeout = 2 * time.Second
type Source interface{ Start(ctx context.Context, source string) (Decoder, error) }
type Decoder interface{ io.Reader; Wait() error }
func FFmpegArgs(source, caFile string) []string

// internal/controller/install (additions)
Names.StreamsConfigMap, Names.StreamBridge                     // "<f>-streams"
const StreamsKey = "streams.json"; AnnotationStreamsVersion = "patchbay.chrismiller.xyz/streams-version"
func StreamBridgeAddress(n Names, namespace string, port int) string // "<f>-streams.<ns>.svc:<port>"
func StreamBridgeURL(n Names, namespace string, port int) string     // "http://" + StreamBridgeAddress
var ErrStreamsNotCompiled error
func StreamsConfigMap(n Names, namespace string, d streams.Document) (*corev1.ConfigMap, error)
func StreamsFromConfigMap(cm *corev1.ConfigMap) (streams.Document, error)
func ReadStreams(ctx context.Context, c client.Reader, namespace string, n Names) (streams.Document, error)
const EventReasonSourceFailed = "SourceFailed"; EventReasonCallRefused = "CallRefused"; EventActionPlay = "Play"
```

---

## 5. Package map, ownership and rules

```text
internal/streams/                                         contract (467ac09)          nobody
internal/streambridge/{doc,options,source,contract_test}.go contract (9a07b6b)        nobody
internal/streambridge/audiosocket/frame{,_contract_test}.go contract (9a07b6b)        nobody
internal/streambridge/api/                                contract (9a07b6b)          nobody
internal/streambridge/ (every other file and subpackage)  BR.1-BR.8                   bridge
api/v1alpha1/, charts/patchbay/templates/crds/            contract (b60841b)          nobody
internal/compile/streams_contract_test.go                 contract (1b802ef)          nobody
internal/compile/ (every other file, testdata/)           RT.1                        routing
internal/controller/install/                              contract (A, c011896)       nobody
internal/controller/settings/                             RT.3 (fixture: f0ddfbc)     routing
internal/controller/bundle/                               RT.2                        routing
internal/controller/bridgeclient/ (new)                   RT.4                        routing
internal/controller/streamstate/ (new)                    RT.5                        routing
internal/controller/numbers/                              RT.5 (loses Streams)        routing
internal/controller/metrics/                              RT.6                        routing
test/e2e/fakephone/script/                                contract (A, 838541e)       nobody
test/e2e/fakephone/ (every other file, Dockerfile)        PK.5                        packaging
test/e2e/streamkit_test.go (new), test/e2e/values.yaml    PK.6                        packaging
Dockerfile, .dockerignore, Makefile, .github/, renovate.json, hack/,
charts/patchbay/ (not templates/crds/), test/chart/, test/image/ (new),
docs/releasing.md                                         PK.1-PK.4                   packaging
internal/controller/*.go (root, crd_smoke_test.go too), cmd/patchbay/,
internal/cli/, test/e2e/*.go (not streamkit_test.go), config/samples/,
docs/ (not releasing.md), README.md, CLAUDE.md            I.1-I.9                     integrator
```

**Rules for the parallel stages** (bridge, routing, packaging):

1. Write only your own files. Never edit a contract file (the
   `nobody` rows above, and every release A contract CLAUDE.md lists) or
   anything you do not own. If a contract is wrong, work around it inside
   your files and report it as a deviation; the integrator reconciles.
2. Import your own packages, the contract packages and the pre-existing
   ones (`api/v1alpha1`, `internal/compile`, `internal/drivers`,
   `internal/streams`, `internal/streambridge/{api,audiosocket}` and the
   streambridge contract files' identifiers, `internal/controller/install`,
   `internal/controller/agentview`, `internal/controller/ctrltest`,
   `internal/version`, `internal/agent/netlimit`); never another stage's
   new code. The bridge stage imports nothing under `internal/controller`
   or `internal/compile`. Nothing imports `cmd` or the controller root.
3. **No new modules:** no `go get`, no `go mod tidy`, no edit of `go.mod`
   or `go.sum`. Everything needed is required already (§2(o)).
4. Verify your own packages only; **never `make fmt`** (it reformats
   everyone's files). With `P` your packages:
   `nix develop -c golangci-lint fmt $P`, `nix develop -c go vet $P`,
   `nix develop -c go vet -tags e2e $P`, `nix develop -c golangci-lint run $P`,
   `nix develop -c go test -race -count=1 $P` and
   `nix develop -c go test -race -count=1 -short -tags e2e $P`. At the end
   run `nix develop -c make vet lint test-unit` once and report (never
   fix) a failure in another stage's files.
5. TDD, small commits, `git add` your own paths only (never `-A` or `.`),
   bodies ending with `Co-Authored-By: Claude Opus 5.5
   <noreply@anthropic.com>`; never bypass hooks; on `index.lock`, wait and
   retry. **No cluster:** only the integrator creates the kind cluster
   (`make e2e`). envtest only in `bundle`, `numbers` and one file of
   `streamstate` (through `ctrltest.Main`); everything else uses the
   controller-runtime fake client, loopback listeners on port 0,
   `t.TempDir()` and fakes. Docker is allowed to the routing stage (`make
   asterisk-check`) and the packaging stage (image builds, `make
   image-check-stream-bridge`); rootless, so container IPs are not
   reachable from the host.
6. **No network beyond loopback in any test**: no test reaches the
   HDHomeRun, SomaFM, Docker Hub or anything else; sources in tests are
   local fakes (`httptest`) or generated files.
7. Never log, put in status, an Event or an error a caller's audio or a
   secret (B has none of its own). Text from ffmpeg or a source is
   bounded (`MaxMessageBytes`) and printable before it goes anywhere.
8. No test loads a kubeconfig.
9. Coverage ≥ 80% per package (`go test -cover`).
10. Fixtures are generated (hard rule 4): tones, WAVs and MPEG-TS files
    are made by the test (or by the dev shell's ffmpeg into
    `t.TempDir()`), never committed.

---

## 6. Stage tasks

### 6.1 Stage bridge (BR): `patchbay stream-bridge`

Everything is in `internal/streambridge` (and new subpackages of it, if you
want them), beside the contract files. The integrator wires
`cmd/patchbay stream-bridge` to `Run`.

Exported API (exact; the integrator calls these):

```go
// Run is the bridge: it serves AudioSocket on o.Listen and the control
// API on o.ControlListen until ctx ends, then stops accepting, sends every
// caller a hangup frame, stops every decoder and returns nil. It fails at
// once on a bad Options or a listener that cannot bind. Zero fields take
// the Default* values; a nil Source is NewFFmpegSource(o.FFmpeg, o.CAFile, o.Log).
func Run(ctx context.Context, o Options) error

// FFmpegSource is the production Source: ffmpeg with FFmpegArgs.
type FFmpegSource struct{ /* path, caFile, log */ }
func NewFFmpegSource(path, caFile string, log logr.Logger) *FFmpegSource
func (s *FFmpegSource) Start(ctx context.Context, source string) (Decoder, error)
// Version runs `<path> -version` and returns its first line (readiness,
// BridgeStatus.FFmpeg).
func (s *FFmpegSource) Version(ctx context.Context) (string, error)
```

and in `audiosocket` (beside `frame.go`):

```go
func ReadFrame(r io.Reader) (Frame, error)            // header then payload; io.EOF only at a frame boundary, io.ErrUnexpectedEOF inside one
func WriteFrame(w io.Writer, f Frame) error           // one Write of header+payload; payload over MaxPayload is an error
func AppendFrame(dst []byte, k Kind, payload []byte) []byte
func UUIDOf(f Frame) ([16]byte, error)                // KindUUID with UUIDSize bytes
```

**BR.1 AudioSocket codec.** As above. A zero-length audio frame is valid
and ignored by the server. *Tests:* `TestFrameRoundTrip` (every kind,
payloads of 0, 1, 640 and 65535 bytes), `TestReadFrameErrors` (EOF before a
header is `io.EOF`; a short header or payload `io.ErrUnexpectedEOF`),
`TestWriteFrameIsOneWrite` (a writer counting calls),
`TestWriteFrameTooLarge`, `TestUUIDOf` (bytes equal
`streams.ParseUUID(streams.UUID("king5"))`; wrong kind or size refused),
`FuzzReadFrame` (seed corpus only under `go test`).

**BR.2 The ffmpeg source.** `Start` runs `exec.CommandContext(ctx, path,
FFmpegArgs(source, caFile)...)` with stdout as the `Decoder`'s reader;
its `Cancel` sends SIGTERM when ctx ends and `WaitDelay = StopTimeout`
kills it after; stderr goes to a ring of its last 4 KiB. `Wait` returns
nil after ctx ended, otherwise an error: `ffmpeg exited with status <n>`
(or `was killed by <signal>`), and `: <reason>`, the last non-empty stderr
line with ffmpeg's `[demuxer @ 0x…] ` prefixes removed, control characters
as spaces, the whole at most `api.MaxMessageBytes`. Two translations, so
the status reads well: "Format not on whitelist" and "Invalid data found
when processing input" become `the source is not an audio stream the
bridge reads (<ffmpeg's words>)`; an HTTP 503 keeps ffmpeg's line ("Server
returned 5XX Server Error reply"). *Tests* use the dev shell's real ffmpeg
(`exec.LookPath`; skipped under `-short`; under plain `go test` a missing
ffmpeg fails the test, so `make test` and CI run them) against `httptest`
servers on loopback, the media generated into `t.TempDir()`:
`TestFFmpegDecodesWAV` (a 48 kHz stereo 1 kHz WAV streamed at real time:
the output is 16 kHz mono, 32 000 ± 10% bytes per second, Goertzel ratio
at 1 kHz ≥ 0.8), `TestFFmpegDecodesMPEGTS` (the dev shell's ffmpeg makes 5
s of MPEG-2 + AC-3 1 kHz TS, as §2(m); first PCM frame in under 1 s, the
tone measured), `TestFFmpeg503` (the server answers 503 with
`X-HDHomeRun-Error: 805 All Tuners In Use`: `Wait` within 3 s, the
reason mentions 503), `TestFFmpegHTMLPage` (text/html: the "not an audio
stream" reason), `TestFFmpegHTTPS` (`httptest.NewTLSServer`: with
`caFile` its certificate, it plays; without, it fails and the reason
mentions the certificate), `TestFFmpegStop` (ctx cancelled while playing:
`Wait` nil within `StopTimeout`, the process gone), `TestFFmpegVersion`.

**BR.3 The session manager.** One per bridge, under one mutex for the
bookkeeping (never held while writing to a socket or reading a decoder).
A session is keyed by source URL. The rules:
1. *Join.* A caller of stream *s* joins the session of `s.Source` if one
   runs (Starting, Playing or Draining; a Draining one goes back to
   Playing or Starting and its grace timer stops). The caller's stream gets
   `LastPlayed` when the caller receives its first audio frame (at once if
   the session is Playing).
2. *Start.* Otherwise, if *s* has a pool: count that pool's sessions (any
   state). Under `Max`, start. At `Max`, if one is Draining, stop it (no
   failure recorded), wait `PreemptSettle`, start. Otherwise refuse:
   `TunerBusy`, message `pool <name> (<max> at a time) is playing
   <stream> (<number>)[, …]` (the streams of the pool's sessions, sorted),
   recorded as the refused stream's `LastFailure`. Count and start are one
   critical section (§2(k)).
3. *Bounds.* A start that would make more than `MaxSessions` sessions is
   refused (`FullCalls`), as is a connection beyond `MaxCalls`.
4. *Audio.* The session's reader `io.ReadFull`s `streams.FrameBytes` at a
   time and pushes each frame to every caller's queue without blocking (a
   full queue drops its oldest frame; `frames_dropped_total`). The first
   frame makes it Playing (`FirstAudioAt`).
5. *Failure.* `Source.Start` fails, or the decoder ends (Read error, `Wait`
   non-nil) while Starting, or no audio comes within `StartTimeout`:
   `SourceFailed`. The decoder ends, or no frame comes for
   `StallTimeout`, while Playing: `SourceEnded`. Either way every caller
   gets a hangup frame and is closed, the decoder is stopped, the session
   removed, and the failure (with the reason, BR.2) is each of the
   session's streams' `LastFailure` (each stream with that source that had
   a caller in it).
6. *Leave.* A caller leaves on EOF, a hangup or error frame from Asterisk,
   or a write error. The last caller leaving makes the session Draining;
   `Grace` later its decoder is stopped and it is removed (the tuner is
   free).
7. *Reload.* A new streams.json applies to new calls only: a session keeps
   its callers when its stream disappears or changes source, and keeps
   counting against its pool until they leave. A stream that is gone has no
   observations and no metrics.
8. *Memory.* `LastPlayed`, `LastFailure` and `Calls` are kept per stream
   UUID in memory only (the controller copies them into status).

*Tests* (a fake `Source` in a `_test.go` file: decoders that emit a known
tone fast or paced, never emit, fail at start, fail after *n* frames,
stall, or block until released; `k8s.io/utils/clock/testing.FakeClock` for
every timer): `TestOneSessionPerSource` (two streams with one source:
one `Start`), `TestFanOut` (three callers receive the same frames in
order), `TestSlowCallerDropsOldest` (a caller that never reads: the others
are unaffected, it is cut to its queue, the drop counter moves),
`TestGrace` (the decoder's ctx ends exactly `Grace` after the last leave; a
join at `Grace` − 1 ms keeps it), `TestStartFails`, `TestExitBeforeAudio`,
`TestStartTimeout`, `TestStall`, `TestEndsWhilePlaying` (callers get a
hangup; `LastFailure` reason and message), `TestPoolLimit` (max 1: a
second source refused with `TunerBusy` naming the holder; the holder plays
on), `TestPoolPreemptsDraining` (stopped, `PreemptSettle` waited, then
started), `TestPoolRace` (100 goroutines join two sources of a max-1
pool at once, 50 each: exactly one source starts; every caller of the
other is refused), `TestRadioHasNoLimit` (eight sources without a pool
play at once; a ninth is refused at `MaxSessions`), `TestReloadKeepsCalls`,
`TestLastPlayed`.

**BR.4 The AudioSocket server.** Accepts on `Options.Listen`, at most
`MaxCalls` connections (beyond: a hangup frame, then close). Per
connection: the first frame within `IDTimeout`, a `KindUUID`
(anything else: close); an unknown UUID: hangup, close (`UnknownCalls`);
then join (or refuse: hangup, close). Then two goroutines. The *reader*
reads and discards Asterisk's frames: audio is dropped, `KindDTMF`
ignored, an unknown kind ignored, `KindHangup`, `KindError`, EOF or an
error ends the call. The *pacer* (§2(l)) writes one `KindAudio` frame
per 20 ms from the caller's queue, or silence (640 zero bytes); when the
session fails it writes a hangup frame and closes. *Tests* (loopback, a
fake `Source`): `TestServeUnknownUUID` (hangup, then close),
`TestServeIDTimeout`, `TestServeNotUUIDFirst`, `TestServePacing` (the
real clock: 250 frames arrive in 5 s ± 60 ms, never more than one frame
early), `TestServeSilenceWhileStarting` (a decoder that waits 1 s: silent
frames at 20 ms, then the tone), `TestServeBurst` (a decoder that emits 5 s
at once: the caller gets at most 1 s of it queued, at 20 ms each, and the
newest), `TestServeDiscardsUpstream` (audio and DTMF from the client
change nothing), `TestServeCallerHangsUp` (EOF: the listener is removed),
`TestServeMaxCalls`, `TestServeRefusalIsHangup` (a refused call gets
exactly one `KindHangup` frame before the close),
`TestServeShutdown`.

**BR.5 streams.json.** Read at start and every `StreamsPollInterval`;
when the bytes change, `streams.Decode`; a valid new version replaces the
old (`StreamsVersion`, `StreamsLoadedAt`), an invalid one is
`StreamsError` and the old stays. Missing at start: not ready, retried.
*Tests:* `TestStreamsLoad`, `TestStreamsReload` (a rewritten file is used
within one poll; the kubelet's symlink swap, `..data`, is followed),
`TestStreamsInvalidKeepsOld`, `TestStreamsMissing`.

**BR.6 The control API.** On `Options.ControlListen`:
`api.PathObservations` (the document: every loaded stream and pool,
sorted by name, lists never null; `GeneratedAt` and `Bridge.StartedAt`,
`Version` from `internal/version`), `PathHealthz` (200), `PathReadyz` (200
once a streams.json is loaded and `FFmpegSource.Version` (or, with a test
Source, nothing) ran; else 503 with the reason), `PathMetrics`. The
listener is capped with `netlimit` (32 connections, 8 per source,
loopback exempt for the probes), `ReadHeaderTimeout` 5 s, idle connections
closed after 15 s. *Tests:* `TestObservationsDocument` (JSON shape against
a session in each state, a pool with two sessions, a failure),
`TestReadyz`, `TestControlCaps`.

**BR.7 Metrics** (registered on `Options.Registerer`, served from
`Options.Gatherer`; test through `promhttp.HandlerFor` text, as
`internal/agent/metrics` does):
`patchbay_stream_bridge_calls{stream}` (open calls per loaded stream),
`patchbay_stream_bridge_sessions{pool}` (running decoders; `none` for no
pool), `patchbay_stream_bridge_session_starts_total{result="played|failed"}`,
`patchbay_stream_bridge_refused_calls_total{reason="unknown_stream|tuner_busy|bridge_full"}`,
`patchbay_stream_bridge_frames_dropped_total`,
`patchbay_stream_bridge_underrun_frames_total` (silence sent while
Playing), `patchbay_stream_bridge_streams_version_info{version}` (1, the
previous series removed), `patchbay_stream_bridge_streams_load_failures_total`.
*Test:* `TestEveryMetric`.

**BR.8 Run.** Validates `Options` (addresses parse; a set `Source` with
an empty `FFmpeg` is fine), binds both listeners (an error names the
address), starts the file watcher, serves, and shuts down as documented.
Logs one line per call (stream, number, how long, how it ended) and per
session start, stop and failure; never audio. *Test:*
`TestRunEndToEnd` (a fake Source, a streams.json in `t.TempDir()`, both
listeners on `127.0.0.1:0` through an unexported hook: a client sends a
UUID frame, receives the tone paced, the observations show one listener,
the client closes, the decoder stops after `Grace`, ctx cancelled: Run
returns nil).

**Bridge acceptance:** `P=./internal/streambridge/...`, rule 4's commands,
coverage ≥ 80%, and `nix develop -c go test -race -count=3
./internal/streambridge/...` green (no flake).

### 6.2 Stage routing (RT)

**RT.1 compile.**
1. `Settings.Validate` gains (each message names the field):
   - `streamBridge`: the host is an IP address or a DNS subdomain
     (`validation.IsDNS1123Subdomain`) and holds none of `/,()`, so it is
     safe inside `Dial(…)`; the message stays `streamBridge %q is not
     host:port`;
   - `streams.callLimit`: zero, or whole seconds from `MinCallLimit` to
     `MaxCallLimit`: `streams.callLimit %s is not whole seconds from 10s to
     24h`;
   - `streams.tunerPools[i]`: name a DNS label (`streams.tunerPools[0]:
     name %q is not a DNS label`), listed once; `hosts` not empty, each an
     IP address (IPv6 without brackets, as `url.URL.Hostname` gives it) or
     a DNS subdomain, compared lower-case, none in two pools (`host %q is
     in pools %q and %q`); `max` from 1 to `streams.MaxPoolMax`.
2. Each Stream that holds its number (claims, as today):
   - `streams.CheckSource` fails: not routed; its number plays "not
     available" with the comment `; Stream <name>: its source is refused
     (<error>)`; not in the document;
   - `Settings.StreamBridge == ""`: `; Stream <name>: streams are disabled
     (no stream bridge)` then "not available"; it **is** in the document;
   - otherwise exactly:
     ```
     ; Stream king5
     exten => 705,1,Answer()
      same => n,Set(TIMEOUT(absolute)=7200)
      same => n,Dial(AudioSocket/patchbay-streams.telephony.svc:9092/89fb6719-3e3d-5ef3-881a-b9f423a8a432/c(slin16),,g)
      same => n,Goto(patchbay-unavailable,s,1)
     ```
     (`streams.UUID(name)`, `streams.Codec`, the limit in whole seconds).
     Never the display name in the dialplan (release A's rule).
3. `Bundle.Streams`: `streams.Document{Pools: every tuner pool (name, max),
   Streams: every Stream of item 2 but the refused ones, keyed by UUID,
   with Pool = the pool whose hosts hold the source's lower-cased
   url.Hostname(), or ""}.Sealed()`. A Sealed error fails Compile (it
   cannot happen after item 1 and 2). `Bundle.Validate` does **not**
   require `Streams`: bundles decoded from pbx-config (the agent) have
   none.
4. Goldens (`go test ./internal/compile -run Golden -update`; the golden
   harness also writes `want/streams.json` from `Bundle.Streams.Encode`):
   new `streams` (the house: 704/705/707/713 on `192.168.0.58`, pool
   `hdhomerun` max 1, two radio Streams, `streamBridge` set, `callLimit`
   2h), new `streams-refused` (an `rtsp://` and a `.pls` Stream written
   before the rule), new `streams-limit` (a 90 m limit, two pools), and
   `streams-disabled` updated (its comment, and its streams.json).
5. `make asterisk-check` gains `TestRealAsteriskStreams`: the `streams`
   golden booted with `streamBridge` replaced by `127.0.0.1:9092` (as
   `checkSettings` replaces the PBX address); the test binary also runs in
   the container as an AudioSocket server (an env mode beside the SIP
   client's) that records each connection's first frame to a file in a
   tmpfs and, by UUID, sends 2 s of `KindAudio` tone then hangs up, or
   hangs up at once; the SIP client gains `SIP_HOLD` (stay up that long
   before BYE). Assertions: a call to 705 is answered and the server saw a
   `KindUUID` frame with `streams.UUID("king5")`'s bytes and wrote 2 s of
   audio without a write error; a call whose server hangs up at once is
   still answered (it then plays "not available"); a call to 750 with no
   server listening is answered; `dialplan show 705@patchbay-devices`
   shows the item 2 lines.
6. *Tests:* `TestStreamDialplan` (the three forms), `TestStreamDocument`
   (pools, UUID keys, pool matching), `TestPoolMatching` (case, any port,
   an IPv6 host, a host in no pool), `TestStreamRefusedSource`,
   `TestSettingsValidateStreams` (every rule of item 1), the goldens.

**RT.2 bundle.** The reconciler writes `install.StreamsConfigMap(…,
b.Streams)` first, when its `AnnotationStreamsVersion` differs, then
pbx-secrets and pbx-config as today; `ConfigWritten`'s note gains `, <s>
streams` when the bundle was written; a streams-only change writes only
`<fullname>-streams` and emits nothing. It watches `<fullname>-streams`
(an edit or delete is rewritten). *Tests (envtest):*
`TestWritesStreamsFirst` (the interceptor records `<fullname>-streams`
before pbx-secrets), `TestStreamsUnchangedNoWrite`,
`TestSourceChangeRewritesOnlyStreams` (pbx-config's version unchanged),
`TestEditedStreamsConfigMapRewritten`.

**RT.3 settings.** `TestParseStreams` (pools and limit parse; a bad
value's error names the field). The fixture is landed; keep it.

**RT.4 bridgeclient** (new):

```go
type Config struct {
	URL  string       // install.StreamBridgeURL(names, namespace, bridgeapi.DefaultControlPort)
	HTTP *http.Client // nil: no proxy, no redirects, 5 s per request
	Log  logr.Logger
}
func New(cfg Config) (*Client, error)
func (c *Client) Observations(ctx context.Context) (bridgeapi.Observations, error)
```
GET, body at most 4 MiB, non-200 an error quoting the status. *Tests
(httptest):* decode, 500, a slow server times out, a redirect is not
followed, a huge body is cut with an error.

**RT.5 streamstate** (new; the Stream reconciler moves here from
`numbers`, which keeps only Person):

```go
type Source interface{ Observations(ctx context.Context) (bridgeapi.Observations, error) }
type Snapshot struct{ Document bridgeapi.Observations; PolledAt time.Time; Reachable bool; Polls uint64; LastError string }
type Config struct {
	Client    client.Client
	Namespace string
	Names     install.Names
	Settings  compile.Settings
	Bridge    Source              // nil when Settings.StreamBridge is "" (no poller)
	Interval  time.Duration       // 0: install.PollInterval
	Recorder  events.EventRecorder
	Metrics   *metrics.Metrics    // nil: none
	Now       func() time.Time    // nil: time.Now
	Log       logr.Logger
}
func Setup(mgr ctrl.Manager, cfg Config) error  // the poller (leader-only Runnable) and the reconciler, Named("stream")
func Decide(in Input) Verdict                    // pure
```

`Input` and `Verdict` are the stage's to shape (nobody else calls
`Decide`): it takes the Stream (its status is the memory), its claim
problem if any, the settings, its compiled document entry and version, the
snapshot, `install.StaleAfter` and `now`; it returns the state, Ready and
NumberConflict, the folded status fields and the Events to emit. No I/O,
no clock.

The poller polls every `Interval`, keeps the last good document on a
failure (Reachable false, `LastError` the error), and enqueues (through a
`source.Channel` of `event.TypedGenericEvent[*v1alpha1.Stream]`) every
Stream on its first poll or a reachability change, otherwise each Stream
whose observation changed (by name), and every Stream when the pools'
observations changed. The reconciler reads the Stream, the claims
(`compile.ResolveClaims` over every resource, as `numbers` does), the
compiled document (`install.ReadStreams`) and the snapshot, calls
`Decide`, writes status only when it changed (conditions with
`ObservedGeneration`), and emits `StateChanged` on a state change and one
`SourceFailed` (Warning) or `CallRefused` (Normal) per new
`lastFailure` (the note: `<reason>: <message>`). It writes nothing before
`<fullname>-streams` exists (`ErrStreamsNotCompiled`), and is enqueued
when it appears. Watches: Stream (`GenerationChangedPredicate`),
`<fullname>-streams` and pbx-config (all Streams), the poller's channel.

`Decide`'s rows, first match wins (state, Ready, message):

| # | State | When | Ready |
|---|---|---|---|
| 1 | `Invalid` | an older resource holds the number, or it is out of the plan | False `NumberConflict` / `NumberNotInPlan` (release A's text) |
| 2 | `Invalid` | `streams.CheckSource` refuses the source | False `InvalidSource`: the error |
| 3 | `Unavailable` | `Settings.StreamBridge` is empty | False `NoStreamBridge`: `streams are disabled (streamBridge.enabled is false): dialling 705 plays "not available"` |
| 4 | `Unknown` | the snapshot is not Reachable, or older than `install.StaleAfter` | Unknown `BridgeUnreachable`: `cannot reach the stream bridge: <LastError>`; listeners, lastPlayed, lastFailure kept |
| 5 | `Pending` | the bridge's document lacks this UUID, or has it with another source or pool | False `BridgePending`: `the stream bridge has not loaded this stream yet (it plays streams <v[:12]>, compiled <v[:12]>)` |
| 6 | `Playing` | `listeners > 0` | True `Playing`: `2 listeners` (`; tuner pool hdhomerun` when it has one) |
| 7 | `Failing` | `lastFailure` is `SourceFailed` or `SourceEnded` and newer than `lastPlayed` (or there is none) | False `SourceFailed`: `<at, RFC 3339>: <message>` |
| 8 | `Busy` | its pool has `max` sessions with listeners, none on its source | False `TunerBusy`: `pool hdhomerun (1 at a time) is playing kiro7 (707)` |
| 9 | `Idle` | otherwise | True `Idle`: `nobody is listening` (`; last played <at>` when known) |

Before rows 5–9 the observation is folded into the status (rows 1–4 keep
it): `listeners` is the observation's; `tunerPool` the compiled document's
pool; `lastPlayed` the later of status's and the observation's;
`lastFailure` the observation's when its `At` is later than status's (an
older or equal one never replaces it; that copy is what emits the Event,
once). NumberConflict is written as `numbers` writes it. A `TunerBusy`
failure never makes a Stream Failing: it is history in `lastFailure` and
an Event, and row 8 says whether the pool is busy now.

*Tests:* `TestDecide` (every row, and the boundaries: a failure equal to
lastPlayed, a stale snapshot at exactly `StaleAfter`, a pool at max with
this stream's own source among the holders, a Draining session not
counted as busy), `TestFold` (failures copied once; an older observation
after a bridge restart changes nothing), `TestReconcileWritesOnlyWhenChanged`,
`TestEventsOncePerFailure`, `TestGatedUntilCompiled`,
`TestPollerEnqueues` (first poll all; a changed stream one; a reachability
change all; a failure keeps the document), and one envtest file
(`TestStreamStatusEnvtest`: a Stream, a compiled `<fullname>-streams`, a
fake bridge document in each shape → Playing, Busy, Failing, Idle,
Unknown and Pending written, conditions carry `ObservedGeneration`, the
enum refuses nothing the controller writes). `numbers`' Stream tests move
here, adapted.

**RT.6 metrics.** `patchbay_stream_bridge_reachable` (1 or 0; absent before
the first poll) and `patchbay_streams{state}` (from the cache at scrape,
`""` as `Pending`), with `Metrics.ObserveBridge(streamstate.Snapshot)`
or an equivalent the poller calls; nil-safe as today.

**Routing acceptance:** `P="./internal/compile/... ./internal/controller/bundle/... ./internal/controller/settings/... ./internal/controller/bridgeclient/... ./internal/controller/streamstate/... ./internal/controller/numbers/... ./internal/controller/metrics/..."`,
rule 4's commands, coverage ≥ 80%, and `nix develop -c make
asterisk-check` green with the new goldens and `TestRealAsteriskStreams`.

### 6.3 Stage packaging and harness (PK)

**PK.1 The image** (§2(f)). The Dockerfile gains, between the build stage
and the patchbay stage (the last stage stays patchbay, so `docker build .`
and CI's current builds are unchanged):

```dockerfile
FROM alpine:3.24.<n>@sha256:<index digest> AS stream-bridge
RUN apk add --no-cache ffmpeg ca-certificates \
 && ffmpeg -hide_banner -buildconf | grep -q -- '--enable-gpl' \
 && ! ffmpeg -hide_banner -buildconf | grep -q -- '--enable-nonfree'
COPY --from=build /out/patchbay /patchbay
LABEL org.opencontainers.image.source=… org.opencontainers.image.description="patchbay stream bridge: live audio sources to callers over AudioSocket (patchbay + ffmpeg)" \
      org.opencontainers.image.licenses="Apache-2.0 AND GPL-3.0-or-later"
USER 65532:65532
ENTRYPOINT ["/patchbay"]
```

(pin the multi-arch index digest; `make check-versions` keeps working with
one builder). `make image-stream-bridge` (`IMG_STREAM_BRIDGE ?=
ghcr.io/christopherjmiller/patchbay-stream-bridge:dev`, `--target
stream-bridge`); `make e2e-images` also builds `…:e2e` (no e2e tag needed).
`make image-check-stream-bridge` runs `test/image` (`//go:build image`,
docker, no network): in the built image, PID 1's uid is 65532; `ffmpeg
-version` runs; `-buildconf` has `--enable-gpl` and no `--enable-nonfree`;
`/etc/ssl/certs/ca-certificates.crt` exists; and the image's ffmpeg takes
`streambridge.FFmpegArgs` for an `http://127.0.0.1:1/x` and an
`https://127.0.0.1:1/x` source, failing with "Connection refused", never
"Option … not found" or "Unrecognized option" (so the contract's flags
work in Alpine's 8.1 as in the dev shell's 9.0).

**PK.2 CI.** `image`: both targets, amd64 and arm64
(`docker/setup-qemu-action`, pinned by SHA, for the bridge's `apk add`),
then `make image-check-stream-bridge` on the amd64 build. `publish`: both
images, the same tags, both attested; outputs `digest` and
`bridge-digest`. `release`: writes `streamBridge.image.digest` too, and
fails unless the packaged chart runs
`ghcr.io/christopherjmiller/patchbay-stream-bridge:<v>@<digest>`.
`actionlint` clean. docs/releasing.md says so.

**PK.3 Renovate.** `ghcr.io/christopherjmiller/patchbay-stream-bridge` is
ours (disabled with patchbay's own image). The Dockerfile manager already
proposes the Alpine digest (3-day wait); it may merge itself on `ci-ok`
(it holds no credentials; unlike Asterisk). `renovate-config-validator
--strict` from the flake.

**PK.4 The chart.** Values under `streamBridge` (beside the landed
`enabled` and `ports`): `image` (`repository:
ghcr.io/christopherjmiller/patchbay-stream-bridge`, `tag: ""` → appVersion,
`digest: ""`, `pullPolicy`), `logLevel: info`, `resources` (requests 50m /
96Mi, limit memory 512Mi; measure in e2e and record), `podAnnotations`,
`nodeSelector`, `tolerations`, `affinity`, `terminationGracePeriodSeconds:
10`. A template (rendered only while enabled) with:
- ServiceAccount `<fullname>-streams` (`automountServiceAccountToken:
  false`), no Role;
- Deployment `<fullname>-streams`: `replicas: 1` (not a value),
  `strategy: Recreate`, component label `stream-bridge`,
  `automountServiceAccountToken: false`, `enableServiceLinks: false`, pod
  `runAsNonRoot`, 65532:65532, seccomp RuntimeDefault; container
  `stream-bridge`: args `stream-bridge`, `--listen=:<audioSocket>`,
  `--control-listen=:<control>`,
  `--streams-file=/etc/patchbay/streams/streams.json`,
  `--ffmpeg=/usr/bin/ffmpeg`, `--zap-log-level=<logLevel>` (the
  `streambridge.Flag*` names and `Default*` values); ports `audiosocket`
  and `control`; startup (period 2, 30 failures) and readiness probes on
  `/readyz`, liveness on `/healthz`, port `control`;
  `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, caps
  drop ALL; volumes: the ConfigMap `<fullname>-streams` (item
  `streams.json`, **not** optional: the pod waits for the controller's
  first compile, like the PBX pod waits for the agent token) read-only at
  `/etc/patchbay/streams`, and an emptyDir at `/tmp`;
- Service `<fullname>-streams`, ClusterIP, ports `audiosocket`
  (`ports.audioSocket`) and `control` (`ports.control`), selecting the
  bridge pod;
- the controller: `--stream-bridge-url=http://<fullname>-streams.<ns>.svc:<control>`
  while enabled; its Role's ConfigMap `update` resourceNames gain
  `<fullname>-streams`;
- the PodMonitor scrapes the bridge's `control` port `/metrics`;
- NOTES.txt: streams, and `<fullname>-streams` among the objects the
  controller leaves behind on uninstall.

The default `settings.json` render must stay equal to the landed fixture
(test/chart compares them), and the module list equal to
`compile.Modules()`. ci values: `all-features-values.yaml` gains a tuner
pool and a call limit; new `no-streams-values.yaml`
(`streamBridge.enabled: false`). *test/chart:* `TestChartStreamBridge`
(every bullet above, against the contract constants: flag names and
defaults, `install.Names`, `install.StreamsKey`,
`streambridge.DefaultStreamsDir`, `bridgeapi` ports and paths, the
pinned image with a digest), `TestChartStreamBridgeService`,
`TestChartStreamBridgeDisabled` (no objects; settings without
`streamBridge`), `TestChartControllerStreams` (the flag equals
`install.StreamBridgeURL(names, "telephony", 9093)`; the Role),
`TestChartSettingsStreams` (pools and limit reach settings.json; the
landed `required` failures), `TestChartPodMonitorStreamBridge`.

**PK.5 fakephone.**
- `fakephone source` per the contract (`script/source.go`), flags
  `--tuner-listen` (default `:5004`), `--tuners` (2), `--lock-delay`
  (1.5 s), `--radio-listen` (`:8000`); the WAV is real time in 20 ms
  writes; the tone keeps its phase across writes. *Tests:*
  `TestSourceWAV` (the header fields; the tone at `hz`, measured at 48
  kHz), `TestSourceRealTime` (1 s of audio per second ± 10%),
  `TestSourceTuners` (two open, the third 503 with the header, `Refused`;
  a close frees one), `TestSourceOccupy`, `TestSourceLockDelay`,
  `TestSourceStatus`, `TestRadioUnlimited`.
- `fakephone call` gains `--expect-ended-by=remote|local|any` (default
  `any`) and `--expect-first-tone-within=<d>` (fail unless a frame with
  `--expect-hz` came within d of the first packet sent); it stops sending
  once the PBX's BYE arrives; the media line keeps every field the e2e
  regex reads and appends ` answered=<duration> endedBy=<local|remote>`.
  *Tests* beside the existing call tests.

**PK.6 e2e helpers** (`test/e2e/streamkit_test.go`, `//go:build e2e`,
package `e2e`; vetted, run only by the integrator's tests):

```go
const streamBridgeImage = "ghcr.io/christopherjmiller/patchbay-stream-bridge:e2e"
func deployFakeSources(ctx context.Context) error  // Deployment "fake-sources" (tools image, `source …`), Services "fake-tuner" (5004) and "fake-radio" (8000); idempotent
func fakeSourceStatus(ctx context.Context, t *testing.T, service string, port int) script.SourceStatus // services/proxy
func occupyTuners(ctx context.Context, t *testing.T, n int) script.SourceStatus
func bridgeObservations(ctx context.Context, t *testing.T) bridgeapi.Observations // services/proxy to <fullname>-streams:9093
func waitStream(ctx context.Context, t *testing.T, name, what string, timeout time.Duration, ok func(*v1alpha1.Stream) (bool, string)) *v1alpha1.Stream
type streamCall struct{ Number string; Duration time.Duration; ExpectHz float64; MinToneFrames int; FirstToneWithin time.Duration; ExpectPrompt bool; EndedBy string }
func callStream(ctx context.Context, t *testing.T, pod string, c streamCall) fakephoneResult // one-shot `fakephone call` as kitchen, sending 300 Hz
func e2eStreams() []v1alpha1.Stream // the site Streams of §6.4 I.5
```

and `test/e2e/values.yaml` gains `streamBridge.image` (`tag: e2e`,
`pullPolicy: Never`), `streams.callLimit: 20s` and `streams.tunerPools:
[{name: tuner, hosts: [fake-tuner.patchbay-e2e.svc], max: 1}]`.

**Packaging acceptance:** for the Go you own, rule 4's commands with
`P="./test/chart/... ./test/e2e/fakephone/..."` (and `go vet -tags e2e
./test/e2e/`, `-tags image ./test/image/`); `nix develop -c make lint`
(helm lint every ci values file); `nix develop -c make image
image-stream-bridge image-check-stream-bridge`; `nix develop -c
actionlint`; `renovate-config-validator --strict` if the flake has it;
`make check-versions`.

### 6.4 Integrator (I)

1. **Reconcile** the stages' reported deviations. Compile-time assertions
   in the controller root: `var _ streamstate.Source =
   (*bridgeclient.Client)(nil)`; in `cmd/patchbay`: `var _
   streambridge.Source = (*streambridge.FFmpegSource)(nil)`.
2. **`cmd/patchbay`.** `stream-bridge`: the four `streambridge.Flag*`
   flags (defaults `Default*`), the zap flags, a signal context, `Run`;
   the stub and `TestStreamBridgeIsAStub` go (a flags test replaces it).
   `controller`: `--stream-bridge-url` (default
   `install.StreamBridgeURL(names, ns, bridgeapi.DefaultControlPort)` when
   settings' `streamBridge` is set; empty means no polling).
3. **Controller root.** `Options.StreamBridgeURL`; `Setup` builds
   `bridgeclient` and `streamstate.Setup` (Bridge nil without a
   `streamBridge`), `numbers.Setup` Person only; `TestSetupWiresEverything`
   extended (a Stream reaches `<fullname>-streams`; with a fake bridge it
   is Idle).
4. **`patchbay status`.** A Stream row shows its listeners and pool
   (`Stream king5 (705): Playing (2 listeners, pool hdhomerun)`) and its
   last failure when Ready is not True.
5. **Harness.** Load the bridge image; `deployFakeSources` before the
   chart's rollout wait; the e2e values; the **site Streams** created at
   install with the others (so the kubelet's sync is done by the time the
   stream tests run): `tuner-a` 705
   `http://fake-tuner.patchbay-e2e.svc:5004/auto/v5.1?hz=1100`, `tuner-b`
   707 `…/auto/v7.1?hz=1300`, `radio-a` 750
   `http://fake-radio.patchbay-e2e.svc:8000/radio-a?hz=500`, `radio-b` 751
   `…/radio-b?hz=700`.
6. **e2e tests** (new file `test/e2e/streams_test.go`, which Go runs after
   release A's sequential tests; one-shot calls as kitchen; tones are
   multiples of 50 Hz away from the SIT tone):

   | Test | Steps | Assertions |
   |---|---|---|
   | `TestStreamBridgeUp` (sequential) | read the bridge pod; exec `cat /proc/1/status` and `ffmpeg -version` in it; wait for the bridge's observations | 1 replica, Recreate, Ready, no restarts this run; no service account token mounted; PID 1 uid 65532, no capabilities, NoNewPrivs, read-only root; `Bridge.FFmpeg` is the exec'd line; `StreamsVersion` equals `<fullname>-streams`' within 120 s; every site Stream Idle (Ready True `Idle`) within 30 s more, `tunerPool` `tuner` on tuner-a and tuner-b, empty on the radios |
   | `TestStreamBridgeDown` (sequential, after Up) | scale `<fullname>-streams` to 0 and wait for its pod to go; call 750 (6 s, `ExpectPrompt`, ended by remote); scale to 1 | the call is answered and plays the prompt (≥ 2 s not silent, ≥ 1 s voice), the PBX hangs up; radio-a Unknown (Ready Unknown `BridgeUnreachable`) within 15 s; after the scale-up, Idle within 30 s with lastPlayed/lastFailure as before |
   | `TestStreamRadio` (parallel) | call 750 for 6 s expecting 500 Hz; meanwhile call 751 for 6 s expecting 700 Hz; then call 751 for 40 s (`EndedBy` remote) | both play at once (no limit): ≥ 200 tone frames each, the first within 3 s; radio-a and radio-b Playing with 1 listener while up; Idle within 15 s after; fake-radio's open streams back to 0 within 10 s of the hang-ups; the long call is ended by the PBX at 20 s ± 2 s (the e2e limit) and heard 700 Hz until then |
   | `TestStreamTuner` (parallel; subtests in order) | **shared:** two concurrent calls to 705 (8 s); **limit:** call 705 for 12 s; once it plays, call 707 (`ExpectPrompt`); **refused:** `occupyTuners(2)`, call 705 (`ExpectPrompt`), `occupyTuners(0)`, call 705 for 5 s | **shared:** both hear 1100 Hz; while both are up fake-tuner has exactly 1 open stream and `Opened["/auto/v5.1"]` grew by 1; the bridge's pool `tuner` has one session with 2 listeners; tuner-a Playing, 2 listeners; fake-tuner's open streams 0 within 10 s of the hang-ups; tuner-a Idle. **limit:** the 707 call plays the prompt and the PBX hangs up; tuner-b Busy (Ready False `TunerBusy`, the message names tuner-a (705)) while 705 plays; tuner-b's lastFailure `TunerBusy`; a `CallRefused` Event on tuner-b; fake-tuner never opened `/auto/v7.1`; after 705 ends, tuner-b Idle within 15 s with the failure kept. **refused:** the call plays the prompt; tuner-a Failing (Ready False `SourceFailed`, the message mentions 503); a `SourceFailed` Event; fake-tuner's `Refused` grew; the next call plays 1100 Hz and tuner-a is Idle after it (lastPlayed later than lastFailure) |
   | `TestStreamAdded` (parallel) | create Stream `added` 759 (`…/added?hz=600`); call it once Idle; delete it; call 759 | Pending (Ready False `BridgePending`) first, then Idle within 150 s (the kubelet's sync, measured); the call hears 600 Hz; after the delete `<fullname>-streams` no longer has it and the call plays the prompt once pbx-config's new version is applied |

   **Budget** (added to release A's, images cached): the bridge image
   build and `kind load` about 15 s; its rollout overlaps the PBX's;
   `TestStreamBridgeUp` about 10 s (the site Streams are created at
   install, about 100 s of sequential tests earlier); `TestStreamBridgeDown`
   about 45 s; the parallel stream tests (about 80, 50 and 150 s) run under
   `TestPhoneLifecycle` (about 370 s), adding nothing. About 70 s in all,
   under the 3 min allowed. Report the measured times in "as built".
7. **Docs.** New **docs/streams.md**: how a stream call works; sources
   (HDHomeRun URLs, SomaFM's direct mounts and why not `.pls`, ffmpeg's
   user agent); tuner pools and the house's policy; the time limit; states
   and conditions; troubleshooting (Pending, Busy, Failing messages, the
   bridge's observations through `kubectl port-forward`); the bridge
   image's licences and where Alpine's sources are; the house's
   manifests (§12). docs/e2e.md (the stream tests, the fake sources),
   docs/operations.md (the bridge: a restart drops stream calls; logs;
   metrics), README (streams), `config/samples` (a radio Stream beside
   king5).
8. **CLAUDE.md.** The new contracts in its contracts paragraph; the
   architecture sketch gains the bridge; deviations: `Dial(AudioSocket/…)`
   instead of the AudioSocket application, at 16 kHz (§3);
   `chan_audiosocket`, `codec_resample`, `func_timeout` loaded and
   `app_audiosocket` not; the bridge's objects `<fullname>-streams` (not
   `patchbay-stream-bridge`); streams.json in its own ConfigMap, not among
   the bundle's files or its version; the time limit and tuner pools as
   settings; sources refused as playlists, credentials or non-http(s);
   `Busy` and `Pending` beside design §9's three states; the bridge's
   unauthenticated control API on 9093.
9. **This plan's "as built"** (a §6.5 like release A's), with the measured
   e2e times.

**Integrator acceptance:**

```sh
cd /home/chris/Repos/patchbay
nix develop -c make generate check-generated fmt vet lint test   # green
nix develop -c go mod tidy -diff                                 # empty
nix develop -c make check-hygiene
nix develop -c make asterisk-check
nix develop -c make image-stream-bridge image-check-stream-bridge
nix develop -c make e2e                                          # leaves no cluster
```

---

## 7. Interfaces on the wire and in the cluster

### 7.1 AudioSocket (Asterisk → bridge, TCP 9092)

| Direction | Frames |
|---|---|
| Asterisk → bridge | one `0x01` UUID (16 bytes) first; then `0x12` audio (640 bytes per 20 ms, the caller's microphone, discarded), `0x03` DTMF (ignored); a close ends the call |
| bridge → Asterisk | `0x12` audio, 640 bytes every 20 ms (silence while waiting); `0x00` hangup then close to refuse or end ("not available" follows) |

### 7.2 The bridge's control API (controller → bridge, TCP 9093)

| Call | Body | Notes |
|---|---|---|
| `GET /v1/observations` | → `streambridge/api.Observations` | no token; polled every 5 s |
| `GET /healthz`, `/readyz` | — | probes |
| `GET /metrics` | Prometheus text | the PodMonitor |

### 7.3 Kubernetes objects (B's additions)

| Object | Writer | Reader | Format |
|---|---|---|---|
| ConfigMap `<fullname>-streams` | controller (bundle reconciler, before pbx-config) | bridge (mounted), controller (`ReadStreams`) | key `streams.json`: `streams.Document`; annotation `streams-version` |
| ConfigMap `<fullname>-settings` | chart | controller | settings.json gains `streamBridge` and `streams` |
| Deployment, Service, ServiceAccount `<fullname>-streams` | chart | — | the bridge; Service ports `audiosocket` 9092, `control` 9093 |
| `Stream.status` | controller | CLI, people | §6.2 RT.5 |

---

## 8. Bench: what only the real HDHomeRun, phone and SomaFM answer

Each unknown is one value with a safe default; the answer is a one-line
change. Run them with the Desk phone and `http://192.168.0.58/status.json`
open.

| # | Question | Switch (file) | Safe default | When answered |
|---|---|---|---|---|
| B1 | How long until KING 5 is heard after dialling 705 (tuner lock + ffmpeg's probe)? Does ffmpeg find the audio with 1 MB / 1 s? | `ffmpegProbeSize`, `ffmpegAnalyzeDuration` (streambridge/source.go, a contract) | 1 MB, 1 s (§2(m): 0.15 s on a generated TS) | if over ~3 s, lower them; if the Stream fails with "could not find codec parameters", raise the duration |
| B2 | Which audio track plays: the main one, or a second-language or described-video one? | `FFmpegArgs` (add `-map 0:a:0`, or a language) | ffmpeg's choice (the track with the most channels) | if wrong, map the first audio track |
| B3 | How soon does the HDHomeRun free a tuner after ffmpeg disconnects (`status.json`)? Does a preempted channel start cleanly 1 s later? | `streambridge.PreemptSettle`, `Grace` | 1 s, 5 s | exit criterion "free within ~10 s"; if a preempted start gets 503, raise `PreemptSettle` |
| B4 | With Plex recording on both tuners, what do ffmpeg and the Stream say? | none (the bridge reports ffmpeg's last line) | `Failing: … 5XX …` | record the real message in docs/streams.md |
| B5 | Does the 7945 take G.722 on a stream call, and does TV sound wideband? | none (`streams.Codec`) | slin16 to the bridge; Asterisk transcodes | if it negotiates G.711 it is narrowband, which is fine; record it |
| B6 | Is TV audio loud enough on the handset (AC-3 dialogue normalisation, the 5.1 downmix)? | `FFmpegArgs` (an `-af volume=…`) | no filter | add a fixed gain; `loudnorm` costs CPU per session |
| B7 | Does SomaFM play from the cluster: https with certificate checks (Alpine's CA bundle), ffmpeg's user agent, musl's DNS? | `FFmpegArgs` (`-user_agent`), the image | verify on; ffmpeg's own agent (`Lavf`), which SomaFM served on 2026-10-16 while it redirected curl's to `alert.mp3` | if SomaFM sends its notice instead of music, set a user agent |
| B8 | What does the 7945 show when Asterisk hangs up at the time limit? | none | no prompt | if it is confusing, a "time limit" prompt is a later change (sounds) |
| B9 | Does Asterisk on `top`'s host network resolve `<fullname>-streams.telephony.svc` (ClusterFirstWithHostNet on Talos)? | settings `streamBridge` | the Service name (e2e proves it in kind) | if not, render the Service's ClusterIP (a chart value) |
| B10 | Does Plex still record on its tuner while a phone holds the other, and what does Plex do when it wants both? | the pool's `max` (luma-homeops values) | 1 | Chris's policy; record Plex's behaviour |
| B11 | Does the bridge pod reach `192.168.0.58:5004` from the pod network? | none | yes (pods reach the LAN through the node) | if a NetworkPolicy blocks it, allow it in luma-homeops |

---

## 9. Exit criteria → acceptance tests

`[real]` marks what only the house's hardware closes; the automated test
beside it covers everything up to that point.

| Criterion | Automated test (owner) | Real-world close |
|---|---|---|
| Dialling 705 plays within about 3 s | e2e `TestStreamTuner/shared` and `TestStreamRadio` (first tone within 3 s behind a 1.5 s fake tuner lock) (I); `TestFFmpegDecodesMPEGTS` (first PCM in under 1 s) (BR) | `[real]` 705 on the Desk phone, timed (B1) |
| Two phones on 705 use one tuner | e2e `TestStreamTuner/shared`: one open stream on the fake tuner and one bridge session with 2 listeners (I); `TestOneSessionPerSource` (BR) | `[real]` two phones, `status.json` |
| Both hang up; the tuner is free within ~10 s | e2e `TestStreamTuner/shared` (open streams 0 within 10 s) (I); `TestGrace` (BR) | `[real]` `status.json` (B3) |
| Both tuners busy → "unavailable", Stream `Failing` | e2e `TestStreamTuner/refused` (fake DVR holds both: prompt, `SourceFailed`, 503) (I); `TestFFmpeg503`, `TestStartFails` (BR); `TestDecide` row 7 (RT) | `[real]` Plex recording two shows (B4) |
| A second channel while one plays → "not available", Stream says why | e2e `TestStreamTuner/limit` (I); `TestPoolLimit`, `TestPoolRace` (BR); `TestDecide` row 8 (RT) | `[real]` 705 then 707 |
| The time limit ends a call | e2e `TestStreamRadio` (20 s in e2e) (I); goldens and `TestRealAsteriskStreams` (the dialplan line) (RT); §3 (4 s) | `[real]` a short `callLimit` on the house for one test call (B8) |
| Internet radio, no tuner limit | e2e `TestStreamRadio` (two at once) (I); `TestRadioHasNoLimit` (BR) | `[real]` 750–755 (B7) |
| AudioSocket sample rate answered by test | §3 (the pinned image, 2026-10-16); `TestRealAsteriskStreams` (RT); `make asterisk-check` | `[real]` B5 |
| CI green, image published | `make …` (all); CI's image, publish and release jobs (PK) | the first `v0.2.0` tag |

---

## 10. Review Focus

The inputs most likely to break a real user, and what pins each.

1. **A playlist or a web page as the source.** People copy SomaFM's
   `.pls` link from channels.json, or a station's web page. The CRD
   refuses `.pls`/`.m3u` (`TestSchemaRejects`); a page that is not audio
   fails as "not an audio stream the bridge reads" (`TestFFmpegHTMLPage`);
   a Stream from before the rule is `InvalidSource`, never a failed
   compile (RT.1 `TestStreamRefusedSource`, golden `streams-refused`).
2. **Two phones dialling two channels at the same moment, or the DVR
   taking the other tuner.** Exactly one may start (`TestPoolRace`); the
   HDHomeRun's 503 is a clear `Failing` (`TestFFmpeg503`, e2e refused).
3. **A phone left off the hook.** The time limit ends it (e2e
   `TestStreamRadio`, §3), and a refused or failed call never lingers
   (`TestServeRefusalIsHangup`).
4. **A tuner device named two ways.** `http://hdhomerun.lan:5004/…` is not
   in a pool listing `192.168.0.58`: the Pool column shows it empty
   (`TestPoolMatching`, docs/streams.md).
5. **An internet station that bursts, stalls or jitters.** Icecast sends
   its first seconds at once; the queue keeps 1 s and the newest audio
   (`TestServeBurst`), silence fills gaps (`TestServeSilenceWhileStarting`),
   and a stall ends the call in 10 s, not never (`TestStall`).
6. **The bridge restarting mid-call** (an upgrade, a node drain). Calls
   drop, the Streams go Unknown, and their history survives in status
   (e2e `TestStreamBridgeDown`, `TestFold`).
7. **A source that redirects.** SomaFM sends some user agents to a notice;
   ffmpeg follows redirects only within http/https (§2(m)), and B7 checks
   SomaFM's answer to ffmpeg's agent.

---

## 11. Known limitations accepted for B

- **A new or changed Stream waits for the kubelet** to update the bridge's
  mounted `<fullname>-streams`: usually under a minute, up to about two.
  Meanwhile it is `Pending` and plays "not available" (§2(b)).
- **Stream calls drop when the bridge restarts or upgrades**: one replica,
  `Recreate` (§2(h)). Phone calls are unaffected.
- **The phone shows the number dialled, not the stream's name**: the
  display name is free text and never enters the dialplan.
- **No warning at the time limit**: the call ends (§2(e)).
- **The first caller hears 1–3 s of silence** while a tuner locks; later
  callers join live, with no rewind.
- **Pool membership is by host as written** (§2(c)).
- **The policy can cost the DVR its second recording** while a phone
  listens (one tuner each); the time limit bounds how long.
- **G.711 callers hear narrowband**; only G.722 phones get 16 kHz.
- **No playlist resolution**: a retired SomaFM host makes its Stream
  `Failing` until the URL is edited (§2(d)).
- **`Failing` stays until a call plays the stream**: the controller never
  probes a source.
- **Anyone in the cluster can read the bridge's observations** (§2(g)).
- **Wideband is not pinned by CI**: the e2e fakephone speaks G.711, so the
  16 kHz path rests on §3 and B5.
- **ffmpeg's patch level floats** within Alpine 3.24 between image
  builds; the published digest pins what runs (§2(f)).
- **ATSC 3.0 channels** (AC-4 audio) would not decode; the FLEX DUO is
  ATSC 1.0, so the house is unaffected.

---

## 12. The house's manifests (luma-homeops)

The integrator copies these into docs/streams.md; the lead copies them into
luma-homeops (never by patchbay's stages). The Argo app's Helm values for
the chart (`cluster/applications/telephony-release.yaml`) gain:

```yaml
streamBridge:
  enabled: true
streams:
  callLimit: 2h
  tunerPools:
    - name: hdhomerun
      hosts: ["192.168.0.58"]
      max: 1          # of its 2 tuners: Plex DVR keeps the other
```

and `cluster/telephony/streams.yaml`:

```yaml
# TV from the HDHomeRun FLEX DUO (at most one tuner for phones):
# 7 + the major channel.
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: komo4}
spec: {number: "704", name: KOMO 4, source: "http://192.168.0.58:5004/auto/v4.1"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: king5}
spec: {number: "705", name: KING 5, source: "http://192.168.0.58:5004/auto/v5.1"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: kiro7}
spec: {number: "707", name: KIRO 7, source: "http://192.168.0.58:5004/auto/v7.1"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: fox13}
spec: {number: "713", name: FOX 13, source: "http://192.168.0.58:5004/auto/v13.1"}
---
# SomaFM (no tuner, no limit): each channel's 128 kbps MP3 mount, from
# its .pls (the bridge reads streams, not playlists).
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: groovesalad}
spec: {number: "750", name: Groove Salad, source: "https://ice2.somafm.com/groovesalad-128-mp3"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: dronezone}
spec: {number: "751", name: Drone Zone, source: "https://ice2.somafm.com/dronezone-128-mp3"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: indiepop}
spec: {number: "752", name: Indie Pop Rocks!, source: "https://ice2.somafm.com/indiepop-128-mp3"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: secretagent}
spec: {number: "753", name: Secret Agent, source: "https://ice2.somafm.com/secretagent-128-mp3"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: defcon}
spec: {number: "754", name: DEF CON Radio, source: "https://ice2.somafm.com/defcon-128-mp3"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: u80s}
spec: {number: "755", name: Underground 80s, source: "https://ice2.somafm.com/u80s-128-mp3"}
```

No router change: Asterisk reaches the bridge inside the cluster, and the
bridge pod reaches the HDHomeRun and SomaFM as any pod reaches the LAN and
the internet (B11). The six SomaFM channels are Chris's examples; by
listeners on 2026-10-16 they rank 1, 2, 3, 7, 8 and 12 of SomaFM's.
