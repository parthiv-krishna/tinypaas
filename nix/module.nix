{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.tinypaas;
  inherit (lib) mkOption types;
in
{
  options.services.tinypaas = {
    enable = lib.mkEnableOption "tinypaas, a tiny binary-based app host";

    package = mkOption {
      type = types.package;
      description = "The tinypaas package.";
    };

    listenAddress = mkOption {
      type = types.str;
      default = "127.0.0.1";
      description = "Address for the HTTP listener. Put a TLS reverse proxy in front of it.";
    };

    port = mkOption {
      type = types.port;
      default = 8090;
      description = "Port for the HTTP listener.";
    };

    publicUrl = mkOption {
      type = types.str;
      example = "https://apps.example.com";
      description = "Public base URL the apps are served under.";
    };

    portBase = mkOption {
      type = types.port;
      default = 20000;
      description = "First localhost port assigned to apps (one per app, counting up).";
    };

    extraPackages = mkOption {
      type = types.listOf types.package;
      default = [ ];
      description = "Packages put on the apps' PATH.";
    };

    noAuth = mkOption {
      type = types.bool;
      default = false;
      description = "DEVELOPMENT ONLY: disable login and treat every visitor as user `dev`.";
    };

    oidc = {
      issuer = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "https://login.example.com";
        description = "OIDC issuer URL. The redirect URI to register is `<publicUrl>/_tinypaas/callback`.";
      };
      clientIdFile = mkOption {
        type = types.nullOr types.path;
        default = null;
        description = "File containing the OIDC client id.";
      };
      clientSecretFile = mkOption {
        type = types.nullOr types.path;
        default = null;
        description = "File containing the OIDC client secret.";
      };
      allowedGroups = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "Groups allowed to sign in. Empty allows any authenticated user.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion =
          cfg.noAuth
          || (cfg.oidc.issuer != null && cfg.oidc.clientIdFile != null && cfg.oidc.clientSecretFile != null);
        message = "services.tinypaas: set oidc.issuer, oidc.clientIdFile and oidc.clientSecretFile (or noAuth).";
      }
    ];

    environment.systemPackages = [ cfg.package ];

    users.users.tinypaas = {
      isSystemUser = true;
      group = "tinypaas";
      home = "/var/lib/tinypaas";
    };
    users.groups.tinypaas = { };

    systemd.services.tinypaas = {
      description = "tinypaas app host";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      path = cfg.extraPackages;

      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "serve"
            "--state-dir"
            "/var/lib/tinypaas"
            "--socket"
            "/run/tinypaas/control.sock"
            "--listen"
            "${cfg.listenAddress}:${toString cfg.port}"
            "--public-url"
            cfg.publicUrl
            "--port-base"
            (toString cfg.portBase)
          ]
          ++ (
            if cfg.noAuth then
              [ "--no-auth" ]
            else
              [
                "--oidc-issuer"
                cfg.oidc.issuer
                "--oidc-client-id-file"
                "%d/oidc-client-id"
                "--oidc-client-secret-file"
                "%d/oidc-client-secret"
                "--allowed-groups"
                (lib.concatStringsSep "," cfg.oidc.allowedGroups)
              ]
          )
        );
        LoadCredential = lib.optionals (!cfg.noAuth) [
          "oidc-client-id:${cfg.oidc.clientIdFile}"
          "oidc-client-secret:${cfg.oidc.clientSecretFile}"
        ];

        User = "tinypaas";
        Group = "tinypaas";
        UMask = "0027";
        StateDirectory = "tinypaas";
        StateDirectoryMode = "0750";
        RuntimeDirectory = "tinypaas";
        RuntimeDirectoryMode = "0750";
        WorkingDirectory = "/var/lib/tinypaas";

        Restart = "always";
        RestartSec = 2;
        # tinypaas stops its apps gracefully; systemd mops up stragglers.
        KillMode = "mixed";
        TimeoutStopSec = 30;

        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        RestrictSUIDSGID = true;
        RestrictRealtime = true;
        RestrictNamespaces = true;
        LockPersonality = true;
        CapabilityBoundingSet = "";
        SystemCallArchitectures = "native";
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
      };
    };
  };
}
