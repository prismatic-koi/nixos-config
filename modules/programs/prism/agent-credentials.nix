{
  config,
  lib,
  pkgs,
  ...
}:
# Gives agent credentials to the agent sessions of a set of repos. Each
# credential is an entry of nx.programs.prism.agentCredentials. The prism Go
# source and the pi extension hold no name of a credential: a new credential
# needs a new entry of this option only. unifi.nix declares the first entry.
#
# Platform split mirrors modules/programs/home-automation.nix:
#   Linux  - sops-nix system secrets under /run/secrets/.
#   Darwin - home-manager sops secrets under ~/.config/sops-nix/secrets/.
let
  username = config.nx.username;
  cfg = config.nx.programs.prism.agentCredentials;
  isDarwin = pkgs.stdenv.hostPlatform.isDarwin;

  namePattern = "[A-Z][A-Z0-9_]*";
  credentialNamePattern = "[A-Za-z0-9][A-Za-z0-9_-]*";

  sopsSecretOptions = {
    sopsFile = lib.mkOption {
      type = lib.types.path;
      description = "The sops file that holds the secret.";
    };
    key = lib.mkOption {
      type = lib.types.str;
      description = ''
        The key of the secret in the sops file. It is also the name of the
        sops-nix secret, so the path is /run/secrets/<key> on Linux.
      '';
    };
  };

  credentialType = lib.types.submodule {
    options = {
      repos = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        example = [ "home-ops" ];
        description = ''
          The repo directory names whose agent sessions get the credential.
          An empty list gives the credential to no session, and declares no
          sops secret for it.
        '';
      };
      files = lib.mkOption {
        type = lib.types.attrsOf (
          lib.types.submodule {
            options = sopsSecretOptions // {
              marker = lib.mkOption {
                type = lib.types.strMatching namePattern;
                example = "EXAMPLE_PASSWORD";
                description = ''
                  The marker name. The redactor replaces the content of the
                  file with [redacted:<marker>].
                '';
              };
            };
          }
        );
        default = { };
        description = ''
          File variables, keyed by the name of the environment variable. The
          variable holds the path of the decrypted secret. No variable holds
          the content. The bwrap isolator binds the file read-only. The
          sandbox-exec profile admits a read of the file and no write. The
          content must hold 8 characters or more, so that the redactor can
          mask it.
        '';
      };
      values = lib.mkOption {
        type = lib.types.attrsOf (lib.types.submodule { options = sopsSecretOptions; });
        default = { };
        description = ''
          Value variables, keyed by the name of the environment variable. The
          variable holds the content of the decrypted secret. The redactor
          does not mask the value, so use a value variable only for a value
          that is not a credential, for example a host name. The value must be
          one token: not empty, with no space and no control character.
        '';
      };
      literals = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        example = {
          EXAMPLE_USERNAME = "agent";
        };
        description = ''
          Literal variables, keyed by the name of the environment variable.
          The variable holds the fixed string. The string must be one token.
        '';
      };
      skills = lib.mkOption {
        type = lib.types.listOf lib.types.path;
        default = [ ];
        description = ''
          Repo skills that each repo in repos gets, through
          nx.programs.prism.repoSkills. So the repo list is declared once.
        '';
      };
    };
  };

  enabled = lib.filterAttrs (_: c: c.repos != [ ]) cfg;

  # One entry for each variable of each credential, enabled or not.
  declarations = lib.concatLists (
    lib.mapAttrsToList (
      name: c:
      map (env: {
        inherit name env;
        kind = "files";
      }) (lib.attrNames c.files)
      ++ map (env: {
        inherit name env;
        kind = "values";
      }) (lib.attrNames c.values)
      ++ map (env: {
        inherit name env;
        kind = "literals";
      }) (lib.attrNames c.literals)
    ) cfg
  );

  describe = d: "${d.name} (${d.kind})";

  badCredentialNames = lib.filter (name: builtins.match credentialNamePattern name == null) (
    lib.attrNames cfg
  );
  badVariableNames = lib.filter (
    d: builtins.match namePattern d.env == null || lib.hasPrefix "PRISM_" d.env
  ) declarations;
  sharedVariableNames = lib.filterAttrs (_: ds: lib.length ds > 1) (
    lib.groupBy (d: d.env) declarations
  );

  errors =
    map (
      name: "the credential name \"${name}\" does not match ${credentialNamePattern}"
    ) badCredentialNames
    ++ map (
      d:
      "the variable name \"${d.env}\" of ${describe d} does not match ${namePattern}, or starts with PRISM_"
    ) badVariableNames
    ++ lib.mapAttrsToList (
      env: ds:
      "the variable ${env} is declared more than once: ${lib.concatMapStringsSep ", " describe ds}"
    ) sharedVariableNames;

  checked =
    if errors == [ ] then
      enabled
    else
      throw "nx.programs.prism.agentCredentials:\n  - ${lib.concatStringsSep "\n  - " errors}";

  # The sops secrets of the enabled credentials. Two credentials can use one
  # key, and another module can declare the same key, for example hass_domain
  # in modules/programs/home-automation.nix. The definitions then merge: the
  # sopsFile must be the same, and owner and mode are at default priority, so
  # a declaration in another module sets the final value.
  secrets = lib.concatLists (
    lib.mapAttrsToList (_: c: lib.attrValues c.files ++ lib.attrValues c.values) checked
  );

  secretPath =
    key:
    if isDarwin then
      config.home-manager.users.${username}.sops.secrets.${key}.path
    else
      config.sops.secrets.${key}.path;

  rendered = lib.mapAttrs (_: c: {
    inherit (c) repos literals;
    files = lib.mapAttrs (_: f: {
      path = secretPath f.key;
      inherit (f) marker;
    }) c.files;
    values = lib.mapAttrs (_: v: { path = secretPath v.key; }) c.values;
  }) checked;
