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
        readonly property color red: "${theme.red}"
        readonly property color orange: "${theme.orange}"
        readonly property color yellow: "${theme.yellow}"
        readonly property color green: "${theme.green}"
        readonly property color aqua: "${theme.aqua}"
        readonly property color blue: "${theme.blue}"
        readonly property color purple: "${theme.purple}"

        // text colors
        readonly property color foreground: "${theme.foreground}"
        readonly property color primary: "${theme.primary}"
        readonly property color secondary: "${theme.secondary}"

        // grey scale
        readonly property color grey0: "${theme.grey0}"
        readonly property color grey1: "${theme.grey1}"
        readonly property color grey2: "${theme.grey2}"

        // backgrounds
        readonly property color bgDim: "${theme.bg_dim}"
        readonly property color bg0: "${theme.bg0}"
        readonly property color bg1: "${theme.bg1}"
        readonly property color bg2: "${theme.bg2}"
        readonly property color bg3: "${theme.bg3}"
        readonly property color bg4: "${theme.bg4}"
        readonly property color bg5: "${theme.bg5}"
        readonly property color bgVisual: "${theme.bg_visual}"
        readonly property color bgRed: "${theme.bg_red}"
        readonly property color bgGreen: "${theme.bg_green}"
        readonly property color bgBlue: "${theme.bg_blue}"
        readonly property color bgYellow: "${theme.bg_yellow}"

        // statusline
        readonly property color statusline1: "${theme.statusline1}"
        readonly property color statusline2: "${theme.statusline2}"
        readonly property color statusline3: "${theme.statusline3}"

        // metadata
        readonly property string themeName: "${theme.name}"
        readonly property string themeType: "${theme.type}"
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
        # add the generated theme singleton
        cp ${themeQml} $out/Theme.qml
        # QML requires a qmldir to register singletons and custom types
        cat > $out/qmldir <<'EOF'
    singleton Theme Theme.qml
    SubMapOverlay 1.0 SubMapOverlay.qml
    WorkspaceBar 1.0 WorkspaceBar.qml
    NowPlaying 1.0 NowPlaying.qml
    EOF
  '';
in
{
  options = {
    nx.desktop.quickshell.enable = lib.mkEnableOption "enables quickshell overlays" // {
      default = true;
    };
  };
  config = lib.mkIf (config.nx.desktop.quickshell.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben =
      {
        lib,
        ...
      }:
      {
        home = {
          packages = [
            pkgs.quickshell
          ];
          # Deploy quickshell config as real files (not symlinks into the nix store).
          # Quickshell watches config files with inotify including IN_ATTRIB.
          # When auto-optimise-store deduplicates files during `nix build`, the
          # hard-link count changes on store inodes, triggering spurious reloads.
          # Copying to a mutable directory avoids this.
          activation.quickshellConfig = lib.hm.dag.entryAfter [ "writeBoundary" ] ''
            config_dir="$HOME/.config/quickshell/shell"
            run mkdir -p "$config_dir"
            run rm -f "$config_dir"/*.qml "$config_dir"/qmldir
            for f in ${quickshellConfig}/*; do
              run cp "$f" "$config_dir/$(basename "$f")"
            done
          '';
        };
      };
  };
}
