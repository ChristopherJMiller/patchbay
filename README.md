# patchbay

A Kubernetes-native PBX for a house: Asterisk 22 (PJSIP) plus a Go operator.
You declare phones, softphones, people and audio streams as Kubernetes
resources; patchbay compiles the Asterisk and phone configuration, serves it to
the phones, and walks each device through a state machine to `Registered`
without hand edits.

```yaml
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Phone
metadata: {name: desk}
spec: {model: cisco-7945, mac: D0C78915E8C8, number: "100", label: Desk}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Softphone
metadata: {name: chris-laptop}
spec: {number: "150", label: "Chris (laptop)"}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Person            # dialling 301 rings every listed device
metadata: {name: chris}
spec: {number: "301", name: Chris, rings: [{phone: desk}, {softphone: chris-laptop}]}
---
apiVersion: patchbay.chrismiller.xyz/v1alpha1
kind: Stream            # dialling 705 plays live TV audio from an HDHomeRun
metadata: {name: king5}
spec: {number: "705", name: KING 5, source: "http://192.168.0.58:5004/auto/v5.1"}
```

Numbers are strings: quote them (`number: "100"`). A bare `number: 100` is
refused by the API server (design §4).

It is scoped to the hardware in one house (Cisco CP-7945G/7965G/7975G desk
phones on SIP firmware, Linphone softphones, an HDHomeRun tuner), LAN only,
with no outside line. The design, including the per-device state machines, is
[docs/design.md](docs/design.md).

## Status

**Pre-alpha.** Release A ("core calling") is built end to end: the
`v1alpha1` API, the compiler, the Cisco 79x5 and softphone drivers (rendering
and the state machines), the controller (it mints every Secret, compiles the
resources into the bundle, polls the PBX agent, decides each device's state
and writes status, sends resyncs, reports unknown phones), the PBX pod (the
agent applies, verifies and rolls back bundles, serves device HTTP and
one-time softphone provisioning; an unprivileged file server serves TFTP and
HTTP :6970; Asterisk loads only the compiled modules) and the CLI. The e2e
suite in kind runs all of it against real Asterisk with simulated phones: a
79x5 walking from SCCP firmware to `Registered`, a factory-reset one,
Linphone's one-time QR code, calls both ways, a person ringing two devices, a
blind transfer, a label change reaching the screen, a resync held during a
call. Release A (`v0.1.0`) runs on the house's cluster: the Cisco 7945 on
the desk registers, calls and transfers; the bench questions and their
answers are in [docs/plans/release-a.md §8](docs/plans/release-a.md).

