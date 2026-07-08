# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
{
  description = "koryph — multi-project orchestrator for autonomous Claude Code agents";

  inputs = {
    # Determinate Systems' weekly nixpkgs carries the tool versions this
    # project pins: go 1.26.x, goreleaser 2.16+, zensical 0.0.4x, syft,
    # cosign, reuse. flake.lock freezes the exact revision for reproducibility.
    nixpkgs.url = "https://flakehub.com/f/DeterminateSystems/nixpkgs-weekly/0.1.tar.gz";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f system (import nixpkgs { inherit system; }));

      # nixpkgs lags the patched Go releases this project needs: 1.26.5 fixes
      # GO-2026-5856 (crypto/tls Encrypted Client Hello privacy leak) that
      # govulncheck flags on 1.26.4. Wrap the official prebuilt 1.26.5 toolchain
      # until nixpkgs catches up (then delete this and use pkgs.go). Keep in sync
      # with go.mod's `go` directive and .github/workflows/ci.yml's setup-go pin.
      goVersion = "1.26.5";
      goPlatform = {
        aarch64-darwin = "darwin-arm64";
        x86_64-darwin = "darwin-amd64";
        aarch64-linux = "linux-arm64";
        x86_64-linux = "linux-amd64";
      };
      goSha256 = {
        aarch64-darwin = "efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a";
        x86_64-darwin = "6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf1ef725";
        aarch64-linux = "fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49";
        x86_64-linux = "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053";
      };
      goFor = system: pkgs:
        pkgs.stdenv.mkDerivation {
          pname = "go";
          version = goVersion;
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go${goVersion}.${goPlatform.${system}}.tar.gz";
            sha256 = goSha256.${system};
          };
          nativeBuildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.autoPatchelfHook ];
          buildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.stdenv.cc.cc.lib ];
          # Prebuilt toolchain: keep binaries as shipped (do not strip/re-sign).
          dontStrip = true;
          installPhase = ''
            runHook preInstall
            mkdir -p $out
            cp -r . $out/
            runHook postInstall
          '';
        };
    in
    {
      devShells = forAllSystems (system: pkgs:
        let
          go = goFor system pkgs;
          # zensical for the docs book (make docs-build / docs-serve); reads
          # the existing mkdocs.yml (compat mode, Material successor).
          docs = pkgs.zensical;
        in
        {
          default = pkgs.mkShell {
            name = "koryph-dev";

            packages = [
              # Go toolchain — official prebuilt 1.26.5; keep go.mod in sync.
              go
            ] ++ (with pkgs; [
              gopls
              gotools

              # Supply-chain / release tooling (q35, 8uk, vgv epics).
              syft
              goreleaser
              cosign
              reuse

              # Documentation book.
              docs

              # Dev workflow.
              gnumake
              git
              gh
              pre-commit
              jq
            ]);

            shellHook = ''
              # Use exactly the pinned Go — never silently fetch a toolchain.
              export GOTOOLCHAIN=local
              echo "koryph dev shell — $(go version | cut -d' ' -f3), tools pinned via flake.nix"
              echo "  note: bd (beads) is not in nixpkgs; install it separately."
            '';
          };
        });
    };
}
