#!/usr/bin/env bash
# The toolchain lockstep of CLAUDE.md's hard rule 1, checked rather than
# remembered: Renovate's monthly flake.lock bump merges itself once CI is
# green, and envtest tolerates one minor of skew, so a nixpkgs that moved
# kubernetes (or Go) a minor ahead would otherwise pass unnoticed.
#
#   - envtest's kube-apiserver minor (nixpkgs' kubernetes, flake.nix) must
#     be the k8s.io/* minor in go.mod (v0.N is Kubernetes 1.N);
#   - the flake's Go minor must be the Dockerfile builder's.
#
# Run inside `nix develop` (make test runs it first).
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
die() {
	echo "check-versions: $*" >&2
	fail=1
}

: "${KUBEBUILDER_ASSETS:?KUBEBUILDER_ASSETS is not set: run inside nix develop}"

mod=$(awk '$1 == "k8s.io/api" { print $2; exit }' go.mod)
[[ "$mod" =~ ^v0\.([0-9]+)\. ]] || die "go.mod: no k8s.io/api v0.N requirement (got '$mod')"
mod_minor=${BASH_REMATCH[1]:-}

api=$("$KUBEBUILDER_ASSETS/kube-apiserver" --version)
[[ "$api" =~ v1\.([0-9]+)\. ]] || die "kube-apiserver --version: '$api'"
api_minor=${BASH_REMATCH[1]:-}

if [[ -n "$mod_minor" && -n "$api_minor" && "$mod_minor" != "$api_minor" ]]; then
	die "envtest's kube-apiserver is 1.$api_minor ('$api') but go.mod has k8s.io/api $mod (1.$mod_minor): move them together (flake.lock or go.mod)"
fi

flake_go=$(go env GOVERSION)
[[ "$flake_go" =~ ^go1\.([0-9]+) ]] || die "go env GOVERSION: '$flake_go'"
flake_minor=${BASH_REMATCH[1]:-}

docker_go=$(sed -n 's/^FROM .*golang:\([0-9][0-9.]*\)-.*/\1/p' Dockerfile)
[[ "$docker_go" =~ ^1\.([0-9]+) ]] || die "Dockerfile: no golang:<version> builder (got '$docker_go')"
docker_minor=${BASH_REMATCH[1]:-}

if [[ -n "$flake_minor" && -n "$docker_minor" && "$flake_minor" != "$docker_minor" ]]; then
	die "the flake's Go is $flake_go but the Dockerfile builds with golang:$docker_go: move them together"
fi

if ((fail)); then
	exit 1
fi
echo "check-versions: kube-apiserver 1.$api_minor = k8s.io/api $mod; Go $flake_go = golang:$docker_go (minors)"
