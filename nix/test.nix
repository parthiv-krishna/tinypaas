# NixOS VM test: deploy an app over the control socket and reach it through
# the router.
{
  pkgs,
  module,
  templateApp,
}:
let
  echoapp = pkgs.buildGoModule {
    pname = "echoapp";
    version = "0";
    src = ../internal/paas/testdata/echoapp;
    vendorHash = null;
    env.CGO_ENABLED = 0;
    postPatch = "printf 'module echoapp\\ngo 1.22\\n' > go.mod";
  };
  manifest = pkgs.writeText "app.json" (
    builtins.toJSON {
      name = "echo";
      title = "Echo";
      description = "test app";
    }
  );
in
pkgs.testers.nixosTest {
  name = "tinypaas";
  nodes.machine = {
    imports = [ module ];
    services.tinypaas = {
      enable = true;
      publicUrl = "https://apps.example.com";
      noAuth = true;
    };
    users.users.deployer = {
      isNormalUser = true;
      extraGroups = [ "tinypaas" ];
    };
    users.users.outsider.isNormalUser = true;
    environment.systemPackages = [ pkgs.curl ];
  };

  testScript = ''
    machine.wait_for_unit("tinypaas.service")
    machine.wait_for_open_port(8090)

    # Only members of the tinypaas group may use the control socket.
    machine.fail("su outsider -c 'tinypaas ls'")
    machine.succeed("su deployer -c 'cd /tmp && cp ${manifest} app.json && tinypaas deploy ${echoapp}/bin/echoapp'")

    out = machine.succeed("curl -sf http://127.0.0.1:8090/echo/hi")
    assert '"path":"/hi"' in out, out
    assert '"user":"dev"' in out, out
    assert '"cwd":"/var/lib/tinypaas/data/echo"' in out, out
    assert "Echo" in machine.succeed("curl -sf http://127.0.0.1:8090/")
    assert "echo" in machine.succeed("su deployer -c 'tinypaas ls'")

    # Apps come back after a daemon restart.
    machine.succeed("systemctl restart tinypaas.service")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8090/echo/hi")
    machine.succeed("test -f /var/lib/tinypaas/data/echo/app.db")

    # The scaffolded app (Go + SQLite) works end to end under the service sandbox.
    machine.succeed("su deployer -c 'tinypaas deploy -manifest ${templateApp.manifest} ${templateApp}/bin/app'")
    machine.succeed("curl -sf -X POST -d body=hello-from-vm http://127.0.0.1:8090/template-app/notes -o /dev/null")
    page = machine.succeed("curl -sfL http://127.0.0.1:8090/template-app/")
    assert "hello-from-vm" in page and 'href="static/style.css"' in page, page
    machine.succeed("curl -sf http://127.0.0.1:8090/template-app/static/style.css")
    machine.succeed("test -f /var/lib/tinypaas/data/template-app/app.db")
    machine.fail("su outsider -c 'cat /var/lib/tinypaas/data/template-app/app.db'")
  '';
}
