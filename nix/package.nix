{ lib, buildGoModule }:
buildGoModule {
  pname = "tinypaas";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  vendorHash = "sha256-2m/dfVcwPqb+Hi2lSTHKqpoooYSROfEciGDoTle/MLs=";
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Run tiny web apps as binaries behind one OIDC-protected domain";
    mainProgram = "tinypaas";
    platforms = lib.platforms.linux;
  };
}
