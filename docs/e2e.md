# End-to-end tests (kind)

The e2e suite runs patchbay the way production does, as far as one laptop
can: a kind cluster, the real Helm chart, the real controller, the real PBX
pod (Asterisk 22 on the host network of a pinned node, the agent sidecar,
the file server), the CLI, and **fakephone**, a simulated Cisco 79x5 and
Linphone, as ordinary Pods. The real 7945 is only the final acceptance test
(design §6).

## The site

The harness installs the chart and then writes the **site resources**, as a
person would:

| Resource | Spec | Used by |
|---|---|---|
| Phone `desk` | cisco-7945, MAC `0200E2E00001`, 100, label `Desk` | `TestDeskLoop` only |
| Phone `kitchen` | cisco-7945, MAC `0200E2E00002`, 101, label `Kitchen` | the W2 tests (one-shot fakephone runs), and the third phone in `TestDeskLoop` |
| Softphone `laptop` | 150, label `Laptop` | `TestProvisioningOneTime`, `TestRollback`, `TestDeskLoop` |
| Person `chris` | 301, rings desk then laptop | `TestController`, `TestDeskLoop` |
| Stream `tuner-a` | 705, `http://fake-tuner.patchbay-e2e.svc:5004/auto/v5.1?hz=1100` (pool `tuner`) | `TestStreamBridgeUp`, `TestStreamTuner` |
| Stream `tuner-b` | 707, `…/auto/v7.1?hz=1300` (pool `tuner`) | `TestStreamBridgeUp`, `TestStreamTuner` |
| Stream `radio-a` | 750, `http://fake-radio.patchbay-e2e.svc:8000/radio-a?hz=500` (no pool) | `TestStreamBridgeUp`, `TestStreamBridgeDown`, `TestStreamRadio` |
| Stream `radio-b` | 751, `…/radio-b?hz=700` (no pool) | `TestStreamBridgeUp`, `TestStreamRadio` |

