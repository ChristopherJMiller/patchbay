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

        # Piper text to speech (piper-tts 1.8.0, GPL-3.0) for the sound prompts'
        # Piper voices (hack/gen-sounds.sh builds it with `nix build .#piper`).
        # It runs only at build time and the image carries only its output, so
        # its licence does not reach the image (docs/sounds.md). Without the
        # training, alignment, HTTP and Japanese extras: nixpkgs' default pulls
        # in torch and friends, a 3.6 GiB closure, where this one is about
        # 1 GiB. Not in the dev shell, so `nix develop` never fetches it. The
        # import check names piper.train, which is left out.
        piper =
          (pkgs.piper-tts.override {
            withAlignment = false;
            withHTTP = false;
            withJapanese = false;
            withTrain = false;
          }).overridePythonAttrs
            (_: {
              pythonImportsCheck = [ "piper" ];
            });

        # The Piper voices the image may carry, from rhasspy/piper-voices at a
        # pinned commit, each file pinned by its sha256 (fixed-output, so
        # fetched once and only when the prompts are rendered). One directory,
        # en_US-<voice>-medium.onnx beside its .onnx.json (piper reads the
        # config from <model>.json). The list is the allowlist in
        # internal/agent/sounds (Voices) and hack/gen-sounds.sh, less espeak:
        # public domain voices only, trained from scratch or fine-tuned from
        # one of these. Never lessac or a voice fine-tuned from it (its
        # dataset's licence is research-only); docs/sounds.md has the
        # licences.
        piper-voices =
          let
            rev = "c10ece1aade47bb51c153c893d14e5bf8e5b7117";
            voices = {
              ljspeech = {
                onnx = "sha256-b1KnUeI0mr56dnNesJ3Bh1KYx36iNC/9L+95/4G4fyI=";
                json = "sha256-FB1hLMCpXtfvwcqTa4RcI2SWfy6SF8Xb/PafxNbGWGA=";
              };
              kristin = {
                onnx = "sha256-WEmVf5Kcv3IMJY+EWGktYQP/8vDj07GcgllHS7BqGNQ=";
                json = "sha256-VoFCbUrq0iGV3nBTHu7t20ZJPPr/xXZLLqPbc0KLZRw=";
              };
              norman = {
                onnx = "sha256-uXOUQyMqgKWcfRiBDdhWiZvxanlkcl9auB6kmxNRy3E=";
                json = "sha256-bC239VikqN65/oIlg8HFEF9sToNN0PneitF6iI7p/h0=";
              };
              john = {
                onnx = "sha256-eJxsh1cm5ifd7pPVHYcnhZq+nAkcPRQVkfS4PCBy6Yg=";
                json = "sha256-r2Dxd7a1UPPXowJyDA+4nn+UqCtdykZHde9jscaboJo=";
              };
            };
            file =
              voice: ext: hash:
              let
                name = "en_US-${voice}-medium.${ext}";
              in
              {
                inherit name;
                path = pkgs.fetchurl {
                  url = "https://huggingface.co/rhasspy/piper-voices/resolve/${rev}/en/en_US/${voice}/medium/${name}";
                  inherit hash;
                };
              };
          in
          pkgs.linkFarm "piper-voices" (
            pkgs.lib.concatLists (
              pkgs.lib.mapAttrsToList (voice: h: [
                (file voice "onnx" h.onnx)
                (file voice "onnx.json" h.json)
              ]) voices
            )
          );
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
