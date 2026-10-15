# End-to-end tests (kind)

The e2e suite runs patchbay the way production does, as far as one laptop
can: a kind cluster, the real Helm chart, real Asterisk 22 on the host
network of a pinned node, and **fakephone**, a simulated Cisco 79x5, as an
ordinary Pod. The real 7945 is only the final acceptance test (design §6).

Today the suite is the *walking skeleton*. The controller and agent run but
do not compile or apply config yet, so Asterisk runs a **temporary static
config** (`pbx.staticConfig`, e2e only). Its job is to prove the risky
infrastructure before the real components land on it:

| Test | Proves |
|---|---|
| `TestCRDs` | The chart installs the CRDs; `config/samples` are accepted and read back typed; a CEL rule (a ring target naming both a phone and a softphone) is enforced by the API server. |
| `TestPBXUp` | The PBX pod runs on the host network of the pinned node with the agent as a native sidecar. Asterisk 22 is up as uid 1000 on a read-only root, binds SIP to the node IP over UDP and TCP, loads every module (including AudioSocket), keeps AMI on loopback and HTTP off. The agent answers `/readyz` and `/v1/observations` (401 without the token, 200 with it) **through its Service from the pod network**: the path the controller will use. |
| `TestRegister` | fakephone registers with digest auth over **TCP** (what the 79x5 uses) and over UDP; Asterisk holds the pod's contact and User-Agent. |
| `TestEchoCall` | fakephone calls `600`; SIP crosses pod network → node IP, RTP crosses both ways, and a 1 kHz PCMU tone comes back (Goertzel check on every 20 ms packet) from the node's own address. An unrouted number fails with 404, and fakephone reports it. |

## Running it

Everything runs inside the flake's shell.

```sh
nix develop -c make e2e          # build images, fresh cluster, test, delete cluster
PATCHBAY_E2E_KEEP=1  nix develop -c make e2e   # ...and keep the cluster afterwards
PATCHBAY_E2E_REUSE=1 nix develop -c make e2e   # reuse a kept cluster (fast: ~30 s)
nix develop -c make e2e-up       # cluster + chart, no tests, kept (REUSE+KEEP)
nix develop -c make e2e-down     # delete the cluster
nix develop -c make e2e-images   # just build/pull the three images
```

- Without `PATCHBAY_E2E_REUSE=1` an existing `patchbay-e2e` cluster is
  deleted first, so a default run always starts from scratch.
- Without `PATCHBAY_E2E_KEEP=1` the cluster is deleted at the end, pass or
  fail.
- A fresh run takes about two minutes once the images are cached; the cluster
  node uses about 0.9 GB of RAM.
- Only one kind cluster, `patchbay-e2e`, is ever created.

### Kubeconfig isolation

The suite never reads or writes `~/.kube/config`:

- the kubeconfig is **`.e2e/kubeconfig`** (gitignored), and every `kind`,
  `kubectl` and `helm` call passes `--kubeconfig` explicitly;
- the Makefile and the harness also set `KUBECONFIG` to it, so a tool that
  ignores the flag still cannot fall back to your real config.

Use the same file by hand:

```sh
export KUBECONFIG=$PWD/.e2e/kubeconfig      # inside the repo, in nix develop
kubectl -n patchbay-e2e get pods -o wide
```

### Images

| Image | Built from | How it gets into kind |
|---|---|---|
| `ghcr.io/christopherjmiller/patchbay:e2e` | `Dockerfile` (`make image`) | `kind load docker-image`, `pullPolicy: Never` |
| `ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e` | `test/e2e/fakephone/Dockerfile` | same |
| `andrius/asterisk:<tag>` | the chart's `pbx.asterisk.image`, **pulled by digest** on the host and tagged | same |

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

1. Create the cluster from `test/e2e/kind.yaml` (one node, the node image
   kind v0.32.0 pairs with, pinned by digest), or reuse it.
2. Set the node's `net.ipv4.ip_unprivileged_port_start` back to 1024 (see
   below).
3. Load the three images.
4. Create the namespace (labelled `pod-security.kubernetes.io/enforce:
   privileged`, as a real install needs) and the agent token Secret: the
   controller will mint that token (design §4); until then the harness does.
5. `helm upgrade --install patchbay charts/patchbay` with
   `test/e2e/values.yaml` plus `.e2e/values-runtime.yaml` (the node's name
   and InternalIP as `pbx.nodeName`/`pbx.address`, and the image IDs).
6. Wait for both Deployments to roll out, the way `kubectl rollout status`
   does, polling for up to 5 minutes.

Each fakephone run is a one-shot Pod (`restartPolicy: Never`) with
`FAKEPHONE_SERVER=<node IP>:5060` and the test credentials in its
environment. The test reads its exit status and logs. Pods that succeed are
deleted; failed ones are kept for the diagnostics.

## Debugging a failure

1. **Read the diagnostics.** After a failed test (or a setup that never
   rolled out) the harness prints, and saves under `.e2e/diagnostics/`, the
   namespace's objects and events, `kubectl describe pods`, and every
   container's logs (the agent sidecar's included, plus fakephone pods).
2. **Keep the cluster** and look around:

   ```sh
   PATCHBAY_E2E_KEEP=1 nix develop -c make e2e
   export KUBECONFIG=$PWD/.e2e/kubeconfig
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c asterisk
   kubectl -n patchbay-e2e logs deploy/patchbay-pbx -c agent
   ```

