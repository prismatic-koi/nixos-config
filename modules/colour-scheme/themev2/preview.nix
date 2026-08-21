# themev2 truecolor swatch preview. Wired as the `theme-preview` flake app:
#   nix run .#theme-preview              # all sample schemes
#   nix run .#theme-preview -- <scheme>  # one named scheme
#
# Output is grouped (neutrals / brights / hues / tinted backgrounds / roles).
# Each block is just a 24-bit ANSI colour swatch and the slot name. Renders
# exact hex in a truecolor terminal (e.g. kitty). Provenance lives in the
# per-scheme files as inline comments, not here.
{
  lib,
  runCommand,
  writeShellApplication,
}:
let
  colourLib = import ../lib.nix;
  schemes = {
    edge = import ./edge.nix { inherit colourLib; };
    everforest = import ./everforest.nix { inherit colourLib; };
    catppuccin-latte = import ./catppuccin-latte.nix { inherit colourLib; };
  };

  # Display order the preview walks. Group titles are printed as headers.
  groups = [
    {
      title = "Neutrals";
      band = "neutrals";
      slots = [
        "background_darkest"
        "background_dark"
        "background"
        "surface"
        "overlay"
        "muted"
        "foreground_dim"
        "foreground"
      ];
    }
    {
      title = "Brights";
      band = "brights";
      slots = [
        "bright_red"
        "bright_orange"
        "bright_yellow"
        "bright_green"
        "bright_cyan"
        "bright_blue"
        "bright_magenta"
        "bright_brown"
      ];
    }
    {
      title = "Hues";
      band = "hues";
      slots = [
        "red"
        "orange"
        "amber"
        "yellow"
        "lime"
        "green"
        "emerald"
        "teal"
        "cyan"
        "sky"
        "blue"
        "indigo"
        "violet"
        "purple"
        "fuchsia"
        "pink"
        "rose"
        "brown"
      ];
    }
    {
      title = "Tinted backgrounds";
      band = "backgrounds";
      slots = [
        "bg_red"
        "bg_green"
        "bg_blue"
        "bg_yellow"
        "bg_visual"
      ];
    }
    {
      title = "Roles";
      band = "roles";
      slots = [
        "primary"
        "secondary"
        "error"
        "warning"
        "success"
        "info"
        "selection"
        "cursor"
        "border"
      ];
    }
  ];

  # One data line per slot: "slot|hex". Group titles become "##<title>" marker
  # lines the renderer turns into headers.
  mkSchemeData =
    scheme:
    lib.concatStringsSep "\n" (
      lib.concatMap (
        g: [ "##${g.title}" ] ++ map (slot: "${slot}|${scheme.${g.band}.${slot}}") g.slots
      ) groups
    );

  # A file per scheme, named <scheme>.dat. An unknown scheme has no file, so
  # the renderer errors and exits non-zero.
  dataDir = runCommand "theme-preview-data" { } (
    ''
      mkdir -p "$out"
    ''
    + lib.concatStrings (
      lib.mapAttrsToList (name: scheme: ''
        cat > "$out/${name}.dat" <<'DATAEOF'
        ${mkSchemeData scheme}
        DATAEOF
      '') schemes
    )
  );

  known = lib.concatStringsSep " " (lib.attrNames schemes);
in
writeShellApplication {
  name = "theme-preview";
  runtimeInputs = [ ];
  text = ''
    DATADIR="${dataDir}"
    KNOWN="${known}"

    render_scheme() {
      local scheme="$1"
      local file="$DATADIR/$scheme.dat"
      if [ ! -f "$file" ]; then
        printf 'error: unknown scheme %s\n' "$scheme" >&2
        printf 'known schemes: %s\n' "$KNOWN" >&2
        return 1
      fi
      printf '\n\033[1m=== %s ===\033[0m\n' "$scheme"
      local slot hex r g b swatch
      while IFS='|' read -r slot hex; do
        [ -z "$slot" ] && continue
        case "$slot" in
          '##'*)
            printf '\n  \033[4m%s\033[0m\n' "''${slot#\#\#}"
            continue
            ;;
        esac
        r=$((16#''${hex:1:2}))
        g=$((16#''${hex:3:2}))
        b=$((16#''${hex:5:2}))
        swatch=$(printf '\033[48;2;%d;%d;%dm      \033[0m' "$r" "$g" "$b")
        printf '  %s  %s\n' "$swatch" "$slot"
      done < "$file"
    }

    if [ "$#" -eq 0 ]; then
      for s in $KNOWN; do
        render_scheme "$s" || exit 1
      done
    else
      render_scheme "$1" || exit 1
    fi
    printf '\n'
  '';
}
