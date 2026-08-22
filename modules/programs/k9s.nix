{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.k9s.enable = lib.mkEnableOption "enables k9s" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.k9s.enable {
    home-manager.users.${config.nx.username} = {
      programs.k9s = {
        enable = true;
        package = pkgs.k9s;
        settings = {
          k9s = {
            liveViewAutoRefresh = false;
            refreshRate = 2;
            maxConnRetry = 5;
            readOnly = false;
            noExitOnCtrlC = true;
            ui = {
              enableMouse = false;
              headless = true;
              logoless = false;
              crumbsless = false;
              reactive = false;
              noIcons = false;
            };
            skipLatestRevCheck = false;
            disablePodCounting = false;
            shellPod = {
              image = "busybox:1.35.0";
              namespace = "default";
              limits = {
                cpu = "100m";
                memory = "100Mi";
              };
            };
            imageScans = {
              enable = false;
            };
            logger = {
              tail = 100;
              buffer = 5000;
              sinceSeconds = -1;
              textWrap = false;
              showTime = false;
            };
            thresholds = {
              cpu = {
                critical = 90;
                warn = 70;
              };
              memory = {
                critical = 90;
                warn = 70;
              };
            };
          };
        };
        aliases = {
          cr = "clusterroles";
          crb = "clusterrolebindings";
          dp = "deployments";
          es = "externalsecrets";
          hr = "HelmRelease";
          jo = "jobs";
          ks = "kustomizations";
          np = "networkpolicies";
          rb = "rolebindings";
          ro = "roles";
          sec = "v1/secrets";
        };
        views = {
          "v1/pods" = {
            sortColumn = "AGE:asc";
            columns = [
              "AGE"
              "NAMESPACE"
              "NAME"
              "PF"
              "READY"
              "RESTARTS"
              "STATUS"
              "%CPU/R"
              "%MEM/R"
              "NODE"
            ];
          };
          "v1/nodes" = {
            sortColumn = "NAME:asc";
            columns = [
              "AGE"
              "NAME"
              "STATUS"
              "ROLE"
              "VERSION"
              "PODS"
              "INTERNAL-IP"
            ];
          };
        };
        # Colours source from config.themev2 (issue #2814). v1 `grey1` (dim
        # text, no themev2 slot) maps to neutrals.foreground_dim; `primary` maps
        # to roles.primary.
        skins.skin = with config.themev2; {
          k9s = {
            body = {
              fgColor = neutrals.foreground;
              bgColor = neutrals.background_0;
              logoColor = hues.green;
            };
            prompt = {
              fgColor = neutrals.foreground;
              bgColor = neutrals.background_0;
              suggestColor = hues.orange;
            };
            info = {
              fgColor = neutrals.foreground_dim;
              sectionColor = hues.green;
            };
            dialog = {
              fgColor = neutrals.foreground;
              bgColor = neutrals.background_0;
              buttonFgColor = neutrals.foreground;
              buttonBgColor = hues.green;
              buttonFocusFgColor = neutrals.background_1;
              buttonFocusBgColor = hues.blue;
              labelFgColor = hues.orange;
              fieldFgColor = hues.blue;
            };
            frame = {
              border = {
                fgColor = hues.green;
                focusColor = hues.green;
              };
              menu = {
                fgColor = neutrals.foreground_dim;
                keyColor = hues.yellow;
                numKeyColor = hues.yellow;
              };
              crumbs = {
                fgColor = neutrals.background_1;
                bgColor = hues.green;
                activeColor = hues.yellow;
              };
              status = {
                newColor = hues.blue;
                modifyColor = hues.green;
                addColor = neutrals.foreground_dim;
                pendingColor = hues.orange;
                errorColor = hues.red;
                highlightColor = hues.yellow;
                killColor = hues.purple;
                completedColor = neutrals.foreground_dim;
              };
              title = {
                fgColor = hues.blue;
                bgColor = neutrals.background_0;
                highlightColor = hues.purple;
                counterColor = neutrals.foreground;
                filterColor = hues.blue;
              };
            };
            views = {
              charts = {
                bgColor = neutrals.background_0;
                defaultDialColors = [
                  hues.green
                  hues.red
                ];
                defaultChartColors = [
                  hues.green
                  hues.red
                ];
              };
              table = {
                fgColor = hues.yellow;
                bgColor = neutrals.background_0;
                cursorFgColor = neutrals.background_1;
                cursorBgColor = hues.blue;
                markColor = hues.yellow;
                header = {
                  fgColor = neutrals.foreground_dim;
                  bgColor = neutrals.background_0;
                  sorterColor = hues.orange;
                };
              };
              xray = {
                fgColor = hues.blue;
                bgColor = neutrals.background_0;
                cursorColor = neutrals.foreground;
                graphicColor = hues.yellow;
                showIcons = false;
              };
              yaml = {
                keyColor = hues.green;
                colonColor = neutrals.foreground_dim;
                valueColor = neutrals.foreground_dim;
              };
              logs = {
                fgColor = neutrals.foreground_dim;
                bgColor = neutrals.background_0;
                indicator = {
                  fgColor = hues.blue;
                  bgColor = neutrals.background_0;
                  toggleOnColor = roles.primary;
                  toggleOffColor = neutrals.foreground_dim;
                };
              };
              help = {
                fgColor = neutrals.foreground_dim;
                bgColor = neutrals.background_0;
                indicator = {
                  fgColor = hues.blue;
                };
              };
            };
          };
        };
      };
      programs.zsh.initContent =
        let
          initExtra = lib.mkOrder 1000 ''
            bindkey -s ^k "k9s\n"
          '';
        in
        lib.mkMerge [ initExtra ];
    };
  };
}