3. **Ask Asterisk.** Its CLI works through `kubectl exec`:

   ```sh
   ax() { kubectl -n patchbay-e2e exec deploy/patchbay-pbx -c asterisk -- asterisk -rx "$*"; }
   ax pjsip show endpoints
   ax pjsip show contacts            # truncates long URIs...
   ax database show registrar        # ...astdb holds each contact in full (uri, user_agent, via)
   ax pjsip show transports
   ax module show like pjsip
   ax dialplan show e2e
   ax core show channels
   ax pjsip set logger on            # SIP traces into the asterisk container log
   ax rtp set debug on               # every RTP packet into the log (noisy)
   ```

4. **Run fakephone by hand**, with SIP traces (`-v`):

   ```sh
   NODE_IP=$(kubectl get node patchbay-e2e-control-plane \
     -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
   kubectl -n patchbay-e2e run fp --rm -i --restart=Never \
     --image ghcr.io/christopherjmiller/patchbay-e2e-tools:e2e --image-pull-policy=Never \
     -- call 600 --server "$NODE_IP:5060" --user e2e-phone --password e2e-only-not-a-secret -v
   ```

   Subcommands: `register`, `call <number>`, `tftp get <file>`, `http get <url>`.
   `fakephone --help` documents the flags (each also reads `FAKEPHONE_*`).
5. **Iterate fast**: keep the cluster, change code or the chart, and rerun
   with `PATCHBAY_E2E_REUSE=1 PATCHBAY_E2E_KEEP=1`. Images are rebuilt and
   reloaded, and the pods roll when an image or the static config changed.
6. Run one test: `KUBECONFIG=$PWD/.e2e/kubeconfig PATCHBAY_E2E_REUSE=1
   PATCHBAY_E2E_KEEP=1 go test -tags e2e -count=1 -v -run TestEchoCall
   ./test/e2e/` (after `make e2e-images`).

## What kind proves, and what it hides

Findings from building this suite. They shape the agent and compiler phases.

- **Docker here is rootless.** Container IPs live inside rootlesskit's
  network namespace and cannot be reached from the host. kind works (the API
  server is published on 127.0.0.1), but nothing on the host can talk SIP or
  RTP to the cluster. So fakephone always runs as a Pod, never on the host.
- **Privileged ports.** Docker sets `net.ipv4.ip_unprivileged_port_start=0`
  in every container, kind's node included, so in plain kind any process on
  the host network may bind port 69. Talos keeps 1024. The harness restores
  1024, so kind behaves like the real node. Measured with a host-network pod
  binding UDP 69 on the node:
  - non-root, no capabilities: `permission denied`;
  - non-root with `capabilities.add: [NET_BIND_SERVICE]`: **still denied**
    (Kubernetes grants no ambient capabilities to non-root containers);
  - uid 0 with every capability dropped except `NET_BIND_SERVICE`: works.

  So when the agent serves TFTP on 69 it must run as uid 0 with only
  `NET_BIND_SERVICE` (and no privilege escalation, read-only root). File
  capabilities on the binary would not help, because
  `allowPrivilegeEscalation: false` sets no_new_privs, which ignores them.
- **RTP source address.** Asterisk binds RTP to 0.0.0.0 by default, so
  replies leave from whatever source the route picks. In kind that was the
  pod bridge's `10.244.0.1`, not the node IP advertised in SDP. The static
  config sets `media_address` and `bind_rtp_to_media_address = yes`, and the
  echo test asserts that RTP comes back from the node's IP. The compiler should
  do the same: `top` also carries the control-plane VIP, so do not leave the
  source to the routing table.
- **Pod network ↔ host network works both ways** in kind (kindnet): SIP over
  TCP and UDP to the node IP, RTP in both directions, and pod → ClusterIP
  Service → hostNetwork pod for the agent's control API.
- **Asterisk image (`andrius/asterisk` 22.11.0 trixie).**
  - The image's entrypoint runs as root, `chown -R`s the config and data
    directories, then drops privileges with `-U asterisk -p`. The chart
    bypasses it, running `/usr/sbin/asterisk -f -n -C /etc/asterisk/asterisk.conf`
    directly as uid 1000 with no capabilities and a read-only root
    filesystem. emptyDirs hold the control socket, astdb and spool, and
    `asterisk.conf` points `astdbdir`, `astspooldir` and `astcachedir` at
    them.
  - **The image ships no sound prompts** (`/var/lib/asterisk/sounds` is
    empty), and none for music on hold. Release A's 911 announcement and
    "not available" prompts need sound files from somewhere.
  - Every module the design names exists, including `res_audiosocket` and
    `app_audiosocket`. There is no `format_ulaw.so`: `format_pcm.so` handles
    `.ulaw`, `.alaw` and `.g722`.
  - `res_pjsip_notify` **declines to load** without a `pjsip_notify.conf`,
    so the compiler must always emit one.
  - With the compiler owning all of `/etc/asterisk`, Asterisk still logs
    missing `features.conf`, `indications.conf`, `acl.conf`, `udptl.conf`,
    `pjproject.conf`, `ccss.conf` and `stasis.conf`. They are harmless (it
    uses defaults), but `indications.conf` (ring and busy tones) is worth
    emitting.
- **`kind load` and digests.** See Images above.
