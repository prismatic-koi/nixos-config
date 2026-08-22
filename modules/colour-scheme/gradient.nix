{
  config,
  lib,
  ...
}:
# Single shared source of truth for the ncmpcpp visualiser gradient.
#
# kitty accepts arbitrary truecolor hex values for color16-color255, so
# instead of quantising interpolated theme colours onto the xterm-256 cube
# (lossy — see the removed `nearestXterm256` in ./lib.nix), we define our
# own truecolor ramp and point both kitty and ncmpcpp at the same indices.
#
# `modules/programs/kitty.nix` and `modules/programs/ncmpcpp.nix` both read
# this option rather than each carrying their own copy of the colour list
# or the base index — if they drifted, the visualiser would silently render
# the wrong colours.
let
  colourLib = import ./lib.nix;
  inherit (colourLib) mix;

  # Gradient hue order for the visualiser ramp: a monotonic, cool-to-warm
  # sequence. Index 0 renders at the centre of the mirrored stereo display,
  # so cool hues come first.
  visualiserHueOrder = [
    "blue"
    "cyan"
    "emerald"
    "green"
    "lime"
    "yellow"
    "orange"
    "red"
  ];

  rampLength = 24;
  anchorCount = builtins.length visualiserHueOrder;
  # Number of interpolation segments between anchors (wrap not needed: the
  # ramp does not loop back to its start).
  segments = anchorCount - 1;

  anchors = hues: map (name: hues.${name}) visualiserHueOrder;

  # Evaluate the ramp at `n` evenly spaced points across the anchor arc.
  evalRamp =
    hues: n:
    let
      anchorColors = anchors hues;
      # Position (as a float, scaled by `segments`) along the arc for step i
      # of n, i in [0, n-1].
      stepAt =
        i:
        let
          pos = i * segments * 1.0 / (n - 1); # 0 .. segments, may be fractional (float)
          segIdx = if pos >= segments then segments - 1 else builtins.floor pos;
          frac = pos - segIdx;
          pct = builtins.floor (frac * 100 + 0.5);
          a = builtins.elemAt anchorColors segIdx;
          b = builtins.elemAt anchorColors (segIdx + 1);
        in
        mix a b pct;
    in
    map stepAt (lib.range 0 (n - 1));
in
{
  options.nx.colourScheme.visualiserGradient = {
    baseIndex = lib.mkOption {
      type = lib.types.int;
      readOnly = true;
      default = 16;
      description = "First kitty palette index (color16) used by the visualiser gradient.";
    };

    colours = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      readOnly = true;
      default = evalRamp config.themev2.hues rampLength;
      description = "Ordered list of 24 '#RRGGBB' hex colours interpolated across the visualiser hue arc, for kitty color16-color39.";
    };
  };
}
