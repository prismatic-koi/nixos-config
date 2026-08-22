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

  # Round a (possibly negative) float to the nearest integer.
  round =
    n:
    let
      f = builtins.floor n;
      frac = n - f;
    in
    if frac >= 0.5 then f + 1 else f;

  # xterm-256 colour cube axis values for indices 16-231 (6x6x6 cube).
  cubeSteps = [
    0
    95
    135
    175
    215
    255
  ];

  cubeIdxs = [
    0
    1
    2
    3
    4
    5
  ];

  absInt = n: if n < 0 then -n else n;

  # Nearest cube-axis position (0-5) for a channel value 0-255.
  nearestCubeStep =
    ch:
    let
      diffs = map (i: {
        idx = i;
        diff = absInt (ch - builtins.elemAt cubeSteps i);
      }) cubeIdxs;
    in
    (builtins.foldl' (best: d: if d.diff < best.diff then d else best) (builtins.elemAt diffs 0) diffs)
    .idx;

  # Nearest greyscale-ramp step (0-23) for a channel value 0-255. Index
  # 232 + i represents grey level 8 + i*10.
  nearestGreyStep = v: clamp 0 23 (round ((v - 8) / 10));

  greyLevel = i: 8 + i * 10;

  # Squared Euclidean distance between two { r, g, b } colours.
  dist2 =
    a: b:
    let
      dr = a.r - b.r;
      dg = a.g - b.g;
      db = a.b - b.b;
    in
    dr * dr + dg * dg + db * db;

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

  # mix a b pct — interpolate pct percent of the way from a to b (0-100).
  # mix a b 0 == a; mix a b 100 == b.
  mix =
    a: b: pct:
    let
      ca = parseHex a;
      cb = parseHex b;
      lerp = x: y: clamp 0 255 (round (x + (y - x) * pct / 100.0));
    in
    encodeHex {
      r = lerp ca.r cb.r;
      g = lerp ca.g cb.g;
      b = lerp ca.b cb.b;
    };

  # nearestXterm256 color — map a "#RRGGBB" colour to the nearest xterm-256
  # palette index in the range 16-255 (the 6x6x6 colour cube plus the
  # 24-step greyscale ramp; indices 0-15 — the reconfigurable ANSI/bright
  # slots — are deliberately excluded).
  nearestXterm256 =
    color:
    let
      c = parseHex color;

      cubeR = nearestCubeStep c.r;
      cubeG = nearestCubeStep c.g;
      cubeB = nearestCubeStep c.b;
      cubeColor = {
        r = builtins.elemAt cubeSteps cubeR;
        g = builtins.elemAt cubeSteps cubeG;
        b = builtins.elemAt cubeSteps cubeB;
      };
      cubeIdx = 16 + 36 * cubeR + 6 * cubeG + cubeB;

      greyStep = nearestGreyStep ((c.r + c.g + c.b) / 3);
      greyV = greyLevel greyStep;
      greyColor = {
        r = greyV;
        g = greyV;
        b = greyV;
      };
      greyIdx = 232 + greyStep;

      cubeDist = dist2 c cubeColor;
      greyDist = dist2 c greyColor;

      # Saturation: spread between the loudest and quietest channel. A
      # colour with any meaningful saturation reads badly as a flat grey
      # band in the visualiser, so the cube wins outright once saturation
      # clears a small threshold — the grey ramp is reserved for colours
      # that are genuinely close to neutral.
      maxCh = if c.r > c.g then (if c.r > c.b then c.r else c.b) else (if c.g > c.b then c.g else c.b);
      minCh = if c.r < c.g then (if c.r < c.b then c.r else c.b) else (if c.g < c.b then c.g else c.b);
      saturation = maxCh - minCh;
    in
    if saturation > 20 then
      cubeIdx
    else if cubeDist <= greyDist then
      cubeIdx
    else
      greyIdx;
}
