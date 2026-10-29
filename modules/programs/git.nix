{
  config,
  lib,
  pkgs,
  ...
}:
let
  username = config.nx.username;
  homeDir = config.home-manager.users.${username}.home.homeDirectory;
  signingKey = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey.pub";
  allowedSignersFile = "${homeDir}/.ssh/allowed_signers";

  # Config that every git process of the user gets in its environment. A
  # sandboxed prism session can write a repo config that git on the host
  # reads, and environment config has priority over every config file. The
  # tests, the limits, and where the variables reach are in
  # modules/programs/prism/prism/docs/invariants/user-git-overrides.md.
  #
  # environment.d cannot hold an empty value, and git stops with "missing
  # config value" when one GIT_CONFIG_VALUE_<n> is absent. So the systemd and
  # launchd environments get only the entries that have a value.
  # Every hook event of githooks(5) on git 2.56. git 2.56 can define a hook
  # in config (hook.<name>.command with hook.<name>.event), which
  # core.hooksPath = /dev/null does not stop. hook.<event>.enabled = false
  # stops the config hooks of that event, even when the repo config sets it
  # to true. The owner uses no git hooks, so this breaks nothing.
  hookEvents = [
    "applypatch-msg"
    "pre-applypatch"
    "post-applypatch"
    "pre-commit"
    "pre-merge-commit"
    "prepare-commit-msg"
    "commit-msg"
    "post-commit"
    "pre-rebase"
    "post-checkout"
    "post-merge"
    "pre-push"
    "pre-receive"
    "update"
    "proc-receive"
    "post-receive"
    "post-update"
    "reference-transaction"
    "push-to-checkout"
    "pre-auto-gc"
    "post-rewrite"
    "sendemail-validate"
    "fsmonitor-watchman"
    "p4-changelist"
    "p4-prepare-changelist"
    "p4-post-changelist"
    "p4-pre-submit"
    "post-index-change"
  ];

  gitConfigOverrides = [
    # Keys that run a program: turn them off.
    {
      key = "core.fsmonitor";
      value = "false";
    }
    {
      key = "core.hooksPath";
      value = "/dev/null";
    }
    {
      key = "core.pager";
      value = "cat";
    }
    {
      key = "core.editor";
      value = ":";
    }
    {
      key = "sequence.editor";
      value = ":";
    }
    {
      key = "core.askPass";
      value = "";
    }
    {
      key = "core.alternateRefsCommand";
      value = "";
    }
    {
      key = "gpg.ssh.defaultKeyCommand";
      value = "";
    }
    {
      key = "protocol.ext.allow";
      value = "never";
    }
    # diff.external is not here. git has no value that means "use the builtin
    # diff": it runs any value, and an empty value makes each git diff that
    # shows a patch fail. A planted diff.external on a typed git diff (and
    # any --ext-diff variant) is a known limit, in
    # docs/invariants/user-git-overrides.md. Bare git show and git log -p do
    # not trigger it. The prompt and gitsigns use --no-ext-diff, so they are
    # not affected.
    # interactive.diffFilter runs on git add -p, restore -p, checkout -p,
    # and reset -p. An empty value breaks add -p, so pin it to cat, which
    # passes the diff through unchanged.
    {
      key = "interactive.diffFilter";
      value = "cat";
    }
    # A repo config can name a global attributes file, which selects a
    # driver.
    {
      key = "core.attributesFile";
      value = "/dev/null";
    }
    # Keys that the owner uses: pin them to the values of this module, or to
    # the git default.
    {
      key = "core.sshCommand";
      value = "ssh";
    }
    {
      key = "credential.helper";
      value = "";
    }
    {
      key = "gpg.format";
      value = "ssh";
    }
    {
      key = "gpg.ssh.program";
      value = "ssh-keygen";
    }
    {
      key = "user.signingKey";
      value = signingKey;
    }
    {
      key = "gpg.ssh.allowedSignersFile";
      value = allowedSignersFile;
    }
    {
      key = "gpg.program";
      value = "gpg";
    }
    {
      key = "gpg.openpgp.program";
      value = "gpg";
    }
    {
      key = "gpg.x509.program";
      value = "gpgsm";
    }
    {
      key = "diff.ignoreSubmodules";
      value = "all";
    }
    {
      key = "status.submoduleSummary";
      value = "false";
    }
    {
      key = "submodule.recurse";
      value = "false";
    }
    {
      key = "fetch.recurseSubmodules";
      value = "false";
    }
    {
      key = "push.recurseSubmodules";
      value = "no";
    }
  ]
  ++ map (event: {
    key = "hook.${event}.enabled";
    value = "false";
  }) hookEvents;

  gitEnvOverrides = {
    # GIT_ATTR_SOURCE, which would make git read no .gitattributes file, is
    # not here. On git 2.56 it makes `git apply` (and so `git restore -p`
    # and `git checkout -p`) stop with a segmentation fault, and it does not
    # cover $GIT_DIR/info/attributes. A planted .gitattributes driver is a
    # known limit, in docs/invariants/user-git-overrides.md.
    #
    # A partial-clone config makes git status, diff, and blame fetch a
    # missing object, and the fetch runs remote.<name>.uploadpack.
    GIT_NO_LAZY_FETCH = "1";
    # pager.<command> has priority over core.pager. GIT_PAGER has priority
    # over both.
    GIT_PAGER = "cat";
    # git uses the first core.gitProxy that matches, so a later config value
    # cannot replace a repo value. A set GIT_PROXY_COMMAND makes git skip the
    # config, and an empty one means no proxy.
    GIT_PROXY_COMMAND = "";
  };

  toGitConfigEnv =
    overrides:
    {
      GIT_CONFIG_COUNT = toString (lib.length overrides);
    }
    // lib.listToAttrs (
      lib.concatLists (
        lib.imap0 (i: o: [
          (lib.nameValuePair "GIT_CONFIG_KEY_${toString i}" o.key)
          (lib.nameValuePair "GIT_CONFIG_VALUE_${toString i}" o.value)
        ]) overrides
      )
    );

  shellGitEnv = toGitConfigEnv gitConfigOverrides // gitEnvOverrides;
  serviceGitEnv =
    toGitConfigEnv (lib.filter (o: o.value != "") gitConfigOverrides)
    // lib.filterAttrs (_: v: v != "") gitEnvOverrides;
