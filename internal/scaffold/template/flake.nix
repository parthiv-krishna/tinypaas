{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      # Builds result/bin/app: a static binary deployed with `tinypaas deploy`.
      packages = forAllSystems (pkgs: {
        default = pkgs.buildGoModule {
          pname = "[[.Name]]";
          version = "0";
          src = ./.;
          # Update after changing go.mod: set to pkgs.lib.fakeHash, run
          # `nix build`, and paste the "got:" hash here.
          vendorHash = "sha256-yb9ons1/UroR33G4qvWTaBUS2LsYcdHWsEhAZHJQ2Kw=";
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
          ];
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            sqlite
          ];
          env.CGO_ENABLED = "0";
        };
      });
    };
}
