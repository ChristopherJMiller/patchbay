# CI, dependency updates and releases

## What runs, and when

Everything in `.github/workflows/ci.yml` runs inside the repo's flake
(`nix develop -c make …`), so a red job reproduces locally with the same
`make` target.

| Job | What it proves | Runs on |
|---|---|---|
| `check` | `make generate` is committed, `go mod tidy`, hygiene (no binaries, firmware or secrets, gitleaks over all history), actionlint, govulncheck, golangci-lint, unit tests + envtest, reproducible sound prompts, every compile golden in the pinned Asterisk image (`make asterisk-check`) | every push, PR, weekly |
| `e2e` | the kind suite (`make e2e`, docs/e2e.md) | every push, PR, weekly |
| `image` | the image builds for amd64 and arm64 (pushes nothing; read-only token) | after `check` + `e2e` |
| `publish` | pushes the image and its attestation to ghcr.io, each primary tag once | pushes to main and `v*` tags only, after `image` |
| `release` | chart (image pinned by publish's digest) + GitHub release + Helm index | `v*` tags only, after `publish` |
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
  pinned image digests and tags but Asterisk's, the monthly nixpkgs
  `flake.lock` bump) and every vulnerability fix. Renovate turns on
  GitHub's auto-merge when it opens the PR, so GitHub merges it the moment
  `ci-ok` passes, and Renovate rebases a PR that fell behind main first.
- **Waits for a human:** major updates (label `major`), and anything whose
  `ci-ok` failed.
- **Never proposed:** a new Go minor (flake and Dockerfile move together, by
  hand), Asterisk past 22 (a design change), our own image, kind node images.
- Go modules, actions and images (tags and digests) wait 3 days after a
  release, so a bad release can be yanked first.
- **The Asterisk image never merges itself** (`andrius/asterisk`, a
  community image that runs on the host network and reads every SIP
  password): a person reads the upstream change and the new digest. A
  digest that changes behind an unchanged tag is the signal to look for.

The Dependency Dashboard issue lists everything pending.

## Images

`ghcr.io/christopherjmiller/patchbay`, amd64 and arm64, built from the
`Dockerfile` with the prompts `make sounds` renders.

| Event | Tags |
|---|---|
| push to main | `sha-<7>`, `main` |
| tag `v1.2.3` | `1.2.3`, `1.2` |

Only the `publish` job can push, and only on those two events: PR runs
(Renovate's included) build with a read-only token. Each primary tag
(`sha-<7>` on main, `1.2.3` on a tag) is written once: a re-run of the job
finds it and reuses its digest instead of pushing another build under the
same name (`main` and `1.2` move). The binary is stamped with the commit
time, not the build time.

Deploy by tag **and** digest (`…:sha-abc1234@sha256:…`): the job summary of
each publishing run prints that pin. A released chart already does: it is
packaged with `image.digest` set to the digest `publish` pushed for its
tag. Each pushed image carries an SBOM, a
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

The chart's default image tag is its appVersion and its digest is the one
published for that tag, so a released chart always runs exactly the image
built from the same tag, even if the tag were pushed again. `Chart.yaml` in git
keeps a placeholder version; the tag sets the real one.
