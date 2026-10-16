# CLAUDE.md: patchbay operating guide

patchbay is a Kubernetes-native PBX: Asterisk 22 (PJSIP) plus a Go
controller-runtime operator. **Read [docs/design.md](docs/design.md) before
changing behaviour.** It is the approved spec; §4 is the API, §5 the config and
observation flow, §8 the device state machines. Research behind the Cisco facts
is in `docs/research/`.

## Architecture

```
resources (Phone, Softphone, Person, Stream)
   -> controller   Deployment; the ONLY decider and the ONLY status writer
        Compile()          -> ConfigMap pbx-config (placeholders, no secrets)
        driver.Decide()    -> status, actions
        GET /v1/observations, POST /v1/actions/... (agent control API, :9440)
   -> PBX pod (hostNetwork, pinned node)
        agent      native sidecar: applies bundles, serves device HTTP :8088,
                   scrapes phones, talks AMI, keeps the observations.
                   Senses and acts; never decides.
        fileserver TFTP :69 and HTTP :6970; binds as root, then runs as
                   65533 with no capabilities; reports each transfer to the
                   agent over a unix socket
        asterisk   SIP 5060, RTP 10000-10099; sees only the compiled modules
```

**Pure core, imperative shell.**
- `compile.Compile` and each driver's `Render` and `Decide` are pure
  functions: no I/O, no clock (`now` is an argument), no randomness.
  `Compile`/`Render` are golden-file tested; `Decide` is an ordered rule table,
  table-tested row by row.
- All I/O lives in the shell: `internal/controller` (reads resources, polls the
  agent, writes status, sends actions) and `internal/agent`.
- Only the controller decides and writes status. The agent's memory is
  disposable; whatever must survive is copied into status by the controller.
- State lives in conditions; `status.state` is display only. Every switch over
  a state enum is complete: the `exhaustive` linter fails the build otherwise.
- Secrets never appear in compiled files, only `@@secret:<name>/<key>@@`
  placeholders that the agent resolves.