in
{
  options.nx.programs.prism.agentCredentials = lib.mkOption {
    type = lib.types.attrsOf credentialType;
    default = { };
    description = ''
      Credentials that prism gives to the agent sessions of a set of repos.
      Each key is the name of a credential. A new credential needs a new
      entry of this option, and no change to the prism source.

      A session of a repo in the repos of a credential gets each variable of
      the credential: the file variables, the value variables, and the
      literal variables. When the credential has a file variable, the session
      also gets PRISM_CREDENTIAL_FILES, which lists each file variable and its
      marker. The redactor in the session reads this list, so the content of
      each file is masked as [redacted:<marker>] in tool results, in the
      frames on the prism socket, and in prism.db.

      The coordinator, the workers, and the investigators of the repo get the
      credential. The review agents do not get it.

      prism finds the repo on the host, with the repo identity of the
      project-trust decision. prism checks each credential at the start of a
      session. If a secret file is missing, if a file variable holds fewer
      than 8 characters, or if a value is not one token, the session gets no
      variable of that credential, and gets the other credentials. The
      agent-run log of the session names the credential and the cause.

      Two credentials must not declare one variable name. A variable name
      must match ${namePattern}, and must not start with PRISM_. If not,
      the evaluation fails.

      Limit of host mode: a host-mode session has no filesystem isolation.
      prism sets the variables only for the repos of the credential, but a
      host-mode session of any repo can read the sops path.

      A change takes effect after a home-manager switch, and then at the next
      start of a session. The option is rendered to
      ~/.config/prism/agent-credentials.json. Neither the bwrap nor the
      sandbox-exec isolator gives a sandbox access to that file.
    '';
  };

  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      (lib.mkIf (!isDarwin) {
        sops.secrets = lib.mkMerge (
          map (s: {
            ${s.key} = {
              owner = lib.mkDefault username;
              mode = lib.mkDefault "0400";
              inherit (s) sopsFile;
            };
          }) secrets
        );
      })

      (lib.mkIf isDarwin {
        home-manager.users.${username}.sops.secrets = lib.mkMerge (
          map (s: {
            ${s.key} = {
              mode = lib.mkDefault "0400";
              inherit (s) sopsFile;
            };
          }) secrets
        );
      })

      {
        nx.programs.prism.repoSkills = lib.mkMerge (
          lib.mapAttrsToList (_: c: lib.genAttrs c.repos (_: c.skills)) (
            lib.filterAttrs (_: c: c.skills != [ ]) checked
          )
        );

        # ~/.config/prism/ is persisted on impermanence systems by
        # profiles.nix, so this module declares the file alone.
        home-manager.users.${username}.xdg.configFile."prism/agent-credentials.json".text =
          builtins.toJSON rendered;
      }
    ]
  );
}
