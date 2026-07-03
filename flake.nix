# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers
{
  description = "koryph — multi-project orchestrator for autonomous Claude Code agents";

  inputs = {
    # Determinate Systems' weekly nixpkgs carries the tool versions this
    # project pins: go 1.26.x, goreleaser 2.16+, mkdocs-material 9.7.x, syft,
    # cosign, reuse. flake.lock freezes the exact revision for reproducibility.
    nixpkgs.url = "https://flakehub.com/f/DeterminateSystems/nixpkgs-weekly/0.1.tar.gz";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f system (import nixpkgs { inherit system; }));

      # nixpkgs still packages go 1.26.3, which govulncheck flags for two
      # reachable stdlib vulns (GO-2026-5037 crypto/x509, GO-2026-5039
      # net/textproto) fixed in 1.26.4. Wrap the official prebuilt 1.26.4
      # toolchain until nixpkgs catches up (then delete this and use pkgs.go).
      goVersion = "1.26.4";
      goPlatform = {
        aarch64-darwin = "darwin-arm64";
        x86_64-darwin = "darwin-amd64";
        aarch64-linux = "linux-arm64";
        x86_64-linux = "linux-amd64";
      };
      goSha256 = {
        aarch64-darwin = "b62ad2b6d7d2464f12a5bcad7ff47f19d08325773b5efd21610e445a05a9bf53";
        x86_64-darwin = "05dc9b5f9997744520aaebb3d5deaa7c755371aebbfb7f97c2511a9f3367538d";
        aarch64-linux = "ef758ae7c6cf9267c9c0ef080b8965f453d89ab2d25d9eb22de4405925238768";
        x86_64-linux = "1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f";
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
          # mkdocs + Material theme for the docs book (make docs-build / docs-serve).
          docs = pkgs.python3.withPackages (ps: with ps; [ mkdocs mkdocs-material ]);
        in
        {
          default = pkgs.mkShell {
            name = "koryph-dev";

            packages = [
              # Go toolchain — official prebuilt 1.26.4; keep go.mod in sync.
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
