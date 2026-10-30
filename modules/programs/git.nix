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

  # A git wrapper for a typed `git status` or `git diff`. A worker can stage a
  # submodule and set `ignore` in .gitmodules, which overrides
  # diff.ignoreSubmodules = all from the env. Only --ignore-submodules on the
  # command line has priority over that key. The wrapper adds it to those two
  # subcommands, so git does not descend into the submodule git dir and run a
  # planted filter (#3343). It steps over the global options of git to find
  # the subcommand, and passes every other subcommand through unchanged. A
  # help or version request passes through too, so `git status --help` and
  # `git diff --help` still open the man page. The zsh git function and
  # fugitive's g:fugitive_git_executable both use it.
  submoduleGuard = pkgs.writeShellScript "git-submodule-guard" ''
    args=("$@")
    n=''${#args[@]}
    i=0
    while [ "$i" -lt "$n" ]; do
      case "''${args[i]}" in
        -h | --help | -v | --version)
          exec ${pkgs.git}/bin/git "''${args[@]}" ;;
        -C | -c | --git-dir | --work-tree | --namespace | --config-env | --attr-source)
          i=$((i + 2)) ;;
        -*)
          i=$((i + 1)) ;;
        *)
          break ;;
      esac
    done
    if [ "$i" -lt "$n" ] &&
      { [ "''${args[i]}" = status ] || [ "''${args[i]}" = diff ]; } &&
      [ "''${args[i + 1]:-}" != --help ] &&
      [ "''${args[i + 1]:-}" != -h ]; then
      exec ${pkgs.git}/bin/git "''${args[@]:0:i+1}" --ignore-submodules=all "''${args[@]:i+1}"
    fi
    exec ${pkgs.git}/bin/git "''${args[@]}"
  '';

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
  # turns off the hooks that config defines with that event name. It does not
  # stop a config hook with a friendly name: a planted hook.<event>.command
  # makes the event name a friendly name, and a hook with another name for
  # the same event still runs (hook.c, git 2.56). The write-protection fix is
  # tracked by #3334. The owner uses no git hooks, so this breaks nothing.
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
    # Auto maintenance runs git gc, which runs each value of
    # gc.recentObjectsHook. No config key turns that hook off, so turn off
    # auto maintenance itself (#3355).
    {
      key = "maintenance.auto";
      value = "false";
    }
    # A local-path remote URL or insteadOf runs the hooks and the
    # uploadpack/receivepack of that repo on a fetch or push. The owner uses
    # only SSH remotes, so refuse the file transport (#3355).
    {
      key = "protocol.file.allow";
      value = "never";
    }
    # A planted http.cookieFile with http.saveCookies makes curl write a file
    # at a path the config chooses, on an HTTPS fetch. These pins close the
    # form with no URL. A URL-scoped http.<url>.cookieFile wins by urlmatch
    # and stays a known limit (user-git-overrides.md) (#3355).
    {
      key = "http.cookieFile";
      value = "/dev/null";
    }
    {
      key = "http.saveCookies";
      value = "false";
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
    # The git submodule guard, so the prism nvim config (vim-fugitive.nix) can
    # use the same wrapper that the shell git function uses (#3343).
    nx.programs.git.submoduleGuard = lib.mkOption {
      type = lib.types.path;
      internal = true;
      readOnly = true;
      default = submoduleGuard;
      description = "Wrapper that adds --ignore-submodules=all to a typed git status or git diff (#3343).";
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

          # A typed `git status` or `git diff` at the root of a worker
          # worktree can descend into a staged submodule and run a planted
          # filter, because `ignore` in .gitmodules overrides
          # diff.ignoreSubmodules = all (#3343). Route the interactive git
          # through the guard, which adds --ignore-submodules=all to those two
          # subcommands. The guard runs the real git for every other command.
          # nvim and the shell prompt have their own controls
          # (user-git-overrides.md).
          programs.zsh.initContent = lib.mkAfter ''
            git() { "${submoduleGuard}" "$@"; }
          '';
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
