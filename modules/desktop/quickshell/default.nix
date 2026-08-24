{
  config,
  pkgs,
  lib,
  ...
}:
let
  theme = config.theme;

  # Theme.qml is generated at build time with the active color scheme.
  # QML components access colors via the Theme singleton, e.g. Theme.red
  #
  # Colours now come from config.theme (issue #2814). The QML property names
  # are unchanged so the QML consumers need no edits; only the Nix source of
  # each value moves onto the theme bands. Gap mappings (no direct theme
  # slot) follow the qutebrowser/greasemonkey precedent:
  #   aqua        -> hues.teal            (nearest hue; exact on edge/github-light/gruvbox/nightcity, cyan exact on latte/onedark; teal keeps firefox+qutebrowser consistent)
  #   grey0       -> neutrals.background_5 (structural grey)
  #   grey1       -> neutrals.foreground_dim (dim text)
  #   grey2       -> neutrals.foreground
  #   statusline1 -> hues.indigo
  #   statusline2 -> hues.yellow            (warm; v1 gold #df8e1d == theme yellow on latte)
  #   statusline3 -> hues.red
  #
  # Property names use lowerCamelCase (QML convention):
  #   bg_dim  -> bgDim
  #   bg0     -> bg0    (no change needed)
  #   grey0   -> grey0  (no change needed)
  themeQml = pkgs.writeText "Theme.qml" ''
    pragma Singleton
    import QtQuick

    // Auto-generated from NixOS theme: ${theme.name}
    // Do not edit — this file is built by modules/desktop/quickshell/default.nix
    QtObject {
        // accent colors
        readonly property color red: "${theme.hues.red}"
        readonly property color orange: "${theme.hues.orange}"
        readonly property color yellow: "${theme.hues.yellow}"
        readonly property color green: "${theme.hues.green}"
        readonly property color aqua: "${theme.hues.teal}"
        readonly property color blue: "${theme.hues.blue}"
        readonly property color purple: "${theme.hues.purple}"

        // text colors
        readonly property color foreground: "${theme.neutrals.foreground}"
        readonly property color primary: "${theme.roles.primary}"
        readonly property color secondary: "${theme.roles.secondary}"

        // grey scale
        readonly property color grey0: "${theme.neutrals.background_5}"
        readonly property color grey1: "${theme.neutrals.foreground_dim}"
        readonly property color grey2: "${theme.neutrals.foreground}"

        // backgrounds
        readonly property color bgDim: "${theme.neutrals.background_dim}"
        readonly property color bg0: "${theme.neutrals.background_0}"
        readonly property color bg1: "${theme.neutrals.background_1}"
        readonly property color bg2: "${theme.neutrals.background_2}"
        readonly property color bg3: "${theme.neutrals.background_3}"
        readonly property color bg4: "${theme.neutrals.background_4}"
        readonly property color bg5: "${theme.neutrals.background_5}"
        readonly property color bgVisual: "${theme.backgrounds.bg_visual}"
        readonly property color bgRed: "${theme.backgrounds.bg_red}"
        readonly property color bgGreen: "${theme.backgrounds.bg_green}"
        readonly property color bgBlue: "${theme.backgrounds.bg_blue}"
        readonly property color bgYellow: "${theme.backgrounds.bg_yellow}"

        // statusline (no theme slot; see gap mapping above)
        readonly property color statusline1: "${theme.hues.indigo}"
        readonly property color statusline2: "${theme.hues.yellow}"
        readonly property color statusline3: "${theme.hues.red}"

        // metadata
        readonly property string themeName: "${theme.name}"
        readonly property string themeType: "${theme.type}"
        readonly property string deviceLocation: "${config.nx.deviceLocation}"
        readonly property bool externalAudio: ${
          if config.nx.externalAudio.enable then "true" else "false"
        }
    }
  '';

  # Build the quickshell config directory as a single derivation.
  # This ensures all QML files are siblings in the same directory,
  # which is required for QML type resolution to work.
  quickshellConfig = pkgs.runCommand "quickshell-config" { } ''
        mkdir -p $out
        # copy static QML files from the repo
        cp ${./config/shell.qml} $out/shell.qml
        cp ${./config/SubMapOverlay.qml} $out/SubMapOverlay.qml
        cp ${./config/WorkspaceBar.qml} $out/WorkspaceBar.qml
        cp ${./config/NowPlaying.qml} $out/NowPlaying.qml
        cp ${./config/WindowTitle.qml} $out/WindowTitle.qml
        cp ${./config/Environment.qml} $out/Environment.qml
        cp ${./config/StatusBar.qml} $out/StatusBar.qml
        cp ${./config/OsdOverlay.qml} $out/OsdOverlay.qml
        cp ${./config/SystemStatus.qml} $out/SystemStatus.qml
        # add the generated theme singleton
        cp ${themeQml} $out/Theme.qml
        # QML requires a qmldir to register singletons and custom types
        cat > $out/qmldir <<'EOF'
    singleton Theme Theme.qml
    SubMapOverlay 1.0 SubMapOverlay.qml
    WorkspaceBar 1.0 WorkspaceBar.qml
    NowPlaying 1.0 NowPlaying.qml
    WindowTitle 1.0 WindowTitle.qml
    Environment 1.0 Environment.qml
    StatusBar 1.0 StatusBar.qml
    OsdOverlay 1.0 OsdOverlay.qml
    SystemStatus 1.0 SystemStatus.qml
    EOF
  '';
in
{
  options = {
    nx.desktop.quickshell.enable = lib.mkEnableOption "enables quickshell overlays" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.desktop.quickshell.enable && pkgs.stdenv.hostPlatform.isLinux) {
    home-manager.users.${config.nx.username} = {
      programs.quickshell = {
        enable = true;
        configs.shell = quickshellConfig;
        activeConfig = "shell";
        systemd.enable = true;
      };

      # The HM quickshell module generates: quickshell --config shell
      # which resolves ~/.config/quickshell/shell at runtime — a symlink
      # that HM activation updates to the new store path on every switch.
      # We only need to ensure the service is restarted after activation so
      # it picks up the new config. X-Restart-Triggers causes systemd to
      # restart the unit whenever the trigger value changes, which here is
      # the quickshellConfig store path (changes on every rebuild).
      systemd.user.services.quickshell = {
        Unit.X-Restart-Triggers = [ "${quickshellConfig}" ];
        Service.Environment = [
          # Workaround for Qt QML GC crash (QTBUG-134687): setting QV4_GC_TIMELIMIT=0
          # disables the incremental GC time limit, preventing SIGSEGV in QV4::MarkStack::drain
          # during rapid window open/close events.
          "QV4_GC_TIMELIMIT=0"
          # Quickshell watches config file inodes directly; nix builds can trigger
          # spurious hot-reloads via store hardlink deduplication even without a
          # switch. Suppress the popup since it's misleading in that context.
          "QS_NO_RELOAD_POPUP=1"
        ];
      };
    };
  };
}
