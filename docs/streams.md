# Streams

A `Stream` is a number that plays a live audio source: a TV channel from a
tuner, or an internet radio station. Pick up a phone, dial `705`, and you
hear KING 5; hang up, and five seconds later the tuner is free again. This
is release B (design §9; the plan, with the reasoning behind every choice,
is [docs/plans/release-b.md](plans/release-b.md)).

```yaml
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream
metadata: {name: king5}
spec: {number: "705", name: KING 5, source: "http://192.168.0.58:5004/auto/v5.1"}
```

Contents: [How a call works](#how-a-stream-call-works) ·
[Sources](#sources) · [Tuner pools](#tuner-pools) ·
[The time limit](#the-time-limit) · [States](#states-and-conditions) ·
[Troubleshooting](#troubleshooting) · [Who can reach the bridge](#who-can-reach-the-bridge) ·
[The image](#the-bridge-image-and-its-licences) · [The house](#the-house-luma-homeops)

## How a stream call works

```
phone --SIP/RTP--> Asterisk (PBX pod, host network)
                     705: Answer(), Set(TIMEOUT(absolute)=<limit>),
                          Dial(AudioSocket/<fullname>-streams.<ns>.svc:9092/<uuid>/c(slin16),,gS(<limit>))
                     --TCP 9092, AudioSocket--> stream bridge (<fullname>-streams, pod network)
                                                  one ffmpeg per source --HTTP(S)--> tuner / Icecast
                     when Dial returns: "not available"
```

1. A Stream's number answers at once, sets the time limit, and dials the
   stream bridge over Asterisk's AudioSocket channel driver with the
   Stream's UUID (`uuid5` of its name: it never changes while the name
   does not). Audio is 16 kHz signed linear both ways (`slin16`, 640-byte
   frames every 20 ms); Asterisk transcodes to the phone's codec. A G.722
   phone (the 79x5s) hears it wideband, a G.711 one narrowband.
2. The bridge looks the UUID up in `streams.json` (the `<fullname>-streams`
   ConfigMap the controller compiles, mounted). An unknown UUID, a tuner
   pool at its limit, or a bridge at its bounds (64 calls, 8 sources)
   gets a hangup frame: `Dial` returns, and the caller hears "not
   available".
3. Otherwise the caller joins the source's **session**: one ffmpeg per
   source URL, shared by every caller of every Stream with that source. The
   first caller starts it; the others join it live (no rewind). Until the
   first audio arrives (a tuner locking: 1 to 3 s) the caller hears
   silence.
4. Each caller gets one frame every 20 ms on the bridge's clock, from a
   queue of its own (1 s, the oldest dropped when full), or silence when
   the queue is empty. A slow caller never holds up the others, and a
   station that bursts its first seconds (Icecast does) is cut to the
   newest second.
5. Audio from the phone is thrown away; so are key presses.
6. When the last caller leaves, the session drains for 5 s (a caller who
   redials at once hears audio at once), then ffmpeg is stopped and the
   tuner is free.
7. If the source fails (it refuses, ends, sends nothing for 10 s, or no
   audio within 15 s of starting), every caller of that session gets a
   hangup frame and hears "not available", and each Stream that had a
   caller records the failure.
8. Asterisk ends the call at the time limit ([below](#the-time-limit));
   the bridge ends any call still open 5 s after it.

The bridge is one Deployment (`<fullname>-streams`, one replica,
`Recreate`: two bridges would hold two tuners for one channel) with a
ClusterIP Service of the same name (ports `audiosocket` 9092 and `control`
9093). It holds no Kubernetes credentials (no service account token) and
runs as 65532 with no capabilities on a read-only root. A restart of the
bridge drops every stream call; phone calls are unaffected.

The controller polls the bridge's observations every 5 s
(`GET :9093/v1/observations`, no token: names, numbers, sources, counts and
bounded error text) and writes each Stream's status from them. It never
probes a source itself: a probe would hold a tuner.

## Sources

`spec.source` is a **direct** stream URL that ffmpeg can read:

- `http://` or `https://`, with a host; at most 2048 characters;
- no white space or control characters, and no credentials (`user@`,
  `user:password@`): status, Events and the bridge's observations show the
  source, so it must hold no secret;
- not a playlist: a path ending in `.pls` or `.m3u` (any case) is refused
  at `kubectl apply` by the CRD. HLS (`.m3u8`) is a stream to ffmpeg and is
  allowed.

**Playlists are not resolved** (plan §2(d)). The bridge never fetches a
`.pls` or `.m3u`: resolving one would be a second HTTP client and parser on
untrusted input, for a list of interchangeable mirrors of one stream. Open
the playlist once and use one of its entries. If that host is retired, the
Stream turns `Failing` until its URL is edited.

ffmpeg reads the source with only the `http`, `https`, `tcp`, `tls` and
`crypto` protocols (also through a redirect or an HLS playlist), only the
`mpegts`, `mp3`, `aac`, `ogg`, `flac`, `wav`, `hls` and `mov` demuxers, and
fails a read that stalls for 10 s. https sources are verified against the
image's CA bundle (Alpine's `ca-certificates`). The exact command line is
`streambridge.FFmpegArgs`:

```
ffmpeg -nostdin -hide_banner -loglevel error
  -protocol_whitelist http,https,tcp,tls,crypto
  -format_whitelist mpegts,mp3,aac,ogg,flac,wav,hls,mov
  -rw_timeout 10000000 -probesize 1000000 -analyzeduration 1000000
  [-tls_verify 1]                          # https sources only
  -i <source> -vn -sn -dn -ac 1 -ar 16000 -acodec pcm_s16le -f s16le pipe:1
```

ffmpeg's own user agent (`Lavf/<version>`) is sent. SomaFM served it on
2026-10-16 (it redirects some agents, curl's among them, to a notice); if a
station sends its notice instead of music, that is bench question B7.

**An HDHomeRun** streams a channel at
`http://<device>:5004/auto/v<channel>`, for example
`http://192.168.0.58:5004/auto/v5.1` for 5.1. `http://<device>/lineup.json`
lists the channels it has found, and `http://<device>/status.json` which
tuners are in use. It sends MPEG-TS (MPEG-2 video and AC-3 audio for
ATSC 1.0); ffmpeg keeps the audio. A FLEX DUO has two tuners; with both in
use it answers 503 (`X-HDHomeRun-Error: 805 All Tuners In Use`).

**SomaFM** lists its channels at `https://api.somafm.com/channels.json`;
each channel's `playlists` are `.pls` files whose entries are direct
Icecast mounts such as `https://ice2.somafm.com/groovesalad-128-mp3`
(the `.pls` lists several `ice*` hosts serving the same mount). Use that mount, not the `.pls`.

## Tuner pools

A tuner device can play only so many channels at once, so the install
declares its tuner devices as **pools** (the chart's `streams.tunerPools`,
rendered into the settings):

```yaml
streams:
  tunerPools:
    - name: hdhomerun          # a DNS label, shown as status.tunerPool
      hosts: ["192.168.0.58"]  # the host as the Streams' sources write it
      max: 1                   # sources of the pool that may play at once
```

- A Stream belongs to a pool when its source URL's host (any port,
  compared lower-case) is one of the pool's hosts. `kubectl get streams -o
  wide` shows the pool, and `status.tunerPool` holds it. There is no field
  on the Stream: four channels of one tuner share one limit, and a new
  channel needs nothing but its URL.
- **List every form of the host the Streams use.** A Stream that names the
  device `hdhomerun.lan` is not in a pool that lists only `192.168.0.58`:
  its Pool column is empty, and it is not limited.
- `max` counts **sources** (sessions), not calls: everyone on one channel
  shares one tuner, so ten phones on `705` hold one tuner.
- A call that needs a new source in a pool already playing `max`:
  - if one of the pool's sessions is draining (its last caller left less
    than 5 s ago), that session is stopped, and the new one starts a second
    later (time for the device to free its tuner); the caller hears
    silence meanwhile;
  - otherwise the call is refused: the caller hears "not available", the
    refused Stream records `lastFailure` `TunerBusy` ("pool hdhomerun (1 at
    a time) is playing king5 (705)") with a `CallRefused` Event, and while
    the pool stays full it is `Busy`.
  Counting and starting are one step, so two phones dialling two channels
  at the same moment start exactly one.
- A source on no pool's host (internet radio) has no limit but the bridge's
  own (8 sources, 64 calls).
- Settings rules (the controller refuses the settings at start otherwise,
  naming the field): names are DNS labels, each used once; `hosts` is not
  empty, each an IP address (IPv6 without brackets) or a DNS name, and no
  host is in two pools; `max` is 1 to 16.

**The house's policy** (Chris, 2026-10-16): the HDHomeRun FLEX DUO has two
tuners and Plex DVR uses them too, so phones may hold **at most one**
(`max: 1`): the DVR always keeps the other. "Both tuners busy" for a phone
is the DVR recording on both, which makes the next tuner call fail with the
device's 503 (`Failing`). While a phone listens, the DVR has one tuner, so
a second recording at that moment fails on Plex's side; the time limit
bounds how long that can last.

## The time limit

Every stream call ends after `streams.callLimit` (default `2h`; whole
seconds from `10s` to `24h`), so a phone left off the hook cannot hold a
tuner for ever. It applies to every Stream, radio included. There is no
warning before it (a prompt would be a new sound in every voice; a later
change). Three things enforce it:

- Asterisk, on the caller: the dialplan sets `TIMEOUT(absolute)` right
  after answering, and Asterisk hangs up at it.
- Asterisk, on the stream's own leg: `Dial(…,,gS(<limit>))` puts the same
  limit on the AudioSocket channel, from its answer. A phone that
  **transfers** a stream call leaves with its own channel and its
  `TIMEOUT(absolute)`; after a *blind* transfer (a REFER naming the
  target) the stream's leg keeps `S()`, so the call still ends at the
  limit (`make asterisk-check`, `TestRealAsteriskStreams/transferred`, to
  the echo test).
- The bridge: `streams.json` carries the limit (`callLimitSeconds`), and
  the bridge ends any call still open **5 s after it** (a hangup frame,
  then the close), however the call got there. This covers what Asterisk
  loses track of. An *attended* transfer into an application (consult the
  echo test, then complete the transfer) moves the stream's leg into a
  channel that holds neither limit: tested against the pinned Asterisk,
  the call ran until the source stopped. Asterisk takes the same path
  whenever the consulted call is not yet bridged, so a transfer completed
  while the other phone still rings likely ends here too (not tested); an
  attended transfer to a phone that answered moves the leg between bridges
  and should keep `S()` (not tested either). A 79x5's Transfer key
  usually consults first, so on the bench either may end it (B12). And a
  client in the cluster that dials the bridge itself. Such an end is
  logged (`call ended past its time limit`, with the peer's address) and
  counted in `patchbay_stream_bridge_call_limit_ends_total`.

## States and conditions

The controller decides each Stream with a pure rule table (first match
wins). `status.state` is for display; tools read the `Ready` condition.

| State | Ready | When | Message |
|---|---|---|---|
| `Invalid` | False `NumberConflict` / `NumberNotInPlan` | an older resource holds its number, or it is outside `7XX` | as for every resource |
| `Invalid` | False `InvalidSource` | its source breaks a rule above (a Stream written before release B's CRD) | the rule it breaks; its number plays "not available" and it is left out of `streams.json` |
| `Unavailable` | False `NoStreamBridge` | the chart's `streamBridge.enabled` is false | `streams are disabled (streamBridge.enabled is false): dialling 705 plays "not available"` |
| `Unknown` | Unknown `BridgeUnreachable` | the controller cannot poll the bridge, or its last observations are over 30 s old | `cannot reach the stream bridge: …`; listeners, pool and history are kept |
| `Pending` | False `BridgePending` | the controller has not compiled it as it is yet, or the bridge has not loaded that `streams.json` | `this stream is not compiled yet (compiled streams <v>)` or `the stream bridge has not loaded this stream yet (it plays streams <v>, compiled <v>)` |
| `Playing` | True `Playing` | someone is listening | `2 listeners; tuner pool hdhomerun` |
| `Failing` | False `SourceFailed` | its last failure is `SourceFailed` or `SourceEnded` and newer than when it last played | `<time>: <why>` |
| `Busy` | False `TunerBusy` | its pool plays `max` sources with listeners, none of them its own | `pool hdhomerun (1 at a time) is playing king5 (705)` |
| `Idle` | True `Idle` | otherwise: dialling it now should play | `nobody is listening; last played <time>` |

- `status.listeners` and `status.tunerPool` are the bridge's and the
  compiled pool; `status.lastPlayed` is when a caller last got its first
  audio; `status.lastFailure` (`at`, `reason` `SourceFailed`, `SourceEnded`
  or `TunerBusy`, `message`) is the latest failure. The controller copies
  them from the bridge, whose memory is lost when it restarts, so they
  survive a bridge restart.
- `Failing` stays until a call plays the stream again: the controller
  never probes a source. A `TunerBusy` refusal never makes a Stream
  `Failing`; it is history in `lastFailure` and an Event, and `Busy` says
  whether the pool is full now. Nor does a refusal hide a broken source:
  while a Stream's last failure is its source's and nothing has played it
  since, a refused call leaves that failure in `lastFailure` (the bridge
  keeps it too) and sends no `CallRefused` Event, so the Stream stays
  `Failing` when the pool frees up (its source was never tried again);
  `refused_calls_total{reason="tuner_busy"}` still counts the refusal.
- Each new failure is one Event on the Stream: `SourceFailed` (Warning,
  `<reason>: <message>`) for a source that failed or ended, `CallRefused`
  (Normal, the bridge's message) for a refusal. A state change is a
  `StateChanged` Event.
- A new or changed Stream is `Pending` until the kubelet has updated the
  bridge's mounted `<fullname>-streams` (its periodic sync: usually under a
  minute, up to about two). Meanwhile its number plays "not available".

`patchbay status` prints each Stream with its listeners and pool, and its
last failure while Ready is not True:

```
Stream king5 (705): Playing (2 listeners, pool hdhomerun)
Stream kiro7 (707): Busy (pool hdhomerun)
  Ready=False TunerBusy: pool hdhomerun (1 at a time) is playing king5 (705)
  last failure 2026-10-16T19:02:11Z TunerBusy: pool hdhomerun (1 at a time) is playing king5 (705)
```

## Troubleshooting

| What you see | Why | What to do |
|---|---|---|
| `Pending`, "not compiled yet" | The controller has not compiled the Stream as it is now (just created or edited), or compiling fails (`patchbay status`'s `Config:` line). | Seconds; otherwise see docs/operations.md. |
| `Pending`, "has not loaded this stream yet" for minutes | The kubelet has not synced the bridge's ConfigMap volume yet (up to about two minutes), or the bridge refuses the file (its log, and `streamsError` in its observations). | Wait two minutes. Restarting the bridge pod loads the file at once (stream calls drop). |
| The bridge pod waits in `ContainerCreating` | `<fullname>-streams` does not exist yet: the controller writes it on its first compile. | Look at the controller. |
| `Busy` | The pool already plays `max` other sources (the message names them). | Hang up the other channel, or raise the pool's `max`. |
| `Failing: … Server returned 5XX Server Error reply` | The source answered 5XX. From an HDHomeRun this is "805 All Tuners In Use": the DVR (or another client) holds every tuner. ffmpeg at `-loglevel error` names the class, not the code, and never shows the HDHomeRun's header. | Wait for the recording to end; `http://<device>/status.json` shows who holds the tuners. |
| `Failing: … Server returned 404 Not Found` | The channel or mount does not exist (a retired SomaFM host, a channel the tuner has not found). | Fix the URL (`lineup.json`, `channels.json`). |
| `Failing: … the source is not an audio stream the bridge reads (…)` | The URL is a web page, a playlist without the extension, or a format outside the demuxer list ("Format not on whitelist", "Invalid data found when processing input"; a short web page ends as "Input/output error"). | Use the direct stream URL. |
| `Failing:` a certificate, DNS or connection error | ffmpeg could not verify the https certificate against the image's CA bundle, resolve the host, or connect. | Check the URL from another pod; a NetworkPolicy may block the bridge's egress. |
| `Failing: … ffmpeg exited with status 0: the source ended (…)` / `SourceEnded` | The source closed the stream, or sent nothing for 10 s, while it played. ffmpeg's own line follows in parentheses: a live source (an HDHomeRun, an Icecast mount) that closes reads `(Error during demuxing: Input/output error)`, a stall `(Error during demuxing: Connection timed out)` or `no audio came from the source for 10s`. This is not the "not an audio stream" row: the stream played first. | The next call starts it again. A station that does this often shows in `session_ends_total{reason="source_ended"}`. |
| `Unknown`, `cannot reach the stream bridge` | The bridge pod is down, restarting or not Ready; its calls dropped. | `kubectl -n <ns> get pods -l app.kubernetes.io/component=stream-bridge` and its log. |
| A stream number plays "not available" | The bridge refused the call or its source failed: the Stream's `lastFailure` and its Events say which. A call to a Stream the bridge has not loaded yet (`Pending`) is refused as an unknown stream (`unknownCalls` in its observations). | As the failure says. |
| A call ends by itself after the time limit | `streams.callLimit` (2h at home), transferred calls included. | Redial; raise the limit if it is too short. |
| The bridge logs `call ended past its time limit`; `call_limit_ends_total` grows | A call outlived the limit by 5 s, so the bridge ended it: one transferred into an application by an attended transfer, or a client in the cluster dialling the bridge itself (the log names its address). | The first is harmless. Otherwise find the client (see [Who can reach the bridge](#who-can-reach-the-bridge)). |

**The bridge's own view.** Its observations hold every loaded stream and
pool, each session's state and listeners, the last failure per stream, and
the decoder it runs:

```sh
kubectl -n <ns> port-forward svc/<fullname>-streams 9093 &
curl -s localhost:9093/v1/observations | jq '.bridge, .pools'
curl -s localhost:9093/v1/observations | jq '.streams[] | {name, number, listeners, session, lastFailure}'
curl -s localhost:9093/readyz      # 503 with the reason until streams.json is loaded and ffmpeg runs
curl -s localhost:9093/metrics | grep patchbay_stream_bridge
kubectl -n <ns> logs deploy/<fullname>-streams   # one line per call and per session start, stop, failure
```

**Metrics.** The bridge (the PodMonitor scrapes its `control` port):
`patchbay_stream_bridge_calls{stream}` (open calls per loaded stream),
`patchbay_stream_bridge_sessions{pool}` (running decoders; `none` for no
pool), `patchbay_stream_bridge_session_starts_total{result="played|failed"}`,
`patchbay_stream_bridge_session_ends_total{reason="source_ended|source_failed|idle|preempted|stopping"}`
(each session once, when it stops),
`patchbay_stream_bridge_refused_calls_total{reason="unknown_stream|tuner_busy|bridge_full"}`,
`patchbay_stream_bridge_call_limit_ends_total` (calls the bridge ended past
the time limit),
`patchbay_stream_bridge_frames_dropped_total`,
`patchbay_stream_bridge_underrun_frames_total` (silence sent while
playing), `patchbay_stream_bridge_streams_version_info{version}`,
`patchbay_stream_bridge_streams_load_failures_total`. The controller:
`patchbay_streams{state}` and `patchbay_stream_bridge_reachable`.

What to alert on: `patchbay_stream_bridge_reachable == 0` (streams are
down); `patchbay_streams{state="Failing"} > 0` (a source is broken until
a call plays it); `rate(patchbay_stream_bridge_session_ends_total{reason="source_ended"}[1h])`
above a few an hour (a station or the tuner keeps dropping mid-call:
`Failing` alone clears on the next good play); and
`increase(patchbay_stream_bridge_call_limit_ends_total[1d]) > 0` if a
client dialling the bridge itself would worry you. patchbay ships no
alert rules.

## Who can reach the bridge

The bridge's two ports answer anyone who can reach its Service or pod in
the cluster; patchbay ships no NetworkPolicy. Accepted for release B
(plan §11), because the bridge holds nothing secret (sources carry no
credentials) and the cluster is the house's:

- **9093, the observations**: anyone can read what is playing and to how
  many. The connections are capped at 32, 8 per source address; the
  operator's `kubectl port-forward` arrives on the pod's loopback, which
  is outside the 32, but the kubelet's probes come from the node, not the
  pod's loopback, and count: something in the cluster holding all 32
  (four pods of 8, re-dialled) shuts the probes out, so the bridge turns
  unready (every stream plays "not available") and its liveness probe
  restarts it, and the controller's polls fail (Streams `Unknown`).
- **9092, AudioSocket**: there is no check of who connects, and a
  stream's UUID is public (`uuid5` of its name, also listed on 9093).
  Anyone can join a stream directly, or start a tuner stream and hold the
  pool's tuner; the bridge ends each such call 5 s past the time limit,
  but a client can dial again. Holding the bridge's 64 calls (a connection
  counts from its accept, before its UUID; 5 s to send one) refuses every
  phone's stream call.

If your CNI enforces NetworkPolicy, you can admit 9092 only from the PBX
(a host-network pod, so its connections come from its node's address: an
`ipBlock` of that address) and 9093 only from the controller and your
Prometheus. patchbay has not tested such a policy.

## The bridge image and its licences

`ghcr.io/christopherjmiller/patchbay-stream-bridge` is the patchbay binary
on Alpine 3.24 with Alpine's ffmpeg (the Dockerfile's `stream-bridge`
target), amd64 and arm64, published with the same tags as patchbay and run
by digest by a released chart.

- patchbay is Apache-2.0. It runs ffmpeg as a separate process and links
  nothing of it.
- ffmpeg, as Alpine builds it (`--enable-gpl --enable-version3`, never
  `--enable-nonfree`: CI refuses that build), is **GPL-3.0-or-later**
  (`ffmpeg -L` says so; Alpine's package metadata lists the source tree's
  `GPL-2.0-or-later AND LGPL-2.1-or-later`). Its libraries carry their own
  licences. The image's `org.opencontainers.image.licenses` is
  `Apache-2.0 AND GPL-3.0-or-later`.
- **Where the corresponding source is.** Every package in the image comes
  from Alpine's `v3.24` `main` and `community` repositories. Alpine
  publishes each package's build recipe in
  [aports](https://gitlab.alpinelinux.org/alpine/aports) (ffmpeg is
  `community/ffmpeg` on the `3.24-stable` branch) and mirrors the upstream
  source archives it builds from at `https://distfiles.alpinelinux.org/distfiles/v3.24/`.
  Each installed package records the aports commit it was built from (`c:`
  in the image's `/lib/apk/db/installed`):

  ```sh
  docker run --rm --network none --entrypoint /bin/sh ghcr.io/christopherjmiller/patchbay-stream-bridge:<tag> \
    -c "awk '/^P:ffmpeg\$/,/^\$/' /lib/apk/db/installed | grep -E '^(P|V|L|c):'"
  ```

  The image built for this release's e2e had `ffmpeg 8.1.2-r0`, aports
  commit `d2c3ca384892f415fc1b92abd87f78c1d5d0cbba`. The patch level
  floats within Alpine 3.24 between image builds; the published digest pins
  what runs, and the bridge reports `ffmpeg -version`
  (`bridge.ffmpeg` in its observations). Whoever redistributes the image is
  responsible for those sources staying available (GPL-3.0 §6).

## The house (luma-homeops)

What the house deploys, for the lead to copy into luma-homeops (never by
patchbay itself). The Streams are the house's, not the chart's.

The HDHomeRun FLEX DUO (HDFX-2US) is at `192.168.0.58`; its
`/discover.json` reported `TunerCount` 2 and firmware `20250623` on
2026-10-16. TV numbers are 7 + the major channel; SomaFM is `750`–`755`.
Each of the six SomaFM mounts below answered 206 `audio/mpeg` to a ranged
GET with ffmpeg's user agent on 2026-10-16.

**Helm values** of the patchbay Argo application
(`cluster/applications/telephony-release.yaml`), beside the existing ones:

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

`streamBridge.enabled: true` and `callLimit: 2h` are the chart's defaults,
written out so the house's choice is visible. A released chart already
runs the bridge image by digest.

**`cluster/telephony/streams.yaml`** (in the PBX namespace, `telephony`):

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

`TestHouseStreams` (internal/controller) reads this page: it creates every
Stream above in a real API server (envtest), so the CRD's rules accept
them, and compiles them with the values above (the four TV channels in
pool `hdhomerun`, the radio in none). The `streams` compile golden
(`internal/compile/testdata/golden/streams`) holds the TV channels and
the first two stations with the same settings: its `want/` shows the
dialplan and the `streams.json` they compile to, and `make asterisk-check`
boots it in the pinned Asterisk.

**No router change.** Asterisk reaches the bridge inside the cluster: the
PBX pod (host network, `ClusterFirstWithHostNet`) resolves
`patchbay-streams.telephony.svc` through the cluster DNS, as it does in
kind (bench B9 confirms it on Talos), and the bridge pod reaches the HDHomeRun
on the LAN and SomaFM on the internet as any pod does. If a NetworkPolicy
in the namespace limits egress, allow the bridge (`app.kubernetes.io/component:
stream-bridge`) TCP to `192.168.0.58:5004` and to the internet on 443.

**At the bench** (plan §8): time `705` on the Desk phone (B1, "about
3 s"); open `http://192.168.0.58/status.json` with two phones on `705`
(one tuner) and after both hang up (free within about 10 s, B3); with Plex
recording on both tuners, dial `705` and record the Stream's message here
(B4); note whether the 7945 negotiates G.722 on a stream call (B5); and
listen to `750`–`755` from the cluster (B7).