The **controller** does the rest, as in production: it mints the agent
token (the PBX pod waits for it in ContainerCreating), the AMI password and
a password per device (and laptop's provisioning token), compiles the site
into `patchbay-streams` (the stream bridge waits for it in
ContainerCreating), `patchbay-pbx-secrets` and then `patchbay-pbx-config`,
and the agent applies the bundle. The install's settings are the chart's
values with the kind node's InternalIP as `pbx.address` and the node's **pod CIDR**
(`10.244.0.0/24`) as the only `allowedNetworks` entry: every fakephone
registers from the pod network, and the compiled ACL refuses everything
else. Tests read the compiled bundle back from the cluster
(`patchbay-pbx-config` with the `patchbay-pbx-secrets` of the same version),
never from a copy of their own.

One Secret is not the controller's: the messages endpoint's bearer token,
`patchbay-messages-token` (key `token`), which the harness creates before
the chart, as an operator would (`messages.enabled` in
`test/e2e/values.yaml` names it). Tools pods read it from the Secret; the
harness never holds it.

A test that needs more devices creates its own (`lifecycle-<hex>` 111,
`reset-<hex>` 112, `discovered` 113) or Streams (`added` 759), deleting
any a previous run left first, and deletes them at the end. `TestPhoneLifecycle`'s names are random
per run: the agent keeps a device's fetch log by its ID after the device
leaves the bundle, so a Phone recreated under a fixed name on a reused
cluster would start with the last run's fetches (plan §11). A test that
creates a device waits until the compiled bundle holds it before it waits
for that bundle to be applied.

## Two kinds of fakephone pod

- **One-shot** (`fakephone register|call|tftp|http|ami|proc|read`): a pod
  that does one exchange and exits; the test reads its exit code and log.
  It runs as a compiled device (`FAKEPHONE_USER` the number, `..._AUTH_USER`
  the device ID, the password from the device's Secret). The W2 tests
  and the extra callers of `TestDeskLoop` are these, as kitchen.
- **Scripted** (`fakephone run`): a long-lived phone executing a
  `script.Script` (`test/e2e/fakephone/script`): in cisco mode it TFTPs
  `SEP<MAC>.cnf.xml`, parses it and registers over its transport with its
  credentials and expiry, serves the three web pages the agent scrapes
  (from a scripted load and log, or 503 while "flashing"), fetches firmware
  files with retries, answers service-control NOTIFYs by refetching (and
  re-registering when its credentials changed); in linphone mode it GETs the
  one-time provisioning URL and registers over UDP. Both answer calls
  (ringing, a tone of their own, counting the tones they hear, recording
  the caller's From, Remote-Party-ID and P-Asserted-Identity), place calls
  and blind-transfer them (an in-dialog REFER). The script travels in a
  Secret; the pod binds port 80 as non-root through the safe sysctl
  `net.ipv4.ip_unprivileged_port_start=0`. The harness drives it with
  **signals** (`POST /fakephone/signal/<name>` releases a `waitSignal` step)
  and reads its **State** (`GET /fakephone/state`: every fetch,
  registration, NOTIFY and call), both through the API server's
  `pods/proxy`, so nothing in the image is needed for it.

**The fake stream sources** (`fakephone source`, the Deployment
`fake-sources` of the tools image, behind the Services `fake-tuner` :5004
and `fake-radio` :8000) stand in for the house's HDHomeRun and SomaFM, so no
test leaves the cluster. Both stream a WAV tone (48 kHz stereo, the
frequency from `?hz=`) at real time after a 4 s burst (ffmpeg's probe reads
half a megabyte of a PCM WAV before it decodes any; a real tuner's MPEG-TS
or an Icecast mount fills it at once). The tuner has two tuners, each open
stream holding one, answers 503 with `X-HDHomeRun-Error: 805 All Tuners In
Use` when none is free, and sends its first audio 1.5 s after the request
(a tuner locking); `POST /fake/occupy?tuners=n` makes it hold n tuners as
the DVR would. `GET /fake/status` on either port is its `SourceStatus`
(open and opened streams by path, refusals), which the tests read through
`services/proxy`. The test/e2e/values.yaml settings put `fake-tuner` in the
pool `tuner` (max 1) and the stream calls' time limit at 20 s.

Device status is followed with a **watch** on the Phone or Softphone
(`tracker`): a test waits for a condition on the latest status and can ask
for every state the device went through.

## The tests

Go runs the tests without `t.Parallel()` first, in file order, then the
rest together.

**Sequential**

| Test | Proves |
|---|---|
| `TestCRDs` | The chart installs the CRDs; `config/samples` are accepted and read back typed; a CEL rule is enforced by the API server. |
| `TestPBXUp` | The PBX pod's shape: on the host network of the pinned node, the `asterisk-modules` init container completed, the agent a native sidecar, `asterisk` and `fileserver` running, nothing restarted. **Identities, from `/proc`** (what the kernel enforces, not the spec): Asterisk is uid/gid 1000 with no capabilities, `no_new_privs` and a read-only root; the agent is 65532 with no capabilities (group 1000, the pod's fsGroup); the file server is 65533:65533 with **no groups**, every capability set empty except the bounding set (see below) and `no_new_privs`. The file server's identity can read `current/tftp/` and **cannot read** `manager.conf`, `pjsip.conf`, a softphone's provisioning document or `bundle.json`. Asterisk runs the compiled config: transports on the node IP, the site's endpoints, every `compile.Modules()` module Running, AMI on loopback, HTTP off, the echo extension. The agent answers through its Service from the pod network (readyz; 401 without the token; the observations), and over a port-forward reports AMI up and the controller's latest version applied without error. |
| `TestController` | Under the chart's least-privilege Role the controller holds its install's Lease; it minted the agent token, the AMI password and every device's Secret, with its labels, the device Secrets owned by their resource (`blockOwnerDeletion`), the install's unowned; `pbx-secrets` and `pbx-config` carry one version, `pbx-secrets` exactly the keys the bundle references with the values it was compiled with, and the agent applied it; Person chris is Ready (Routed). |
| `TestFirmwareUploadAndServe` | The e2e load (`E2E45.0-0-1S`, built only with `-tags e2e`) goes up through the agent over a port-forward with the token: a corrupted file **422** (nothing stored), the good one **201**, again **200** (a reused cluster's persistent volume keeps the store, so its files answer 200 at once); the store sensor reports it present, and the file server serves it over TFTP and HTTP :6970. |
| `TestProvisioningOneTime` | Softphone laptop's provisioning URL, as its status shows it (the compiled token), serves its document once over the pod network (200), then 404; the agent container is killed, and after it restarts the URL is still 404 (the spend is on the persistent volume) and the served fetch, with the softphone's version, is back in the observations. The controller is scaled to 0 meanwhile, so the agent restarts while the token is still granted (once a rotated version settles the agent rightly forgets the spend); scaled back, it sees the fetch and replaces the spent token (`lastAction` rotateToken, Provisioned) and publishes no URL (only an Issued softphone shows one). A laptop with no URL (a reused cluster's) is reissued first. |
| `TestRollback` | §9's rollback, the agent half (the controller half is the envtest `TestApplyFailedControllerHalf`). The controller is scaled to 0; a bundle whose `pjsip.conf` renames kitchen's endpoint is written as the controller writes (`install.ProjectedSecret`, then `install.ConfigMap`, with the version `Bundle.VersionWith` gives it): the agent applies it, fails verification (after its 30 s), swaps the previous version back and reloads; it reports `lastApplyVersion` and the verify error while `appliedVersion` stays the good one, and the old config still registers and echoes. Scaled back to 1, the controller writes its own version again (pbx-secrets first, with the same values: the intake keys an attempt by the values, so the refused version is not applied again and the reissue applies at once), and `patchbay softphone qr laptop --reissue` makes a new version that the agent applies cleanly, so a reused cluster starts without this test's error. No `ApplyFailed` Event was emitted and no device shows it: the broken version was never compiled. |
| `TestPBXDown` | Design §7's "Asterisk down": the asterisk container is stopped (crictl) each time it comes back until kitchen shows `Registered=False PBXDown`, and it never shows `Ready=Unknown AgentUnreachable` meanwhile. The PBX pod is NotReady while Asterisk is down, so this holds only because the agent Service publishes not-ready addresses. Asterisk then comes back after the kubelet's restart backoff, the agent's AMI session is up again and PBXDown clears (registrations survive: astdb is on an emptyDir). |
| `TestStreamKit` | The stream kit against itself, with no cluster: the site Streams' sources pass `streams.CheckSource`, the tuner ones are on the e2e pool's host and the radio ones on none, every tone is its own; the e2e values run the bridge image `:e2e`, never pulled, with a 20 s limit; a call's command line and media line read as fakephone writes them. |
| `TestStreamBridgeUp` | The bridge as the chart runs it: one replica, `Recreate`, Ready, no service account token or Secret volume, no restarts; PID 1 is `patchbay` as 65532:65532 with every capability set empty, `no_new_privs` and a read-only root (from `/proc`); its observations report the `ffmpeg -version` line the image runs and the `streams.json` version the controller compiled (within 120 s), which holds every site Stream as written and the pool `tuner` (1); every site Stream is Idle (Ready True), `tunerPool` `tuner` on the tuner ones, empty on the radios. |
| `TestStreamBridgeDown` | With `patchbay-streams` scaled to 0, a call to 750 is still answered, plays "not available" (≥ 2 s of audio, ≥ 1 s not the SIT tone) and the PBX hangs up (Asterisk's `Dial` finds no bridge: the Service has no endpoints, the connection is refused); radio-a is Unknown (`BridgeUnreachable`) within 15 s. Scaled back to 1, radio-a is Idle again with the same `lastPlayed` and `lastFailure`. |

**Parallel**

| Test | Proves |
|---|---|
| `TestPhoneLifecycle` | §9 "Desk walks to Registered", both paths, with the e2e load wiped from the volume first. **lifecycle** (firmware `E2E45.0-0-1S`, its web page on SCCP): Unseen (P2), then NeedsFirmware (FirmwareAvailable False NotStaged, FirmwareCurrent False OtherLoad, P3 naming both files). **firmware-load**: `patchbay firmware load` of a zip (a top-level folder, a readme) whose only firmware file is corrupted exits non-zero naming it and stores nothing; the good zip stores both (`stored (201)` or `already stored (200)`), and FirmwareAvailable becomes True. The phone downloads the load: Upgrading (P7, `upgradeStartedAt` set); after the 3 min window and a config fetch on the new load, Registering (no human action); registered: Registered with Ready, FirmwareAvailable, FirmwareCurrent (Running), Provisioned, Registered and ConfigCurrent all True. **reset** (the Desk phone's own path, started first): it asks only for its `.sgn`, which does not exist. The agent never reads its web page (it scrapes only a phone it served or that registered), so it is the 3 min rule that makes it NeedsTrustReset, with **P6** (P5 needs the default load, Cisco's, staged, and e2e never has Cisco's files; the driver's `TestLifecycle` pins P5). After the factory reset (web page down, `term45.default.loads`, its config, the staged load downloaded, its config again on the new load, registered): Upgrading, never NeedsTrustReset again, then Registered with FirmwareCurrent Unknown (NotReported), no `lastAction`, no NOTIFY. |
| `TestDeskLoop` | The house's loop, desk (79x5 over TCP) and laptop (Linphone over UDP) each started once with its whole script. **softphone**: `patchbay softphone qr laptop --reissue` prints a URL; laptop is Issued with it (S1); Linphone fetches it (`misc/config-uri` cleared), downloads the contacts it names (desk and laptop among them, each `sip:<number>@<node>`) and registers; the controller replaces the spent token (Provisioned, `lastAction` rotateToken) and publishes no URL; Registered, still no URL; the same URL again is 404 and laptop stays Registered. **provision**: desk fetches its SEP file and registers; Registered, ConfigCurrent True, its screen label `Desk`. **push: test-push reaches desk's screen**: `patchbay phone test-push desk --text e2e --beep` prints status 0; desk asked its authenticationURL (AUTHORIZED), took a push of `Play:patchbay-chime.raw` and the message page, and fetched a page whose text holds `e2e`; `--clear` gives a push of `Init:Services`. **calls**: 100 calls 150 and 150 calls 100, each held 4 s; each side counts at least 50 packets of the other's tone; laptop sees `Desk` (From or P-Asserted-Identity), desk sees `Laptop` (Remote-Party-ID). **person**: kitchen calls 301; both phones get the INVITE from 101, desk answers (after 1 s), laptop's is cancelled (487), kitchen hears desk's tone. **transfer**: kitchen calls 100, desk answers and REFERs it to 150 (202), and laptop answers a call carrying at least 50 packets of kitchen's tone. **label**: desk relabelled `Desk Two`: a new version, one resync NOTIFY, desk refetches (`DeskTwo` on its screen: the 79x5 drops spaces), Registered on the new version. **messages** (in three steps, the second inside change-during-call's open call): `patchbay phone message desk dinner`: desk takes an authorized push (Status 0) of `Play:patchbay-chime.raw` and its message page, fetches a page whose text holds `dinner`, and the Message is Shown; during the call, a second message is Held `InCall`, desk gets no push for the 20 s, and after the BYE a push with the chime brings a page holding both, the second Shown; then a tools pod PUTs `{"quiet":true}` to `patchbay-messages:9441/v1/house` with the bearer token from `patchbay-messages-token` (which the harness creates before the install) and GETs it back, a third message stays Held `QuietHours` while an `--urgent` one with `--key k` is pushed with the chime (the held one not on its page) and Shown; `patchbay phone withdraw desk --key k` pushes the page again without it and without the chime, and it is Withdrawn. **change-during-call** (Review Focus 5): desk on an open call to 600, relabelled `Desk Three`: ConfigStale, ConfigCurrent False Stale "waiting for the call to end", no NOTIFY for 20 s; after the BYE exactly one, then Registered. **secret-heals**: desk's Secret deleted: a new one (new UID, new password), a new version, one NOTIFY, desk registers with the new password, and the old one gets 401. |
| `TestDiscovery` | §9's second phone: an unknown MAC (random, `0200E2E1xxxx`) asks for its SEP file; an `UnknownPhone` Event on `patchbay-pbx-config` says `unknown phone <MAC> requested config from <pod IP>`, and `patchbay status` lists the MAC from that address. A Phone with that MAC is then created; a scripted phone fetches its config and registers: Registered, FirmwareCurrent Unknown. |
| `TestRegister` | Kitchen registers over **TCP**, challenged first; Asterisk holds the pod's contact and User-Agent, and the agent reports the registration with its **Call-ID**. A wrong password ends on **401**, and so does a contact outside the allowed networks (the endpoint's contact ACL: Asterisk 22 re-challenges, it does not answer 403). Only a **source** outside them meets the global ACL, which answers **403** without a challenge. (A Softphone over UDP, whose contact Asterisk rewrites, is `TestDeskLoop`'s laptop.) |
| `TestEchoCall` | Calling `600` crosses pod network → node IP for SIP and RTP both ways, and a 1 kHz PCMU tone comes back from the node's own address. |
| `TestModulesNarrowed` | Asterisk's module directory, and `module show`, hold exactly `compile.Modules()`. Over AMI, as the agent's own account (from a host-network pod: AMI is on the node's loopback), a `ModuleLoad` reload works, and `ModuleLoad` of `app_system.so`, `func_shell.so`, `res_agi.so`, `res_ari.so` and `../usr/lib/asterisk/modules/app_system.so` all fail. |
| `TestTFTPAndHTTP6970` | The file server serves kitchen's `SEP<MAC>.cnf.xml` with its password resolved, byte for byte the same over TFTP and HTTP :6970; the `.sgn` and an unknown MAC's SEP file are not found. Every request is in `/v1/observations`: attributed to `phone-kitchen` (served ones with its config version) or, for the unknown MAC, in `unknownDevices` with the requesting pod's address. |
| `TestPrompts` | `911`, the reserved `0`, `999` (`9.`) and `205` (`2XX`), and the unassigned `199` are answered with at least 2 s of non-silent audio, at least 1 s of it not the SIT tone: the prompts the agent installed from the image play. |
| `TestResyncNotify` | `POST /v1/actions/resync` makes the agent send a registered kitchen a service-control NOTIFY whose body is the plan's, quoting its REGISTER's Call-ID; fakephone then refetches its SEP file over TFTP. |
| `TestStreamRadio` | **Two stations at once** (no pool, no limit): calls to 750 and 751 (10 s each) hear 500 Hz and 700 Hz, each first within 5 s of the first packet (the time is logged) and in ≥ 200 packets; while both are up the bridge has both with a listener in one document, and radio-a and radio-b are Playing with 1 listener; the fake radio opened each once, and closed both within 10 s of the hang-ups; both Idle within 15 s. **The time limit**: a 40 s call to 751 is ended by the PBX at 20 s ± 2 s (the e2e `streams.callLimit`), having heard 700 Hz in ≥ 800 packets. |
| `TestStreamTuner` | The tuner policy against the fake HDHomeRun (pool `tuner`, max 1). **shared**: two concurrent 12 s calls to 705 both hear 1100 Hz (first within 5 s, the 1.5 s lock included); while both are up the pool has one session with 2 listeners, the fake tuner exactly one open stream, and tuner-a is Playing with 2 listeners; the tuner opened `/auto/v5.1` once, is free within 10 s of the hang-ups, and the pool has no session; tuner-a Idle. **limit**: while a 16 s call to 705 plays, tuner-b is Busy (`TunerBusy`, naming `tuner-a (705)`); a call to 707 plays "not available" and the PBX hangs up; tuner-b's `lastFailure` is `TunerBusy` naming tuner-a, with a `CallRefused` Event saying so; the fake tuner never opened `/auto/v7.1`; once 705 ends tuner-b is Idle with the refusal kept. **refused**: with the fake DVR holding both tuners, a call to 705 plays "not available"; tuner-a is Failing (`SourceFailed`, the message giving ffmpeg's "5XX"), with a `SourceFailed` Event; the fake tuner's refusals grew. Freed, a call to 705 hears 1100 Hz and tuner-a is Idle with `lastPlayed` after `lastFailure`. |
| `TestStreamAdded` | A Stream `added` (759, on the fake radio) created on the running install is Pending (`BridgePending`) first, then Idle within 150 s (the kubelet's sync of the bridge's `patchbay-streams` mount, logged), and a call hears its 600 Hz within 5 s. Deleted, it leaves `patchbay-streams` and the compiled dialplan; once the agent has applied that bundle a call to 759 plays "not available". |

`TestRegister` and `TestResyncNotify` take turns (`kitchenContact`):
kitchen's AOR keeps one contact (`remove_existing`), so one's REGISTER would
take the other's NOTIFY. Calls as kitchen need no turn.

## Running it

Everything runs inside the flake's shell.

```sh
nix develop -c make e2e          # build images, fresh cluster, test, delete cluster
PATCHBAY_E2E_KEEP=1  nix develop -c make e2e   # ...and keep the cluster afterwards
PATCHBAY_E2E_REUSE=1 nix develop -c make e2e   # reuse a kept cluster
nix develop -c make e2e-up       # cluster + chart, no tests, kept (REUSE+KEEP)
nix develop -c make e2e-down     # delete the cluster
nix develop -c make e2e-images   # just build/pull the four images and build .e2e/bin/patchbay
```

- Without `PATCHBAY_E2E_REUSE=1` an existing `patchbay-e2e` cluster is
  deleted first, so a default run always starts from scratch.
- Without `PATCHBAY_E2E_KEEP=1` the cluster is deleted at the end, pass or
  fail, and also when the run is cut short:
  - **Ctrl-C or SIGTERM**: the first signal cancels the running step and
    teardown still deletes the cluster (give it a few seconds); a second
    signal kills at once;
  - **the time limit**: the suite gives itself `-timeout` (`E2E_TIMEOUT`,
    30m) less a margin (3 min at 30m), so it stops and tears down before
    go test's own alarm, which panics without any teardown.

  Only a hard kill (SIGKILL, a second Ctrl-C) leaves the cluster behind;
  `make e2e-down` removes it, and the next default run deletes it anyway.
  With `PATCHBAY_E2E_KEEP=1` an interrupted run keeps a possibly half-built
  cluster, so run once without `PATCHBAY_E2E_REUSE=1` afterwards.
- A fresh run takes about twelve minutes once the images are built (697 s
  measured for release B): about a minute to create the cluster, a minute
  to load the four images, 15 s to install and roll out, two and a half
  minutes of sequential tests (TestRollback alone waits 30 s, twice, for
  verification to give up; the two sequential stream tests about 15-35
  s), then six and a half minutes for the parallel group, which
  TestPhoneLifecycle sets: the Desk phone's path waits out the driver's
  3 min `.sgn` rule and then its 3 min download window. TestDeskLoop, the
  stream tests and the rest finish inside it.
- Only one kind cluster, `patchbay-e2e`, is ever created.

### Kubeconfig isolation

The suite never reads or writes `~/.kube/config`:

- the kubeconfig is **`.e2e/kubeconfig`** (gitignored), and every `kind`,
  `kubectl` and `helm` call passes `--kubeconfig` explicitly;
- so does every run of the CLI under test (`.e2e/bin/patchbay --kubeconfig
  .e2e/kubeconfig ... -n patchbay-e2e`), which never falls back to another
  config when one is named;
- the Makefile and the harness also set `KUBECONFIG` to it, so a tool that
  ignores the flag still cannot fall back to your real config;
- they set `KUBECACHEDIR=.e2e/kube-cache` too, so kubectl and helm keep
  their discovery and HTTP caches there instead of adding a directory per
  run to the shared `~/.kube/cache`.

Use the same file by hand:

```sh
export KUBECONFIG=$PWD/.e2e/kubeconfig      # inside the repo, in nix develop
kubectl -n patchbay-e2e get pods -o wide
```

### Images

| Image | Built from | How it gets into kind |
|---|---|---|
| `ghcr.io/christopherjmiller/patchbay:e2e` | `Dockerfile` with **`GO_TAGS=e2e`** (`make image`, after `make sounds`) | `kind load docker-image`, `pullPolicy: Never` |
| `ghcr.io/christopherjmiller/patchbay-stream-bridge:e2e` | the `Dockerfile`'s `stream-bridge` target (`make image-stream-bridge`, with `GO_TAGS=e2e` only to share the build stage) | same |
| `ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e` | `test/e2e/fakephone/Dockerfile` | same |
| `andrius/asterisk:<tag>` | the chart's `pbx.asterisk.image`, **pulled by digest** on the host and tagged | same |
| `.e2e/bin/patchbay` (not an image) | `go build -tags e2e ./cmd/patchbay` | run on the host, against the cluster's API server only |

The patchbay image and the CLI the suite runs are built with the `e2e`
build tag: only then does the firmware manifest hold the e2e test load
(docs/plans/release-a.md §2(b)). No other image or binary may be built
with it. `make image` renders the sound prompts
first (`make sounds`: every voice, with espeak-ng, Piper and ffmpeg from
the flake, into `build/sounds/<voice>/`, which `.dockerignore` admits and
`.gitignore` keeps out of git); the Dockerfile copies them to
`/usr/share/patchbay/sounds/`. The suite runs the default voice, espeak
(docs/sounds.md).

`kind load` cannot carry a registry digest into the node's containerd (it
re-imports the image under a new manifest digest), so `test/e2e/values.yaml`
drops the digest and sets `pullPolicy: Never`. The content is still the pinned
one: `make e2e-images` pulls `repository:tag@digest` and tags that exact
image. The harness annotates the pods with each image's ID, so a rebuilt
`:e2e` image rolls the pods on a reused cluster.

### What the harness does

`test/e2e` is behind the `e2e` build tag and uses
[e2e-framework](https://github.com/kubernetes-sigs/e2e-framework) for its
environment and features. Its own kind provider is not used, because it runs
`kind create/delete cluster` without `--kubeconfig` and writes kubeconfigs to
`$TMPDIR`; the harness drives `kind` itself.

1. Create the cluster from `test/e2e/kind.yaml`, or reuse it. It is one
   node running **Kubernetes 1.33** (kind v0.32.0's v1.33.12 image, pinned
   by digest): the minor of the cluster patchbay is built for and the
   chart's `kubeVersion` floor. envtest covers the newest end (1.37). The
   whole create is bounded at 10 minutes.
2. Set the node's `net.ipv4.ip_unprivileged_port_start` back to 1024 (see
   below).
3. Load the four images.
4. Create the namespace (labelled `pod-security.kubernetes.io/enforce:
   privileged`, as a real install needs) and delete any fakephone pods and
   phone scripts a reused cluster still holds.
5. `helm upgrade --install patchbay charts/patchbay` with
   `test/e2e/values.yaml` plus `.e2e/values-runtime.yaml` (the node's name
   and InternalIP as `pbx.nodeName`/`pbx.address`, its pod CIDR as
   `allowedNetworks`, and the image IDs); wait until the API server serves
   the CRDs; write the site resources (on a reused cluster, put their specs
   back) and the fake stream sources (`fake-sources`, `fake-tuner`,
   `fake-radio`).
6. Wait for the controller, the PBX and the stream bridge to roll out, the
   way `kubectl rollout status` does, polling for up to 5 minutes each (the
   PBX pod waits for the agent token the controller mints, then for the
   first bundle; the bridge for the `patchbay-streams` it compiles), then
   for the fake sources.
7. Read the settings the chart rendered and the minted agent token, and
   wait until the agent has applied the controller's bundle.

The tests that talk to the agent's control API many times (observations,
the firmware upload, the resync action) go through a **port-forward** to the
PBX pod from the harness (`pods/portforward`, as the CLI does), with the
agent token from its Secret; `TestPBXUp` also checks the Service path from
the pod network that the controller uses. The CLI the tests run is
`.e2e/bin/patchbay`, built with `-tags e2e` by `make e2e-images` (so
`firmware load` knows the e2e load), always with `--kubeconfig
.e2e/kubeconfig -n patchbay-e2e`.

One-shot pods say which outcome they expect: a pod that ends as expected is
deleted, and any other is kept for the diagnostics. Scripted phones are
deleted when their test ends.

## The PBX pod

```
asterisk-modules  init      Asterisk image, uid 1000: copies compile.Modules() into an emptyDir
agent             sidecar   65532, no capabilities: applies bundles, device HTTP :8088, AMI, observations, control API :9440
asterisk                    1000, no capabilities: SIP :5060, RTP 10000-10099, on the agent's live config
fileserver                  starts as 0 with NET_BIND_SERVICE, SETUID, SETGID; serves as 65533 with nothing: TFTP :69, HTTP :6970
```

### Privileged ports: the file server

Docker sets `net.ipv4.ip_unprivileged_port_start=0` in every container,
kind's node included, so in plain kind any process on the host network may
bind port 69. Talos keeps 1024. The harness restores 1024, so kind behaves
like the real node. Measured with a host-network pod binding UDP 69 on the
node:

- non-root, no capabilities: `permission denied`;
- non-root with `capabilities.add: [NET_BIND_SERVICE]`: **still denied**
  (Kubernetes grants no ambient capabilities to non-root containers);
- uid 0 with every capability dropped except `NET_BIND_SERVICE`: works.

A hostNetwork pod cannot use a user namespace (`hostUsers: false`), so uid
0 there is uid 0 on the node. The decision (docs/plans/release-a.md
§5.2.1): **a third container, `patchbay fileserver`**, the same image,
which starts as uid 0 with only `NET_BIND_SERVICE`, `SETUID` and `SETGID`
(`runAsUser: 0`, `runAsNonRoot: false`, `allowPrivilegeEscalation: false`,
read-only root), binds TFTP and HTTP :6970 **on the PBX address** (not
0.0.0.0: replies must come from the address phones asked), then
`setgroups([])`, `setresgid`, `setresuid` to **65533:65533**, and checks
`/proc/self/status` (all four uids and gids, no groups, `CapInh`, `CapPrm`,
`CapEff` and `CapAmb` zero, `NoNewPrivs: 1`) before it opens any file; if
anything differs it exits non-zero. It holds no Kubernetes credentials,
no control API token and no AMI password, and the live volume's modes
(`internal/agent/layout`) keep its uid, which has no groups, to the
world-readable `tftp/` tree and the firmware store. It reports every
transfer to the agent over a unix socket on a shared emptyDir.

`TestPBXUp` reads all of this back from `/proc`. The file server's
**bounding set** still holds the three capabilities it started with
(`CapBnd 00000000000004c0`): dropping them from the bounding set needs
`CAP_SETPCAP`, which it never had. With `no_new_privs` and a static
distroless image (no setuid or file-capability binary to exec) they can
never come back into its effective set.

### The narrowed module directory

Asterisk's loader joins a module name to `astmoddir` without refusing
`..`, so a separate, narrow `astmoddir` would not keep the image's other
modules out of reach: `module load ../usr/lib/asterisk/modules/app_system.so`
loads through any of them. The `asterisk-modules` init container (the
Asterisk image, uid 1000) copies exactly `compile.Modules()` into an
emptyDir, and the Asterisk container mounts it **read-only over**
`/usr/lib/asterisk/modules`, hiding the image's directory. `astmoddir` is
unchanged. `test/chart` keeps the chart's list equal to
`compile.Modules()`; `TestModulesNarrowed` shows that even the agent's AMI
account, which may `ModuleLoad`, cannot load `app_system`, `func_shell`,
`res_agi` or `res_ari`, by name or by a relative path. The init container
never mounts the live config, so it never sees a secret.

### Asterisk's console needs `-C`

The remote console (`asterisk -rx`) reads `asterisk.conf` to find the
control socket. The image's own `/etc/asterisk/asterisk.conf` happens to
name the same directory, but the chart's probes, the NOTES and this suite
all pass `-C /var/lib/patchbay/config/current/asterisk/asterisk.conf`, the
file Asterisk itself runs on.

## Debugging a failure

1. **Read the diagnostics.** After a failed test (or a setup that never
   rolled out) the harness prints, and saves under `.e2e/diagnostics/`, the
   namespace's objects and events, `kubectl describe pods`, and every
   container's logs (the agent's, the file server's, the bridge's, the fake
   sources' and the fakephone pods' included) and the bridge's
   observations.
2. **Keep the cluster** and look around:

   ```sh
   PATCHBAY_E2E_KEEP=1 nix develop -c make e2e
   export KUBECONFIG=$PWD/.e2e/kubeconfig
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c agent
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c fileserver
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c asterisk
   kubectl -n patchbay-e2e logs deploy/patchbay-controller          # its decisions, the poller, Events sent
   kubectl -n patchbay-e2e logs deploy/patchbay-streams             # the bridge: each call, each session start/stop/failure
   kubectl -n patchbay-e2e logs deploy/fake-sources                 # the fake tuner and radio
   kubectl -n patchbay-e2e get streams -o wide                      # state, listeners, pool, source
   kubectl get --raw /api/v1/namespaces/patchbay-e2e/services/http:patchbay-streams:9093/proxy/v1/observations | jq
   kubectl get --raw /api/v1/namespaces/patchbay-e2e/services/http:fake-tuner:5004/proxy/fake/status | jq
   kubectl -n patchbay-e2e get phones,softphones,people -o wide     # state at a glance
   kubectl -n patchbay-e2e get events.events.k8s.io                 # StateChanged, ActionSent, UnknownPhone, ...
   .e2e/bin/patchbay --kubeconfig .e2e/kubeconfig -n patchbay-e2e status
   kubectl -n patchbay-e2e get configmap patchbay-pbx-config -o yaml   # the compiled bundle, file by file
   ```

3. **Ask Asterisk.** Its CLI works through `kubectl exec`:

   ```sh
   ax() { kubectl -n patchbay-e2e exec deploy/patchbay-pbx -c asterisk -- \
     asterisk -C /var/lib/patchbay/config/current/asterisk/asterisk.conf -rx "$*"; }
   ax pjsip show endpoints
   ax pjsip show contacts            # truncates long URIs...
   ax database show registrar        # ...astdb holds each contact in full (uri, user_agent, via)
   ax pjsip show transports
   ax module show
   ax dialplan show patchbay-devices
   ax core show channels
   ax pjsip set logger on            # SIP traces into the asterisk container log
   ax rtp set debug on               # every RTP packet into the log (noisy)
   ```

4. **Ask the agent** over a port-forward:

   ```sh
   kubectl -n patchbay-e2e port-forward deploy/patchbay-pbx 9440 &
   TOKEN=$(kubectl -n patchbay-e2e get secret patchbay-agent-token -o jsonpath='{.data.token}' | base64 -d)
   curl -s -H "Authorization: Bearer $TOKEN" localhost:9440/v1/observations | jq .agent
   curl -s localhost:9440/metrics | grep patchbay_agent
   ```

5. **Run fakephone by hand**, with SIP traces (`-v`), as Phone kitchen:

   ```sh
   NODE_IP=$(kubectl get node patchbay-e2e-control-plane \
     -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
   PW=$(kubectl -n patchbay-e2e get secret patchbay-phone-kitchen -o jsonpath='{.data.sip-password}' | base64 -d)
   kubectl -n patchbay-e2e run fp --rm -i --restart=Never \
     --image ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e --image-pull-policy=Never \
     -- call 600 --server "$NODE_IP:5060" --user 101 --auth-user phone-kitchen --password "$PW" -v
   ```

   Subcommands: `register` (with `--wait-notify`), `call <number>`, `tftp
   get <file>`, `http get|send`, `ami`, `proc`, `read`, and `run` (a
   scripted phone: `--script FILE` or `$FAKEPHONE_SCRIPT`, the JSON of a
   `script.Script`). `fakephone --help` documents the flags (most also read
   `FAKEPHONE_*`). A running scripted phone answers `GET
   /fakephone/state`: `kubectl get --raw
   /api/v1/namespaces/patchbay-e2e/pods/<pod>:80/proxy/fakephone/state | jq`.
6. **Iterate fast**: keep the cluster, change code or the chart, and rerun
   with `PATCHBAY_E2E_REUSE=1 PATCHBAY_E2E_KEEP=1`. Images are rebuilt and
   reloaded, and the pods roll when an image changed.
7. Run one test: `KUBECONFIG=$PWD/.e2e/kubeconfig PATCHBAY_E2E_REUSE=1
   PATCHBAY_E2E_KEEP=1 go test -tags e2e -count=1 -v -run TestEchoCall
   ./test/e2e/` (after `make e2e-images`).

## What kind proves, and what it hides

Findings from building this suite.

- **Docker here is rootless.** Container IPs live inside rootlesskit's
  network namespace and cannot be reached from the host. kind works (the API
  server is published on 127.0.0.1), but nothing on the host can talk SIP,
  RTP or TFTP to the cluster. So fakephone always runs as a Pod, never on
  the host; the harness reaches the agent through the API server
  (port-forward).
- **Pod network ↔ host network works both ways** in kind (kindnet): SIP over
  TCP and UDP to the node IP, RTP in both directions, TFTP from a pod to the
  node IP with replies from the address asked, and pod → ClusterIP Service
  → hostNetwork pod for the agent's control API.
- **RTP source address.** Asterisk binds RTP to 0.0.0.0 by default, so
  replies leave from whatever source the route picks; in kind that was the
  pod bridge's `10.244.0.1`, not the node IP advertised in SDP. The
  compiler sets `media_address` and `bind_rtp_to_media_address = yes`, and
  the echo test asserts that RTP comes back from the node's IP. The file
  server binds the PBX address for the same reason.
- **The ACLs answer differently.** On Asterisk 22 an endpoint's own contact
  ACL refusal ends on **401** (it re-challenges, the client gives up); only
  the global ACL (`[patchbay-acl]`, `type = acl`) answers **403**, without
  a challenge. `TestRegister` pins both.
- **AMI is reachable through a port-forward.** AMI binds the node's
  loopback, and `pods/portforward` on the PBX pod (a hostNetwork pod)
  connects to the node's loopback: whoever may port-forward to the PBX pod
  reaches AMI. Its password, held only by the agent (and resolved in
  Asterisk's manager.conf), is the barrier, as for any process on the node.
  The agent logs in by MD5 challenge-response, so the password never
  crosses the connection: a host-network process that binds 127.0.0.1:5038
  while Asterisk is down (at every PBX pod start the agent dials before
  Asterisk runs) gets only the digest of its own challenge
  (`TestLoginNeverSendsTheSecret`; `make asterisk-check` logs in to the real
  Asterisk this way, and `TestModulesNarrowed`'s fakephone does too).
- **Asterisk image (`andrius/asterisk` 22.11.0 trixie).**
  - The image's entrypoint runs as root, `chown -R`s the config and data
    directories, then drops privileges with `-U asterisk -p`. The chart
    bypasses it, running `/usr/sbin/asterisk -f -n -C <live
    asterisk.conf>` directly as uid 1000 with no capabilities and a
    read-only root filesystem. emptyDirs hold the control socket, astdb and
    spool, and the compiled `asterisk.conf` points `astdbdir`,
    `astspooldir` and `astcachedir` at them.
  - **The image ships no sound prompts.** patchbay renders its own at
    image build (`make sounds`) and the agent installs them.
  - There is no `format_ulaw.so`: `format_pcm.so` handles `.ulaw`, `.alaw`
    and `.g722`. `res_pjsip_notify` declines to load without a
    `pjsip_notify.conf`, so the compiler always emits one.
  - `ModuleLoad` with `LoadType=reload` answers "Module not found." for
    the built-ins (`manager`, `logger`, `indications`); the agent reloads
    with the `Reload` action. `res_pjsip_notify` puts its own
    `Subscription-State: terminated` ahead of the `active` the agent sends.
- **`kind load` and digests.** See Images above.
- **A blind transfer needs the call bridged.** A REFER sent the moment the
  200 OK went out is accepted (202) but fails (`SIP/2.0 400 Bad Request`
  in its NOTIFY): Asterisk has not bridged the call yet. The desk phone
  in `TestDeskLoop` talks for 2 s first, as a person would; then
  `res_pjsip_refer` sends the caller on to 150 (`SIP/2.0 200 OK`).
- **The agent scrapes only phones it knows the address of**: one it has
  served a file to, or that registered. A phone that only asks for its
  `.sgn` (the Desk phone with its old trust list) is never scraped, so it
  becomes NeedsTrustReset by the driver's 3 min rule, not by its log
  (docs/plans/release-a.md §11). That rule, and the 3 min download window
  after it, make `TestPhoneLifecycle` the suite's longest test.
- **Asterisk on the host network reaches a ClusterIP Service by name.**
  The PBX pod's `ClusterFirstWithHostNet` resolves
  `patchbay-streams.patchbay-e2e.svc` through the cluster DNS, and the
  node's kube-proxy rules carry Asterisk's AudioSocket connection to the
  bridge pod. With the bridge scaled to 0 the Service has no endpoints,
  kube-proxy refuses the connection at once, `Dial` ends `CHANUNAVAIL`
  and the caller hears "not available" about 3 s after dialling
  (`TestStreamBridgeDown`). Talos's kubelet and kube-proxy are bench B9.
- **The kubelet synced the bridge's ConfigMap volume within seconds** in
  kind (`TestStreamAdded`: Idle 5 s after the Stream was created). A real
  node updates a mounted ConfigMap on its periodic sync, usually under a
  minute: the 150 s the test allows is for that, not for kind.
- **ffmpeg says "5XX", not "503".** At the contract's `-loglevel error`
  it prints "Server returned 5XX Server Error reply" for the fake tuner's
  503 (and the real HDHomeRun's), never the code or the
  `X-HDHomeRun-Error` header; `TestStreamTuner/refused` expects "5XX".
- **A PCM WAV probes slowly.** ffmpeg reads half a megabyte of a WAV
  before it decodes any, which at the fake source's real-time rate would
  hold the first tone back 2.7 s; the fake bursts each stream's first 4 s,
  as an Icecast mount does, so the first tone comes 0.1-0.2 s after the
  caller's first packet on the radio, and 1.5-2.4 s on the tuner (its
  1.5 s lock included).
