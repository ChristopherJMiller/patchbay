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
```

It is scoped to the hardware in one house (Cisco CP-7945G/7965G/7975G desk
phones on SIP firmware, and Linphone softphones), LAN only, with no outside
line. The design, including the per-device state machines, is
[docs/design.md](docs/design.md).

## Status

**Pre-alpha.** The foundation is in place: the `v1alpha1` API and its CRDs, the
contracts the compiler, drivers and agent implement, the controller manager,
the agent's control API skeleton, and the CLI's shape. The compiler, the
drivers' state machines, the agent's TFTP/AMI work and the Helm chart's
workloads are not written yet. Nothing here runs a phone today.

Planned releases: **A** core calling, **B** live audio streams, **C** phone
extras (wallpaper, ringtones, speed dials, a Services menu, push messages).

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
make image                     # ghcr.io/christopherjmiller/patchbay:dev
```

`make e2e` (kind + real Asterisk + a simulated phone) is coming in the next
stage.

## Layout

```
cmd/patchbay/          one binary: controller | agent | stream-bridge | CLI
api/v1alpha1/          resource types -> generated CRDs
internal/compile/      pure: resources + settings -> config bundle
internal/drivers/      the driver contract; cisco79x5 and softphone drivers to come
internal/agent/        the PBX pod's sidecar (control API today)
internal/controller/   the controller manager and, soon, its reconcilers
charts/patchbay/       Helm chart (CRDs today)
config/samples/        one example of each resource
docs/                  the design and the research behind it
```

## Firmware

Cisco firmware is never committed to this repository, encrypted or not. It is
staged onto the PBX's firmware volume with `patchbay firmware load <zip>`,
checked against the SHA-256 manifest in the driver. See
[docs/research/](docs/research/).

## License

Apache License 2.0. See [LICENSE](LICENSE).
