# CI, dependency updates and releases

## What runs, and when

Everything in `.github/workflows/ci.yml` runs inside the repo's flake
(`nix develop -c make …`), so a red job reproduces locally with the same
`make` target.

| Job | What it proves | Runs on |
|---|---|---|
| `check` | `make generate` is committed, `go mod tidy`, hygiene (no binaries, firmware or secrets, gitleaks over all history), actionlint, govulncheck, golangci-lint, unit tests + envtest, reproducible sound prompts, every compile golden in the pinned Asterisk image (`make asterisk-check`) | every push, PR, weekly |
| `e2e` | the kind suite (`make e2e`, docs/e2e.md) | every push, PR, weekly |
| `image` | the image builds for amd64 and arm64; publishes on main and `v*` tags only | after `check` + `e2e` |
| `release` | chart + GitHub release + Helm index | `v*` tags only, after `image` |
| `ci-ok` | all of `check`, `e2e` and `image` succeeded | always (the required check) |

`renovate-config.yml` validates `renovate.json` whenever it changes.

The weekly run (Mondays 06:17 UTC, on main) catches what changes without a
commit of ours: a new vulnerability report, a pinned image digest that
disappeared, a runner image that moved.

## The main ruleset

`main` has one repository ruleset:

- the status check **`ci-ok`** is required, and the branch must be up to
  date with main before merging;
- no force pushes, no deletion;
- **repository admins bypass it**, so a maintainer can still push straight
  to main (that push gets the same CI run afterwards).

Everything else (Renovate, any other contributor) lands through a PR whose
`ci-ok` passed against current main.

## Renovate

Renovate (the Mend-hosted GitHub app) reads `renovate.json`.

- **Merges itself:** every non-major update (Go modules, GitHub Actions,
  pinned image digests and tags, the Asterisk 22.x patch line, the monthly
  nixpkgs `flake.lock` bump) and every vulnerability fix. Renovate turns on
  GitHub's auto-merge when it opens the PR, so GitHub merges it the moment
  `ci-ok` passes, and Renovate rebases a PR that fell behind main first.
- **Waits for a human:** major updates (label `major`), and anything whose
  `ci-ok` failed.
- **Never proposed:** a new Go minor (flake and Dockerfile move together, by
  hand), Asterisk past 22 (a design change), our own image, kind node images.
- Go modules and actions wait 3 days after a release, so a bad release can
  be yanked first.

The Dependency Dashboard issue lists everything pending.

## Images

`ghcr.io/christopherjmiller/patchbay`, amd64 and arm64, built from the
`Dockerfile` with the prompts `make sounds` renders.

| Event | Tags |
|---|---|
| push to main | `sha-<7>`, `main` |
| tag `v1.2.3` | `1.2.3`, `1.2`, `sha-<7>` |

Deploy by tag **and** digest (`…:sha-abc1234@sha256:…`): the job summary of
each publishing run prints that pin. Each pushed image carries an SBOM, a
BuildKit provenance record and a signed GitHub build-provenance
attestation:

```sh
gh attestation verify oci://ghcr.io/christopherjmiller/patchbay@sha256:<digest> \
  --repo ChristopherJMiller/patchbay
```

## Cutting a release

1. On main, with `ci-ok` green: `git tag -a v0.1.0 -m "patchbay 0.1.0" && git push origin v0.1.0`
   (a `-rc.N` suffix makes a pre-release).
2. CI runs the whole suite on the tag, publishes the image `0.1.0`, then
   packages the chart with version and appVersion `0.1.0`, creates the
   GitHub release `v0.1.0` with the chart attached and generated notes, and
   adds it to the Helm index on `gh-pages`.
3. Install from the repository:

   ```sh
   helm repo add patchbay https://christopherjmiller.github.io/patchbay
   helm install patchbay patchbay/patchbay --version 0.1.0 -f values.yaml
   ```

The chart's default image tag is its appVersion, so a released chart
always points at the image built from the same tag. `Chart.yaml` in git
keeps a placeholder version; the tag sets the real one.