Release B ("streams") is built: dialling a `Stream`'s number plays a live
source, a TV channel from a tuner or an internet radio station, through
`patchbay stream-bridge` (one ffmpeg per source, shared by everyone
listening, 16 kHz over Asterisk's AudioSocket). Tuner pools cap how many
channels of one tuner device play at once (the house lets phones hold one
of its HDHomeRun's two tuners; the DVR keeps the other), every stream call
ends at a time limit (2 h by default), and each Stream's status says
whether it is Idle, Playing, Busy, Failing or Pending, and why. e2e runs
it against a fake tuner and a fake Icecast station; the bench questions
for the real HDHomeRun and SomaFM are in
[docs/plans/release-b.md §8](docs/plans/release-b.md).
[docs/streams.md](docs/streams.md) is the guide.

On a Cisco desk phone, the Directories and Services buttons list the
extensions (phones, softphones, people) and the streams, and picking one
dials it; Linphone shows the same lists as its contacts
([docs/operations.md](docs/operations.md)).

Phone messages (0.4.0, the first part of release C) put text on a Cisco
desk phone's screen with a chime, from `patchbay phone message desk
"Dinner!"` or, for Home Assistant, an in-cluster endpoint: held during a
call, and in quiet hours or while nobody is home unless urgent, expired
after an hour, withdrawn by key ([docs/operations.md](docs/operations.md#messages)).

Planned releases: **A** core calling, **B** live audio streams, **C** phone
extras (messages, then wallpaper, ringtones, speed dials, more of the
Services menu).

## Quickstart

On a cluster of Kubernetes 1.33 or later, in a namespace that admits
host-network pods (`pod-security.kubernetes.io/enforce: privileged`), pick
the node the PBX lives on and its LAN address:

```sh
helm install patchbay charts/patchbay -n telephony --create-namespace \
  --set pbx.nodeName=top --set pbx.address=192.168.0.240
```

The settings phones get (time zone, NTP server, number plan, the networks
allowed to register) are chart values, rendered into
`patchbay-settings`. There is nothing to create by hand: the controller
mints the agent token, the AMI password and each device's password, and the
PBX pod starts once it has (the agent token) and has applied the first
bundle. Then declare the phones (`config/samples` has one of each):

```sh
kubectl -n telephony apply -f config/samples/
patchbay status -n telephony                     # the controller and config, every device, unknown phones
patchbay softphone qr chris-laptop -n telephony  # scan it in Linphone; it works once
patchbay firmware load cmterm-7945_7965-sip.9-4-2SR4-3.zip -n telephony
patchbay phone resync desk -n telephony
patchbay models                                  # what each model supports (no cluster needed)
```

The CLI uses `--kubeconfig`, else `$KUBECONFIG`, else `~/.kube/config`
(never the in-cluster config), and reaches the agent through a
port-forward. A phone's `status.humanAction` says, in words, the one thing
a person must do next (connect it, stage firmware, factory-reset it).
When something is wrong, [docs/operations.md](docs/operations.md) goes from
what you see (in `patchbay status`, `kubectl get phones`, the Events and
the metrics) to its cause and what to do.

Streams need nothing more by default: the chart runs the bridge
(`<fullname>-streams`) and the controller compiles the Streams into the
streams.json it plays. A tuner device is a **tuner pool** in the values,
which limits how many of its channels play at once; internet radio needs
none:

```sh
helm upgrade patchbay charts/patchbay -n telephony --reuse-values \
  --set 'streams.tunerPools[0].name=hdhomerun' \
  --set 'streams.tunerPools[0].hosts[0]=192.168.0.58' \
  --set 'streams.tunerPools[0].max=1'
```

A Stream's source is a direct stream URL (an HDHomeRun channel, an
Icecast mount), never a `.pls` or `.m3u` playlist:
[docs/streams.md](docs/streams.md) has the rules, the states, the house's
manifests and what to do when a stream will not play.

The PBX's spoken prompts (911's "This phone cannot call emergency
services" and "That number is not available") are rendered from text when
the image is built, in five voices: espeak-ng's, the default, and four
Piper voices trained on public domain recordings (`ljspeech`, `kristin`,
`norman`, `john`). Pick one with `--set sounds.voice=kristin`;
[docs/sounds.md](docs/sounds.md) has the voices and their licences.

## Development

Everything runs inside the pinned Nix dev shell, which provides Go, the
Kubernetes tooling, and the envtest control plane (no downloads at test time):

```sh
nix develop                    # or: nix develop -c <command>
make help                      # every target
make tools-versions            # what the shell pins
make generate                  # deepcopy + CRDs (after any api/ change)
make test                      # unit tests + envtest (real kube-apiserver + etcd)
make lint                      # golangci-lint (with exhaustive) + helm lint
make build                     # bin/patchbay
make sounds                    # the 911 and "not available" prompts, every voice (espeak-ng, Piper) into build/sounds
make image                     # ghcr.io/christopherjmiller/patchbay:dev (renders the prompts first)
make image-stream-bridge       # ghcr.io/christopherjmiller/patchbay-stream-bridge:dev (patchbay + Alpine's ffmpeg)
make image-check-stream-bridge # that image's user, ffmpeg licence, CA bundle and decoder flags (docker)
make asterisk-check            # every compile golden booted in the pinned Asterisk (docker)
make e2e                       # kind + the chart + the controller + real Asterisk + simulated phones and sources
make check-hygiene             # no binaries, firmware or secrets in git (CI runs it)
make vulncheck                 # govulncheck, with the Go that builds the image
```

The e2e suite, its knobs and how to debug it: [docs/e2e.md](docs/e2e.md).
CI, Renovate's automerge policy, image tags and releases:
[docs/releasing.md](docs/releasing.md).

## Layout

```
cmd/patchbay/          one binary: controller | agent | fileserver | stream-bridge | CLI
api/v1alpha1/          resource types -> generated CRDs
internal/compile/      pure: resources + settings -> config bundle
internal/drivers/      the driver contract, and the cisco79x5 and softphone drivers
internal/agent/        the PBX pod's sidecar (apply, device HTTP, AMI, observations) and file server
internal/streams/      the stream document (streams.json), its UUIDs and the audio format
internal/streambridge/ the stream bridge: AudioSocket server, sessions (ffmpeg), tuner pools, control API
internal/controller/   the controller: Setup, and its pieces (install, secrets, bundle, poller,
                       devices, numbers, discovery, metrics; bridgeclient and streamstate for Streams;
                       messages: the messages endpoint and the Message reconciler)
internal/cli/          the client commands (status, softphone qr, phone resync, phone message, phone
                       withdraw, phone test-push, messages, firmware load, models)
charts/patchbay/       Helm chart: CRDs, controller, PBX pod (Asterisk, agent sidecar, file server),
                       the stream bridge
test/e2e/              kind e2e suite; test/e2e/fakephone is the simulated 79x5 and Linphone,
                       and the fake stream sources (an HDHomeRun, an Icecast station)
test/image/            the stream bridge image checked (its user, ffmpeg's licence and flags)
test/chart/            the rendered chart checked against the Go layout contracts
hack/                  check-hygiene.sh, gen-sounds.sh
config/samples/        one example of each resource
docs/                  the design, the runbook (operations.md), and the research behind it
```

## Firmware

Cisco firmware is never committed to this repository, encrypted or not. It is
staged onto the PBX's firmware volume with `patchbay firmware load <zip>`,
checked against the SHA-256 manifest in the driver. See
[docs/research/](docs/research/).

## License

Apache License 2.0. See [LICENSE](LICENSE).
