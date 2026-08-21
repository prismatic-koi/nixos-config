# themev2 truecolor swatch preview. Wired as the `theme-preview` flake app:
#   nix run .#theme-preview              # all sample schemes
#   nix run .#theme-preview -- <scheme>  # one named scheme
#
# Each swatch block shows the slot name, the hex value and the provenance tag
# (upstream / derived / adjusted) with a 24-bit ANSI colour block. The swatch
# is the visual divergence register. Renders exact hex in a truecolor
# terminal (e.g. kitty).
{
  lib,
  runCommand,
  writeShellApplication,
}:
let
  colourLib = import ../lib.nix;
  palette = import ./palette.nix { inherit colourLib; };
  inherit (palette) schemes groups;

  # One data line per slot: "slot|hex|provenance|method". Group titles are
  # emitted as "##<title>" marker lines the renderer turns into headers.
  mkLine =
    scheme: group: slot:
    let
      e = scheme.${group}.${slot};
    in
    "${slot}|${e.value}|${e.provenance}|${e.method}";

  mkSchemeData =
    scheme:
    lib.concatStringsSep "\n" (
      lib.concatMap (g: [ "##${g.title}" ] ++ map (mkLine scheme g.group) g.slots) groups
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
      local slot hex prov method r g b swatch
      while IFS='|' read -r slot hex prov method; do
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
        printf '  %s  %-15s %s  %-9s %s\n' "$swatch" "$slot" "$hex" "$prov" "$method"
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
