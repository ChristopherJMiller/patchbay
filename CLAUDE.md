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
        agent      native sidecar: applies bundles, serves TFTP/HTTP, scrapes
                   phones, talks AMI. Senses and acts; never decides.
        asterisk   SIP 5060, RTP 10000-10099
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

Contracts: `internal/compile` (Bundle, Inputs, Settings), `internal/drivers`
(Driver, Device, Memory, Observations, Decision, Profile) and
`internal/agent/api` (the control API's JSON). Change them deliberately: the
controller, agent, drivers and e2e fake phone all build on them.

## Commands

Everything runs in the flake's shell. `go`, `kind`, `helm` and the rest are not
on the global PATH.

```sh
cd /path/to/patchbay && nix develop -c make <target>
make generate     # after ANY api/v1alpha1 change; commit the generated files
make fmt vet lint # golangci-lint v2 (.golangci.yml) + helm lint
make test         # unit + envtest (KUBEBUILDER_ASSETS comes from the flake)
make test-unit    # -short: skips envtest
make build image  # bin/patchbay; ghcr.io/christopherjmiller/patchbay:dev
make tools-versions
make e2e          # kind + chart + real Asterisk + fakephone; docs/e2e.md
                  # PATCHBAY_E2E_KEEP=1 keeps the cluster, PATCHBAY_E2E_REUSE=1 reuses it
make e2e-down     # delete the e2e cluster
```

The CRDs are generated into `charts/patchbay/templates/crds/` and carry
`helm.sh/resource-policy: keep`. Never edit them, or `zz_generated.*`, by hand.

## Hard rules

1. **Toolchain from the flake only.** Bump nixpkgs with `nix flake update`, then
   run `make tools-versions` and `make test` before committing the lock. Keep
   the kube-apiserver minor (envtest) in step with the `k8s.io/*` minor in
   `go.mod`, and the Go minor in step with the Dockerfile's builder.
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
   only a manifest of names and SHA-256s. `.gitignore` blocks `firmware/`,
   `*.zip`, `*.sbn`, `*.loads` and `*.secret*`; do not work around it. Test
   fixtures are generated, not copied from a real phone or Cisco.
5. **Pinned images.** Every image is pinned by digest (tag plus digest, for
   example `golang:1.26.8-trixie@sha256:...`), including test images.
6. **Git hygiene.** Small, logical commits. Stage explicit paths, never
   `git add -A` or `git add .` (other agents may be writing files). Never
   bypass hooks. Do not create remotes, push, or open GitHub repos unless
   Chris asks.
7. **Evidence before claims.** Never say something works without running it,
   and report failures and deviations from the spec plainly.
