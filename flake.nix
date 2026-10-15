{
  description = "patchbay: a Kubernetes-native PBX (Asterisk 22 + a Go operator) - dev toolchain";

  # nixos-unstable, pinned by flake.lock. Bump with `nix flake update nixpkgs`,
  # then run `nix develop -c make tools-versions` and `make test` before
  # committing the new lock. The Go toolchain is pinned to a minor (go_1_26)
  # so a nixpkgs bump cannot silently move the module's language version.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: {
        # envtest control-plane binaries, straight from nixpkgs so tests never
        # download anything (no setup-envtest). controller-runtime's envtest
        # finds them through KUBEBUILDER_ASSETS. Keep the kube-apiserver minor
        # in step with the k8s.io/* minor in go.mod (both 1.37 today).
        envtest-assets = pkgs.linkFarm "envtest-assets-${pkgs.kubernetes.version}" [
          {
            name = "kube-apiserver";
            path = "${pkgs.kubernetes}/bin/kube-apiserver";
          }
          {
            name = "etcd";
            path = "${pkgs.etcd}/bin/etcd";
          }
          {
            name = "kubectl";
            path = "${pkgs.kubectl}/bin/kubectl";
          }
        ];
      });

      devShells = forAllSystems (
        pkgs:
        let
          system = pkgs.stdenv.hostPlatform.system;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              # Go
              go_1_26
              gopls
              gotools
              golangci-lint
              kubernetes-controller-tools # controller-gen
              govulncheck # make vulncheck

              # Kubernetes / e2e
              kind
              kubectl
              kubernetes-helm
              docker-client

              # Plumbing
              gnumake
              git
              jq
              yq-go
              actionlint
              gitleaks # make check-hygiene: no secrets in the public history

              # Telephony test tools (simulated phones, TFTP checks, test tones)
              sipp
              tftp-hpa
              ffmpeg-headless

              # The sound prompts (make sounds, hack/gen-sounds.sh): text to
              # speech with espeak-ng's own voices, converted by ffmpeg.
              espeak-ng
            ];

            KUBEBUILDER_ASSETS = "${self.packages.${system}.envtest-assets}";
            # Never let `go` fetch a different toolchain behind the flake's back.
            GOTOOLCHAIN = "local";

            shellHook = ''
              # e2e kind clusters keep their kubeconfig here, never in ~/.kube/config.
              export PATCHBAY_E2E_KUBECONFIG="$(git rev-parse --show-toplevel 2>/dev/null || pwd)/.e2e/kubeconfig"
            '';
          };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