in
{
  options = {
    nx.programs.git.enable = lib.mkEnableOption "enables git" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.git.enable (
    let
      isLinux = pkgs.stdenv.hostPlatform.isLinux;
      isDarwin = pkgs.stdenv.hostPlatform.isDarwin;
    in
    lib.mkMerge [
      # Common git and ssh configuration for both platforms
      {
        home-manager.users.${username} = {
          programs.ssh = {
            enable = true;
            settings = {
              "github.com" = {
                User = "git";
                HostName = "github.com";
                Port = 22;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
              "gitlab.com" = {
                User = "git";
                HostName = "gitlab.com";
                Port = 22;
                IdentityFile = "${homeDir}/.ssh/prismatic-koi-ed25519";
              };
            };
          };
          programs.git = {
            enable = true;
            includes = [
              {
                contents = {
                  user = {
                    name = "prismatic-koi";
                    email = "ben@tinfoilforest.nz";
                    inherit signingKey;
                  };
                  push = {
                    autoSetupRemote = true;
                  };
                  init = {
                    defaultBranch = "main";
                  };
                  gpg = {
                    format = "ssh";
                    "ssh" = {
                      inherit allowedSignersFile;
                    };
                  };
                  commit = {
                    gpgsign = true;
                  };
                  # Reset the system-level credential helper list. nixpkgs' Darwin
                  # git ships /nix/store/.../etc/gitconfig with
                  # credential.helper=osxkeychain baked in, which fails with
                  # "fatal: failed to store: 100001" in headless / agent
                  # contexts where the keychain is unavailable. Per
                  # gitcredentials(7), setting credential.helper to the empty
                  # string resets the helper list; SSH auth remains the working
                  # path and no replacement helper is needed.
                  credential = {
                    helper = "";
                  };
                  # git uses a bare repo only when --git-dir or GIT_DIR names
                  # it. A repo config cannot change this key.
                  safe = {
                    bareRepository = "explicit";
                  };
                };
              }
            ];
          };
          home.packages = with pkgs; [
            gh
          ];

          # zsh sources this file from .zshenv, for login and non-login
          # shells. tmux removes the source-once guard from its server
          # environment, so each new pane sources it again.
          home.sessionVariables = shellGitEnv;
        };
      }

      # Linux: the systemd user manager, for the units and for the tmux server
      # that prism-restore starts.
      (lib.mkIf isLinux {
        home-manager.users.${username}.systemd.user.sessionVariables = serviceGitEnv;
      })

      # Darwin: the launchd user domain, for the agents and for the apps that
      # launchd starts. launchctl setenv acts only on processes that start
      # after it, so this agent runs at each login.
      (lib.mkIf isDarwin {
        home-manager.users.${username}.launchd.agents.git-env-overrides = {
          enable = true;
          config = {
            ProgramArguments = [
              "/bin/sh"
              "-c"
              (lib.concatStringsSep "\n" (
                lib.mapAttrsToList (
                  name: value: "/bin/launchctl setenv ${name} ${lib.escapeShellArg value}"
                ) serviceGitEnv
              ))
            ];
            RunAtLoad = true;
          };
        };
      })

      # Linux: sops secrets via system config
      (lib.mkIf isLinux {
        sops.secrets = {
          "ssh/prismatic-koi-ed25519-signingkey" = {
            owner = username;
            mode = "0600";
            path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey";
            sopsFile = ./secrets/ssh.sops.yaml;
          };
          "ssh/prismatic-koi-ed25519-signingkey.pub" = {
            owner = username;
            mode = "0600";
            path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey.pub";
            sopsFile = ./secrets/ssh.sops.yaml;
          };
          "github_token" = {
            owner = username;
            mode = "0600";
            sopsFile = ./secrets/github.sops.yaml;
          };
        };

        # Linux-only: system activation scripts (for impermanence)
        system.activationScripts.signingKeysFolderPermissions = ''
          mkdir -p ${homeDir}/.ssh
          chown ${username}:users ${homeDir}/.ssh
        '';

        # Environment variables for GitHub token
        environment.sessionVariables = {
          GITHUB_TOKEN = "$(cat ${config.sops.secrets.github_token.path})";
          GITHUB_PACKAGES_TOKEN = "$(cat ${config.sops.secrets.github_token.path})";
        };
      })

      # Darwin: sops secrets via home-manager
      (lib.mkIf isDarwin {
        home-manager.users.${username} = {
          sops.secrets = {
            "ssh/prismatic-koi-ed25519-signingkey" = {
              path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey";
              sopsFile = ./secrets/ssh.sops.yaml;
            };
            "ssh/prismatic-koi-ed25519-signingkey.pub" = {
              path = "${homeDir}/.ssh/prismatic-koi-ed25519-signingkey.pub";
              sopsFile = ./secrets/ssh.sops.yaml;
            };
            "github_token" = {
              sopsFile = ./secrets/github.sops.yaml;
            };
          };

          # Environment variables for GitHub token
          home.sessionVariables = {
            GITHUB_TOKEN = "$(cat ${config.home-manager.users.${username}.sops.secrets.github_token.path})";
            GITHUB_PACKAGES_TOKEN = "$(cat ${
              config.home-manager.users.${username}.sops.secrets.github_token.path
            })";
          };
        };
      })
    ]
  );
}