Contracts: `internal/compile` (Bundle with its device index, Inputs, Boot,
Settings, device Secret names; in `layout.go` the bundle's four trees, the
live directory the agent swaps and the sound prompts; in `configmap.go` the
pbx-config ConfigMap encoding), `internal/drivers` (Driver and the optional
SettingsChecker, Device and the DeviceID format, Memory and its DeepCopy,
UserText, RegistrationGrace, Observations, DecideSettings, Decision, Profile with
EndpointOptions, FirmwareLoad), `internal/agent/api` (the control API's
JSON), and the agent's internal seams: `internal/agent/sensor` (fetch
events, the file server's socket format, the scrape and firmware sensors),
`internal/agent/provision` (one-time token grants), `internal/agent/layout`
(mounts, uids, file modes, version directories), `internal/agent/ami/ami.go`
(the AMI client interface), `internal/agent/firmware/store.go` (the
firmware store interface) and `agent.Options` (what the control API
serves); and the controller's (W3): `internal/controller/install` (the
install's object names, keys, labels, the compiled bundle written as
pbx-secrets then pbx-config and read back with `ReadCompiled`, secret
generation, Event reasons and which Events client-go joins into a series,
the bounds on status and Event text), `internal/controller/agentview` (the poller's
`Snapshot`, `Observer`/`Holder`, the `Changes` enqueue channels,
`SnapshotFunc`, the `Actor` the device reconcilers send actions through),
`internal/controller/ctrltest` (the shared envtest harness),
`internal/cli/env.go` (the CLI's seams) and `test/e2e/fakephone/script`
(what `fakephone run` does and reports). Change them deliberately: the
controller, agent, drivers and e2e fake phone all build on them. The release A plan, with the reasoning behind
each contract, is [docs/plans/release-a.md](docs/plans/release-a.md).

## Deviations from the design

`docs/design.md` is a verbatim copy of the approved spec, so where the code
departs from it, the departure is recorded here (and in the code's docs).

- **The chart is published by CI's `release` job, not chart-releaser-action**
  (design §6 "CI and releases"). Same result: the chart is a GitHub release
  asset and the Helm index lives on gh-pages at
  `https://christopherjmiller.github.io/patchbay`. But the app and the chart
  share one version, set from the `v*` tag at package time, and
  chart-releaser-action releases a chart only when `charts/` changed
  (docs/releasing.md).
- **Per-device config versions.** `compile.Bundle.Devices[id].Version`, so
  ConfigStale (§8.3) compares each phone with its own compiled version, not
  one install-wide version.
- **SIP identity** (§4 leaves it open): a device's PJSIP endpoint and auth
  username is its ID (`phone-desk`, `drivers.DeviceID`), its SIP user and
  AOR are its number (`100`), and endpoints are identified by auth username.
  The ID survives renumbering; the line name stays the plain number the 79x5
  expects. IDs are at most 30 characters, the 79x5's `authName` limit.
- **The running load** (§8.2 names the "app/boot load ID"): the scraper
  also reads `/DeviceInformationX`'s `versionID`, the load ID that
  `loadInformation` and the manifest use; `appLoadID` is the application
  image (`jar45sip….sbn`), which never equals a load name. The bench 7945
  (2026-10-15) fills both and wraps `versionID` in asterisks
  (`*SIP45.9-4-2SR4-3S*`): the agent reports it as given, the driver trims
  them from every reported load and counts either field that names the
  target load.
- **Only `/DeviceInformationX` decides a scrape** (§8.2 lists three
  pages): the bench 7945 breaks off its own `/NetworkConfigurationX` (a
  NullPointerException mid-page) and serves `/DeviceLogX` empty, so those
  two are best effort (`api.WebScrape`): an unreadable one is nil, a
  broken-off one keeps the fields before the break, and neither fails the
  scrape.
- **PBX down**: §7 says `Registered=False`, §8.3's Unknown row says
  `Registered=Unknown`. With Asterisk down the device is certainly not
  registered, so it is `False`, reason `PBXDown`; an unreachable agent is
  `Unknown`, reason `AgentUnreachable`.
- **Firmware digests can be pending** (`drivers.SHA256Pending`) until taken
  from Cisco's zip: a pending file is never accepted, served or counted as
  staged. The 9.4(2)SR4-3 load's were filled in on 2026-10-15 from a copy
  whose files match Cisco's published sizes; they are to be confirmed against
  Cisco's own copy (the CUCM device pack).
- **Labels and names reject control characters** in the schema, because
  they are rendered into device XML and Asterisk config.
- **Display power-save pulled forward from release C** (§4's Phone has no
  such field; §10 adds phone extras in C): `Phone.spec.display`
  (`idleTimeout`, `wakeOnCall`, `onWindow{start, duration, days}`, whole
  minutes, validated by the CRD) and a house default in the settings
  (`compile.Settings.PhoneDefaults.Display`, settings.json
  `phoneDefaults.display`, from the chart value of that name) that fills
  each field a Phone leaves unset (`onWindow` whole;
  `drivers.DisplayWithDefaults`); a change rolls only the controller. The
  chart checks its shape, the controller its values. The 79x5 renders
  vendorConfig's display elements (cisco79x5/display.go), and nothing
  without either, so a phone keeps Cisco's schedule. `displayPowerSave` is
  `Untested` until the bench answers (plan §8). On a model whose profile
  lacks it, a Phone's own display is Invalid with `Unsupported=True`
  (reason `UnsupportedFeature`, §8.3's "unsupported feature" rule), and the
  house default is left out.
- **Softphone status has memory too** (`observed`, `lastAction`,
  `humanAction`), which §8.4's Offline and rotateToken need.
- **Bundle trees.** §5 says TFTP and HTTP "serve from the same live
  directory". They serve only their own tree (`tftp/`, `http/`); Asterisk's
  `asterisk/` tree, which holds every password once resolved, is never
  served.
- **A separate file server** (§5 has the agent serve TFTP :69 and HTTP
  :6970): `patchbay fileserver`, a third container in the PBX pod, binds
  both as root with only `NET_BIND_SERVICE`, `SETUID` and `SETGID`, then
  drops to uid 65533 with no groups and no capabilities before it reads a
  file. The code that parses packets from the voice VLAN holds no token
  and no AMI password, and file modes keep it from every resolved file but
  the `tftp/` tree it serves. It reports transfers to the agent over a unix
  socket (docs/plans/release-a.md §5.2.1).
- **One projected Secret** (§5: the agent "watches pbx-config and its
  Secrets"): the agent reads only `<fullname>-pbx-secrets`, which the
  controller writes before each pbx-config with every secret the bundle
  references (`Bundle.SecretRefs`), each under `SecretRef.ProjectedKey`
  (`<name>_<key>`). The agent's Role names that Secret and the ConfigMap,
  nothing else. The agent applies a version only with the values it was
  compiled with (`Bundle.VersionWith`): a Secret written ahead of its
  ConfigMap is ignored, a ConfigMap ahead of its Secret waits 30 s for it.
- **Spent provisioning tokens survive restarts** (§11: "The token is then
  spent: the same URL no longer serves"; nothing says where): device HTTP
  writes each spend (the token's SHA-256 and the served fetch) to
  `layout.StateDir` on the PBX's persistent volume *before* it sends the
  document, re-records the served fetches at start, and forgets a spend
  only once the *settled* bundle (verified, and not restored by a
  rollback) no longer grants that token, so a URL is never reusable after
  an agent restart or a rollback.
- **The persistent volume** (§6: the `firmware` PVC) is mounted whole at
  `layout.PersistentDir` and holds the firmware store and the agent's
  state: read-write in the agent, read-only in the file server.
- **Asterisk sees only the compiled modules**: an init container copies
  `compile.Modules()` out of the image into an emptyDir mounted read-only
  over `/usr/lib/asterisk/modules` (a separate `astmoddir` would not do:
  Asterisk loads `../` paths through it). Even the AMI account cannot
  load `app_system`, `func_shell`, `res_agi` or `res_ari` (e2e
  `TestModulesNarrowed`).
- **Reloads** (§5: "module reload res_pjsip.so, then dialplan reload") use
  AMI's `Reload` action: `ModuleLoad` cannot reload the built-ins
  (`manager`, `logger`, `indications`) on Asterisk 22.11. The resync
  NOTIFY carries Asterisk's own `Subscription-State: terminated` ahead of
  the agent's `active` (res_pjsip_notify adds it); whether the 7945 minds
  is a bench question (docs/plans/release-a.md §8).
- **The agent applies only versions its pod's Asterisk can run without a
  restart** (§5 has every version reloaded): asterisk.conf, modules.conf
  and the PJSIP transports only load at a restart (`compile.Boot`), so
  the agent (`--sip-bind`, pbx.address:pbx.ports.sip) refuses a version
  compiled for another SIP bind or by another release, before writing it.
  A settings change rolls the PBX pod and the controller together; the
  new pod stays unready, refusing the old controller's version, until the
  new one has compiled, instead of booting Asterisk on the old transports
  and then reporting the new version good after a reload that kept them.
- **Ready also needs the prompts** (§5: the startup probe passes once the
  first version is applied): `/readyz` also requires every sound prompt
  installed from the image, so 911 never plays a missing announcement.
  The image carries them in every voice of an allowlist; the chart's
  `sounds.voice` (default `espeak`) picks one, and the agent
  (`--sounds-voice`) refuses any other (docs/sounds.md).
- **Request annotations** (`patchbay.chrismiller.xyz/resync-requested`,
  `.../reissue-requested`) are acted on once per value, recorded in
  `status.handledRequest`, so the controller never edits metadata.
- **MACConflict**: two Phones with one MAC; oldest wins, like numbers.
- **Decide** takes `DecideSettings` (signalling backend, staleness), as
  §8.1's signature has settings; §4's sketch omits them.
- **Firmware manifests are grouped by load** (`[]FirmwareLoad`), because two
  loads both carry `term45.default.loads`; §4's sketch has a flat list.
- **e2e runs Kubernetes 1.33** (the target cluster's minor) and the chart
  requires >= 1.33.
- **Labels and names lose runs of `@`** (§4 says "as written"): Compile
  collapses every run of `@`, with any white space between them, in a
  label or person name to one `@`, so text a person writes can never form
  a `@@secret:…@@` placeholder that the agent would resolve into a file
  served to devices, even once a driver drops its spaces (the 79x5's
  screen label does). The collapse is `drivers.UserText`, which the 79x5's
  `LabelShortened` applies too, so it reports what the phone shows.
- **The number plan also refuses shadowing** (§4 only says the controller
  enforces it): a fixed-length pattern may not match a prefix of a number
  in Devices, People, Streams or a built-in (People `1X` beside Devices
  `1XX`), because the 79x5 dial template would dial the short number
  first. Numbers only a reserved pattern holds may be shadowed (`911`
  dials before `9.`'s `9111` could).
- **Settings come from a chart-rendered file** (§5 has the controller
  compile with the install's settings; nothing says where they live): the
  chart renders `<fullname>-settings` (`settings.json`, `compile.Settings`)
  from its values, the controller reads the mounted file once at start
  (strict JSON, `install.CheckSettings`), and a `checksum/settings`
  annotation rolls the controller when one changes. No watch, no RBAC.
  The controller also checks them against every driver that refuses some
  install-wide values (`drivers.SettingsChecker`,
  `compile.Settings.CheckDrivers`): a time zone the 79x5 has no name for
  stops it at start, naming the zones it takes, instead of failing every
  compile once the first Phone exists.
- **The controller mints the agent token** (§4) into `<fullname>-agent-token`;
  the chart's override and its "create it first" note are gone. The PBX
  pod's Secret volume is not optional, so the pod waits in
  ContainerCreating until the controller has written it.
- **The softphone's provisioning URL shows the compiled token**
  (`pbx-secrets`' value, only while its version is `pbx-config`'s), not the
  device Secret's: a rotated token is shown once the agent can serve it.
  And only while the softphone is `Issued` (§8.4) and its device Secret
  still holds that token: the URL is a live grant of the SIP password, so
  a Provisioned, Registered or Offline softphone shows none (anyone who can
  read Softphones but not Secrets would otherwise have it), nor does a
  token a reissue is replacing.
- **No status before the first poll** (a restarted controller never flips
  every phone to Unknown for one poll) and none for a device that is not in
  the compiled index, or while `pbx-config` does not exist.
- **One leader-election Lease per install**
  (`<fullname>-controller.patchbay.chrismiller.xyz`), so two installs in a
  namespace never share a leader, and the Role names it.
- **The controller's Role is least privilege**: it reads its resources and
  writes only their status (and phones'/softphones' finalizers, for
  `blockOwnerDeletion`); it creates ConfigMaps and Secrets and updates them
  (pbx-config by name; a device Secret's name is per resource); it never
  deletes or patches. So its objects outlive `helm uninstall` (§11 of the
  plan says how to remove them).
- **The controller refuses unusable Secrets** (`secrets.ErrInvalid`): a
  value outside its generator's alphabet, a Secret of another type or
  owner. The agent resolves values into config files unescaped, so a
  hand-edited Secret is a `CompileFailed` Event, not a broken file.
- **Discovery is once per MAC per agent start, in the controller's
  memory**: a controller restart reports each still-asking phone once more.
  Each UnknownPhone Event names its MAC in `related` (else client-go joins
  them into one series with the first note); at most 16 go out at once and
  one per 30 s after, and only MACs the agent still lists are remembered.
- **A registration is live for 15 s past its expiry**
  (`drivers.RegistrationGrace`; §8 says a lapsed registration is no
  contact): the 79x5 refreshes 5 s before expiry and the controller sees
  the refresh up to two 5 s polls later, so without it a healthy phone
  flickered to Unreachable. The e2e fakephone's cisco mode refreshes 5 s
  before expiry too.
- **Text from devices is bounded**: the agent keeps each scraped field to
  128 bytes and each log line to 256, control characters as spaces; the
  controller cuts condition messages and human actions at 2048 bytes and
  Event notes at 1024 (events.k8s.io's limit), so a hostile web page can
  never make a status write fail or an Event be refused, and `patchbay
  status` prints no control character.
- **The CLI never uses the in-cluster config**: `--kubeconfig`, else
  `$KUBECONFIG`, else `~/.kube/config`.
- **Three more metrics** than §10 lists: `patchbay_compile_failures_total`,
  `patchbay_agent_reachable` (the "agent unreachable" alert) and
  `patchbay_asterisk_up` (the "PBX down" alert; absent while the agent
  cannot be reached).
- **The agent Service publishes not-ready addresses**: the PBX pod is
  NotReady whenever Asterisk is, and the controller reaches the agent only
  through that Service, so without it an Asterisk outage read as
  `AgentUnreachable` instead of §7's `PBXDown` (e2e `TestPBXDown`).
- **The control API caps its connections** (64, 16 per source) like device
  HTTP and :6970, but the node's loopback (127.0.0.1, ::1) is outside the
  total: the agent's probes set `host: 127.0.0.1`, so LAN hosts holding
  every other place cannot fail them, and the CLI's port-forward arrives
  there too. A source is an IPv4 address or an IPv6 /64 on all three
  listeners. Idle control connections close after 15 s.
- **A phone that only asks for its `.sgn` is never scraped** (§8.2 takes
  the phone's IP from the fetch log, before it registers): the agent
  takes it only from a fetch it served or a registration (W2), so the
  Desk phone with its old trust list never has its log read for the trust
  error; it becomes NeedsTrustReset by the 3 min `.sgn` rule (e2e
  `TestPhoneLifecycle/reset`; plan §11).

## Commands

Everything runs in the flake's shell. `go`, `kind`, `helm` and the rest are not
on the global PATH.

```sh
cd /path/to/patchbay && nix develop -c make <target>
make generate     # after ANY api/v1alpha1 change; commit the generated files
make fmt vet lint # golangci-lint v2 (.golangci.yml) + helm lint
make test         # unit + envtest (KUBEBUILDER_ASSETS comes from the flake), then
                  # the unit tests again with -tags e2e (cisco79x5's e2e test load),
                  # and the one envtest that needs that load (TestPhoneLifecycleFirmware)
make test-unit    # -short: skips envtest
make build image  # bin/patchbay; ghcr.io/christopherjmiller/patchbay:dev (GO_TAGS=e2e
                  # only for e2e images and .e2e/bin/patchbay: the e2e firmware load)
make sounds       # the prompts in every voice into build/sounds/<voice>; make image runs it.
                  # espeak-ng + ffmpeg from the shell; Piper and its voices are flake packages
                  # (.#piper, .#piper-voices) that gen-sounds `nix build`s, never in the shell
make tools-versions
make check-versions # envtest's kube-apiserver minor = go.mod's k8s.io minor, flake Go minor =
                    # the Dockerfile's (hard rule 1, checked; make test runs it first)
make e2e          # kind + chart + controller + real Asterisk + fakephones; docs/e2e.md
                  # PATCHBAY_E2E_KEEP=1 keeps the cluster, PATCHBAY_E2E_REUSE=1 reuses it
make e2e-down     # delete the e2e cluster
make check-hygiene # no tracked binaries, big files, firmware types or secrets
make vulncheck    # govulncheck with the Dockerfile's Go (not the flake's): CI gates on it
make asterisk-check # every compile golden booted in the chart's Asterisk image (docker,
                    # no cluster), then registered to and called as each device;
                    # also the agent's AMI client against the real Asterisk
```

CI, the `ci-ok` required check, Renovate's automerge policy, image tags and
cutting a release: [docs/releasing.md](docs/releasing.md). Never publish by
hand; a `v*` tag does it.

The CRDs are generated into `charts/patchbay/templates/crds/` and carry
`helm.sh/resource-policy: keep`. Never edit them, or `zz_generated.*`, by hand.
`test/chart` renders the chart (`helm template`) and checks it against the
Go layout contracts: change a path, uid or module in one and the test names
the other.

The prompt voices are an allowlist kept in four places: `sounds.Voices`
(internal/agent/sounds, the reference), `hack/gen-sounds.sh`, the flake's
`piper-voices` (each file pinned by sha256) and the chart's
`patchbay.soundsVoice`. The tests compare the script's `--list` and the
chart's refusal with `sounds.Voices`; `make sounds` fails if `piper-voices`
lacks one. A new voice needs a public domain dataset and no lessac in its
lineage (docs/sounds.md says why).

## Hard rules

1. **Toolchain from the flake only.** Bump nixpkgs with `nix flake update`, then
   run `make tools-versions` and `make test` before committing the lock. Keep
   the kube-apiserver minor (envtest) in step with the `k8s.io/*` minor in
   `go.mod`, and the Go minor in step with the Dockerfile's builder
   (`make check-versions` fails otherwise; `make test` runs it).
2. **Kubeconfig isolation.** Never read or write `~/.kube/config`, and never
   touch any cluster other than the e2e kind cluster. Its kubeconfig lives
   only at `.e2e/kubeconfig` (gitignored; `$PATCHBAY_E2E_KUBECONFIG` in the
   shell): create it with `kind create cluster --name patchbay-e2e
   --kubeconfig .e2e/kubeconfig`, and pass that file explicitly to every
   kubectl, helm and client-go call. Tests that run the CLI pass a
   `--kubeconfig` that does not exist, so nothing falls back to a real one.
3. **At most one kind cluster, named `patchbay-e2e`.** The workstation has
   about 7 GB of free RAM. Delete it when you are done with it.
4. **No firmware, secrets or binary blobs in the repo, ever, encrypted or
   not.** The repo is public. Firmware lives on the PBX's PVC; the repo holds
   only a manifest of names and SHA-256s. `.gitignore` blocks firmware,
   archive and secret file types (`*.sbn`, `*.loads`, `*.cop`, `*.sgn`,
   `*.tlv`, `*.zip`, `*.tar`, `*.bin`, `*.secret*`, ...) and the top-level
   `/firmware/` staging directory; do not work around it (`git add -f`).
   `make check-hygiene` (CI runs it) fails on any tracked binary, any file
   over 512 KiB, any of those types, or a secret anywhere in the history
   (gitleaks). Test fixtures are generated, not copied from a real phone or
   Cisco.
5. **Pinned images.** Every image is pinned by digest (tag plus digest, for
   example `golang:1.26.8-trixie@sha256:...`), including test images.
6. **Git hygiene.** Small, logical commits. Stage explicit paths, never
   `git add -A` or `git add .` (other agents may be writing files). Never
   bypass hooks. Do not create remotes, push, or open GitHub repos unless
   Chris asks.
7. **Evidence before claims.** Never say something works without running it,
   and report failures and deviations from the spec plainly.
