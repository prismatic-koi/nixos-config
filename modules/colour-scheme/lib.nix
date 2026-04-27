# Colour manipulation utilities for "#RRGGBB" hex strings.
# Pure Nix — no module system args. Import directly:
#
#   let colourLib = import ./lib.nix;
#   in colourLib.darken config.theme.bg_green 15
#
# pct is an integer percentage, e.g. 15 = 15%.
let
  hexDigits = {
    "0" = 0;
    "1" = 1;
    "2" = 2;
    "3" = 3;
    "4" = 4;
    "5" = 5;
    "6" = 6;
    "7" = 7;
    "8" = 8;
    "9" = 9;
    "a" = 10;
    "b" = 11;
    "c" = 12;
    "d" = 13;
    "e" = 14;
    "f" = 15;
    "A" = 10;
    "B" = 11;
    "C" = 12;
    "D" = 13;
    "E" = 14;
    "F" = 15;
  };

  decDigits = [
    "0"
    "1"
    "2"
    "3"
    "4"
    "5"
    "6"
    "7"
    "8"
    "9"
    "a"
    "b"
    "c"
    "d"
    "e"
    "f"
  ];

  # Parse a two-character hex pair (e.g. "3f") into a decimal integer 0–255.
  parsePair =
    s: (hexDigits.${builtins.substring 0 1 s}) * 16 + (hexDigits.${builtins.substring 1 1 s});

  # Encode a decimal integer 0–255 as a two-character lowercase hex string.
  encodePair =
    n:
    let
      hi = builtins.elemAt decDigits (n / 16);
      lo = builtins.elemAt decDigits (builtins.bitAnd n 15);
    in
    "${hi}${lo}";

  # Clamp an integer to [lo, hi].
  clamp =
    lo: hi: n:
    if n < lo then
      lo
    else if n > hi then
      hi
    else
      n;

  # Parse "#RRGGBB" into { r, g, b } integers.
  parseHex =
    color:
    let
      hex = builtins.substring 1 6 color;
    in
    {
      r = parsePair (builtins.substring 0 2 hex);
      g = parsePair (builtins.substring 2 2 hex);
      b = parsePair (builtins.substring 4 2 hex);
    };

  # Encode { r, g, b } integers back to "#RRGGBB".
  encodeHex =
    {
      r,
      g,
      b,
    }:
    "#${encodePair r}${encodePair g}${encodePair b}";

in
{
  # darken color pct — darken a "#RRGGBB" colour by pct percent (0–100).
  # e.g. darken "#dafbe1" 15  →  slightly darker green
  darken =
    color: pct:
    let
      c = parseHex color;
      scale = ch: clamp 0 255 (ch * (100 - pct) / 100);
    in
    encodeHex {
      r = scale c.r;
      g = scale c.g;
      b = scale c.b;
    };

  # lighten color pct — lighten a "#RRGGBB" colour by pct percent (0–100).
  # e.g. lighten "#425047" 15  →  slightly lighter green
  lighten =
    color: pct:
    let
      c = parseHex color;
      scale = ch: clamp 0 255 (ch + (255 - ch) * pct / 100);
    in
    encodeHex {
      r = scale c.r;
      g = scale c.g;
      b = scale c.b;
    };
}
