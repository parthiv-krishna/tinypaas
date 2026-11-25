{
  description = "tinypaas: run tiny web apps as binaries behind one OIDC-protected domain";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      templateAppFor =
        pkgs:
        import ./nix/template-app.nix {
          inherit pkgs;
          tinypaas = self.packages.${pkgs.stdenv.hostPlatform.system}.tinypaas;
        };
    in
    {
      packages = forAllSystems (pkgs: {
        tinypaas = pkgs.callPackage ./nix/package.nix { };
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.tinypaas;
      });

      # services.tinypaas, defaulting to this flake's package.
      nixosModules.default =
        { lib, pkgs, ... }:
        {
          imports = [ ./nix/module.nix ];
          services.tinypaas.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.tinypaas;
        };

      checks = forAllSystems (pkgs: {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.tinypaas;
        template-app = templateAppFor pkgs;
        vm = import ./nix/test.nix {
          inherit pkgs;
          module = self.nixosModules.default;
          templateApp = templateAppFor pkgs;
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
          ];
          env.CGO_ENABLED = "0";
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
