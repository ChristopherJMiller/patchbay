# End-to-end tests (kind)

The e2e suite runs patchbay the way production does, as far as one laptop
can: a kind cluster, the real Helm chart, the real PBX pod (Asterisk 22 on
the host network of a pinned node, the agent sidecar, the file server), and
**fakephone**, a simulated Cisco 79x5, as ordinary Pods. The real 7945 is
only the final acceptance test (design §6).

## Compiled config

Asterisk runs **compiled config**, applied by the agent, exactly as in
production. Until the controller compiles (W3), the harness stands in for
it (`test/e2e/bundle_test.go`):

- it compiles a fixed install with `compile.Compile` and the built-in
  drivers (`builtin.Registry()`, which in the `-tags e2e` test binary also
  knows the e2e firmware load): Phone `desk` (cisco-7945, MAC
  `0200E2E00001`, number 100), Softphone `laptop` (150), and Person `chris`
  (301) ringing both;
- the settings are the chart's ports, the kind node's InternalIP as the PBX
  address, and the node's **pod CIDR** (`10.244.0.0/24`) as the only
  allowed network: fakephone registers from the pod network, and the
  compiled ACL refuses everything else;
- it keeps every generated secret where the controller will: one Secret per
  device (`patchbay-phone-desk`: `sip-password`;
  `patchbay-softphone-laptop`: `sip-password`, `provisioning-token`) and
  `patchbay-ami` (`password`), each random and alphanumeric (12, 24, 32,
  32 characters), kept as they are on a reused cluster (but for the
  laptop's provisioning token, which `TestProvisioningOneTime` rotates on
  every run);
- it writes `patchbay-pbx-secrets` (every `Bundle.SecretRefs()` key, under
  `SecretRef.ProjectedKey`) and then `patchbay-pbx-config`
  (`Bundle.ConfigMapData`), before installing the chart, so the agent
  applies the bundle as soon as it starts.

The controller still runs (it has no reconcilers yet, and `TestController`
checks its leader election), and the harness still creates the agent token
Secret. W3 replaces both stand-ins with the real controller.

fakephone registers as a compiled device: its number is the SIP user (AOR,
From, To, Contact) and its device ID the digest username (`--user 100
--auth-user phone-desk`), its password from the device's Secret.

## The tests

