{
  config,
  pkgs,
  lib,
  ...
}:
let
  isLinux = pkgs.stdenv.hostPlatform.isLinux;
  t = config.theme;
  n = t.neutrals;
  # Calibre derives dark mode from Window/Base. Base uses the recessed shade,
  # so it stays on the same side of the scale as Window on dark and light themes.
  # Highlight uses the selection role; HighlightedText uses the primary text colour.
  palette = {
    WindowText = n.foreground;
    Text = n.foreground;
    ButtonText = n.foreground;
    ToolTipText = n.foreground;
    PlaceholderText = n.foreground_dim;
    BrightText = t.roles.error;
    HighlightedText = n.foreground;
    Window = n.background_0;
    Base = n.background_dim;
    Button = n.background_2;
    AlternateBase = n.background_1;
    ToolTipBase = n.background_1;
    Highlight = t.roles.selection;
    Accent = t.roles.primary;
    Link = t.roles.info;
    LinkVisited = t.roles.secondary;
  };
  viewerScheme = {
    name = t.name;
    foreground = n.foreground;
    background = n.background_0;
    link = t.roles.info;
  };
  themeJson = pkgs.writeText "calibre-theme.json" (
    builtins.toJSON {
      inherit (t) type name;
      inherit palette viewerScheme;
    }
  );
  # Merge a jq filter into a JSON file in place. Args: file, filter.
  # A missing file starts as {}. An invalid file is left alone with a warning.
  mergeFn = ''
    _calibre_merge() {
      _file="$1"
      _filter="$2"
      _dir="$(dirname "$_file")"
      mkdir -p "$_dir"
      if [ -e "$_file" ]; then
        if ! ${pkgs.jq}/bin/jq -e . "$_file" >/dev/null 2>&1; then
          echo "warning: calibre: $_file is not valid JSON, leaving it unchanged" >&2
          return 0
        fi
        _src="$_file"
      else
        _src=""
      fi
      _tmp="$(mktemp "$_dir/.theme.XXXXXX")" || return 0
      if [ -n "$_src" ]; then
        _in="$_src"
      else
        _in="$(mktemp)" && echo '{}' > "$_in"
      fi
      if ${pkgs.jq}/bin/jq --slurpfile theme ${themeJson} "$_filter" "$_in" > "$_tmp" 2>/dev/null; then
        if [ -n "$_src" ]; then
          chmod --reference="$_src" "$_tmp" 2>/dev/null || chmod 600 "$_tmp"
        else
          chmod 600 "$_tmp"
        fi
        mv "$_tmp" "$_file"
      else
        echo "warning: calibre: could not update $_file" >&2
        rm -f "$_tmp"
      fi
      [ -z "$_src" ] && rm -f "$_in"
      return 0
    }
  '';
  guiFilter = ''
    $theme[0] as $t
    | .color_palette = $t.type
    | .[$t.type + "_palette_name"] = "__current__"
    | .[$t.type + "_palettes"] = ((.[$t.type + "_palettes"] // {}) | .["__current__"] = $t.palette)
  '';
  viewerFilter = ''
    $theme[0] as $t
    | ("*" + $t.name) as $k
    | .session_data = ((.session_data // {})
        | .user_color_schemes = ((.user_color_schemes // {}) | .[$k] = $t.viewerScheme)
        | .current_color_scheme = $k)
  '';
in
{
  options = {
    nx.programs.calibre.enable = lib.mkEnableOption "enables calibre" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.calibre.enable {
    home-manager.users.${config.nx.username} =
      { lib, ... }:
      {
        home.packages = [
          pkgs.calibre
        ];
        home.persistence."/persist" = {
          directories = [
            ".config/calibre"
          ];
        };
        # Merge theme keys into calibre's JSON settings. ~/.config/calibre is a
        # bind mount from /persist, made at local-fs.target, before home-manager
        # activation runs, so the writes land in the persisted directory.
        home.activation.calibreTheme = lib.mkIf isLinux (
          lib.hm.dag.entryAfter [ "writeBoundary" ] ''
            ${mergeFn}
            _calibre_merge "$HOME/.config/calibre/gui.json" ${lib.escapeShellArg guiFilter}
            _calibre_merge "$HOME/.config/calibre/viewer-webengine.json" ${lib.escapeShellArg viewerFilter}
          ''
        );
        xdg.mimeApps.defaultApplications = lib.mkIf isLinux {
          "application/epub+zip" = "calibre-ebook-viewer.desktop";
        };
      };
  };
}