| Test | Proves |
|---|---|
| `TestCRDs` | The chart installs the CRDs; `config/samples` are accepted and read back typed; a CEL rule is enforced by the API server. |
| `TestPBXUp` | The PBX pod's shape: on the host network of the pinned node, the `asterisk-modules` init container completed, the agent a native sidecar, `asterisk` and `fileserver` running, nothing restarted. **Identities, from `/proc`** (what the kernel enforces, not the spec): Asterisk is uid/gid 1000 with no capabilities, `no_new_privs` and a read-only root; the agent is 65532 with no capabilities (group 1000, the pod's fsGroup); the file server is 65533:65533 with **no groups**, every capability set empty except the bounding set (see below) and `no_new_privs`. The file server's identity can read `current/tftp/` and **cannot read** `manager.conf`, `pjsip.conf`, a softphone's provisioning document or `bundle.json`. Asterisk runs the compiled config: transports on the node IP, both endpoints, every `compile.Modules()` module Running, AMI on loopback, HTTP off, the echo extension. The agent answers through its Service from the pod network (readyz; 401 without the token; the observations with the applied version), and over a port-forward reports AMI up and no apply error. |
| `TestController` | The controller wins leader election (holds its Lease) under the chart's narrowed Role. |
| `TestRegister` | Phone desk registers over **TCP** and Softphone laptop over **UDP**, each challenged first; Asterisk holds the pod's contact and User-Agent, and the agent reports the registration with its **Call-ID**. A wrong password ends on **401**, and so does a contact outside the allowed networks (the endpoint's contact ACL: Asterisk 22 re-challenges, it does not answer 403). Only a **source** outside them meets the global ACL, which answers **403** without a challenge. |
| `TestEchoCall` | Calling `600` crosses pod network → node IP for SIP and RTP both ways, and a 1 kHz PCMU tone comes back from the node's own address. |
| `TestModulesNarrowed` | Asterisk's module directory, and `module show`, hold exactly `compile.Modules()`. Over AMI, as the agent's own account (from a host-network pod: AMI is on the node's loopback), a `ModuleLoad` reload works, and `ModuleLoad` of `app_system.so`, `func_shell.so`, `res_agi.so`, `res_ari.so` and `../usr/lib/asterisk/modules/app_system.so` all fail. |
| `TestTFTPAndHTTP6970` | The file server serves `SEP<MAC>.cnf.xml` with the phone's password resolved, byte for byte the same over TFTP and HTTP :6970; the `.sgn` and an unknown MAC's SEP file are not found. Every request is in `/v1/observations`: attributed to `phone-desk` (served ones with its config version) or, for the unknown MAC, in `unknownDevices` with the requesting pod's address. |
| `TestFirmwareUploadAndServe` | The e2e load (`E2E45.0-0-1S`, built only with `-tags e2e`) goes up through the agent over a port-forward with the token, the path `patchbay firmware load` will take: a corrupted file **422** (nothing stored), the good one **201**, again **200** (on a reused cluster, whose persistent volume keeps the store, the files already there answer 200 at once); the store sensor reports it present, and the file server serves it over TFTP and HTTP :6970. |
| `TestPrompts` | `911`, `0` (reserved) and `199` (unassigned) are answered with at least 2 s of non-silent audio, at least 1 s of it not the SIT tone: the prompts the agent installed from the image play. |
| `TestResyncNotify` | `POST /v1/actions/resync` makes the agent send the registered fakephone a service-control NOTIFY whose body is the plan's, quoting its REGISTER's Call-ID; fakephone then refetches its SEP file over TFTP. |
| `TestProvisioningOneTime` | First the laptop's token is rotated as the controller will (its Secret, `pbx-secrets`, then `pbx-config`), and the test waits for the new version to be applied and verified: a reused cluster's token was spent by the last run, and the agent must take a version whose Secret came first. Then Softphone laptop's provisioning URL serves its document once over the pod network (200), then 404; the agent container is killed, and after it restarts the URL is still 404 (the spend is on the persistent volume) and the served fetch, with the softphone's version, is back in the observations. |
| `TestRollback` | A `pbx-config` whose `pjsip.conf` renames Phone desk's endpoint (with the version `Bundle.VersionWith` gives it, as the agent applies a version only with the secret values it was compiled with): the agent applies it, fails verification (after its 30 s), swaps the previous version back and reloads; it reports `lastApplyVersion` and the verify error while `appliedVersion` stays the good one, and the old config still registers and echoes. The good ConfigMap is written back afterwards. |

## Running it

Everything runs inside the flake's shell.

```sh
nix develop -c make e2e          # build images, fresh cluster, test, delete cluster
PATCHBAY_E2E_KEEP=1  nix develop -c make e2e   # ...and keep the cluster afterwards
PATCHBAY_E2E_REUSE=1 nix develop -c make e2e   # reuse a kept cluster
nix develop -c make e2e-up       # cluster + chart, no tests, kept (REUSE+KEEP)
nix develop -c make e2e-down     # delete the cluster
nix develop -c make e2e-images   # just build/pull the three images
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
- A fresh run takes about four minutes once the images are cached: about
  40 s to create the cluster, 15 s to install and roll out, two minutes of
  tests (the rollback alone waits 30 s for verification to give up).
- Only one kind cluster, `patchbay-e2e`, is ever created.

### Kubeconfig isolation

The suite never reads or writes `~/.kube/config`:

- the kubeconfig is **`.e2e/kubeconfig`** (gitignored), and every `kind`,
  `kubectl` and `helm` call passes `--kubeconfig` explicitly;
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
| `ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e` | `test/e2e/fakephone/Dockerfile` | same |
| `andrius/asterisk:<tag>` | the chart's `pbx.asterisk.image`, **pulled by digest** on the host and tagged | same |

The patchbay image is built with the `e2e` build tag: only then does the
firmware manifest hold the e2e test load (docs/plans/release-a.md §2(b)).
No other image may be built with it. `make image` renders the sound prompts
first (`make sounds`: espeak-ng and ffmpeg from the flake into
`build/sounds/`, which `.dockerignore` admits and `.gitignore` keeps out
of git); the Dockerfile copies them to `/usr/share/patchbay/sounds/`.

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
3. Load the three images.
4. Create the namespace (labelled `pod-security.kubernetes.io/enforce:
   privileged`, as a real install needs), delete any fakephone pods a
   reused cluster still holds, create the agent token Secret, and write the
   compiled bundle (above).
5. `helm upgrade --install patchbay charts/patchbay` with
   `test/e2e/values.yaml` plus `.e2e/values-runtime.yaml` (the node's name
   and InternalIP as `pbx.nodeName`/`pbx.address`, and the image IDs).
6. Wait for both Deployments to roll out, the way `kubectl rollout status`
   does, polling for up to 5 minutes.

Each fakephone run is a one-shot Pod (`restartPolicy: Never`) of the tools
image, on the pod network unless a test needs the node's (`hostNetwork`:
AMI on the node's loopback, a source outside the allowed networks) or its
processes (`hostPID`: `fakephone proc` reads the agent's and the file
server's `/proc/<pid>/status`, since their distroless image has no shell).
Each run says which outcome it expects: a pod that ends as expected is
deleted, and any other is kept for the diagnostics. The tests that talk to
the agent's control API many times (observations, the firmware upload, the
resync action) go through a **port-forward** to the PBX pod from the
harness (`pods/portforward`, as the CLI will), with the agent token from
its Secret; `TestPBXUp` also checks the Service path from the pod network
that the controller uses.

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
   container's logs (the agent's, the file server's and the fakephone pods'
   included).
2. **Keep the cluster** and look around:

   ```sh
   PATCHBAY_E2E_KEEP=1 nix develop -c make e2e
   export KUBECONFIG=$PWD/.e2e/kubeconfig
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c agent
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c fileserver
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c asterisk
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

5. **Run fakephone by hand**, with SIP traces (`-v`), as Phone desk:

   ```sh
   NODE_IP=$(kubectl get node patchbay-e2e-control-plane \
     -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
   PW=$(kubectl -n patchbay-e2e get secret patchbay-phone-desk -o jsonpath='{.data.sip-password}' | base64 -d)
   kubectl -n patchbay-e2e run fp --rm -i --restart=Never \
     --image ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e --image-pull-policy=Never \
     -- call 600 --server "$NODE_IP:5060" --user 100 --auth-user phone-desk --password "$PW" -v
   ```

   Subcommands: `register` (with `--wait-notify`), `call <number>`, `tftp
   get <file>`, `http get|send`, `ami`, `proc`, `read`. `fakephone --help`
   documents the flags (most also read `FAKEPHONE_*`).
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
