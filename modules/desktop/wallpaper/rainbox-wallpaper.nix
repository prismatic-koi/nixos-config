{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  resolution = config.nx.desktop.wallpaper.resolution;
  variant = config.nx.desktop.wallpaper.variant;
in
{
  config = lib.mkIf (config.nx.desktop.wallpaper.enable && pkgs.stdenv.isLinux) {
    home-manager.users.ben = {
      home.file.".config/rainbow-wallpaper-2880x1800.svg" =
        lib.mkIf (variant == "rainbow" && resolution == "2880x1800")
          {
            text =
              let
                colourbg = bg_dim;
                colour1 = red;
                colour2 = yellow;
                colour3 = green;
                colour4 = blue;
                colour5 = purple;
              in
              # svg
              ''
                <?xml version="1.0" encoding="UTF-8" standalone="no"?>
                <svg
                   width="2880"
                   height="1800"
                   viewBox="0 0 762 476.25"
                   version="1.1"
                   id="svg1"
                   inkscape:version="1.3.2 (091e20ef0f, 2023-11-25)"
                   sodipodi:docname="wallpaper-2880x1800.svg"
                   xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape"
                   xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"
                   xmlns="http://www.w3.org/2000/svg"
                   xmlns:svg="http://www.w3.org/2000/svg">
                  <sodipodi:namedview
                     id="namedview1"
                     pagecolor="${colourbg}"
                     bordercolor="${colourbg}"
                     borderopacity="0.25"
                     inkscape:showpageshadow="2"
                     inkscape:pageopacity="0.0"
                     inkscape:pagecheckerboard="0"
                     inkscape:deskcolor="${colourbg}"
                     inkscape:document-units="mm"
                     inkscape:zoom="0.30921515"
                     inkscape:cx="1112.494"
                     inkscape:cy="465.69516"
                     inkscape:window-width="1904"
                     inkscape:window-height="1147"
                     inkscape:window-x="0"
                     inkscape:window-y="0"
                     inkscape:window-maximized="1"
                     inkscape:current-layer="layer2" />
                  <defs
                     id="defs1" />
                  <g
                     inkscape:groupmode="layer"
                     id="layer2"
                     inkscape:label="background">
                    <rect
                       style="fill:${colourbg};fill-opacity:1;stroke-width:0.880531"
                       id="rect6"
                       width="762"
                       height="476.25"
                       x="0"
                       y="0" />
                  </g>
                  <g
                     inkscape:label="rainbow"
                     inkscape:groupmode="layer"
                     id="layer1">
                    <path
                       d="m 761.65325,290.00607 -186.30852,186.30853 28.29388,-0.01 157.82447,-157.82448 z"
                       style="fill:${colour5};stroke-width:2.83292"
                       id="path22" />
                    <path
                       d="m 761.84342,261.53132 -214.79309,214.79309 28.2944,-0.01 186.30852,-186.30853 z"
                       style="fill:${colour4};stroke-width:2.62435"
                       id="path21" />
                    <path
                       d="m 762.03411,233.05606 -243.27818,243.27817 28.2944,-0.01 214.79309,-214.79309 z"
                       style="fill:${colour3};stroke-width:4.33177"
                       id="path20" />
                    <path
                       d="m 762.22428,204.58234 -271.76223,271.76171 28.29388,-0.01 243.27818,-243.27817 z"
                       style="fill:${colour2};stroke-width:0.434521"
                       id="path19" />
                    <path
                       d="m 762.41444,176.1076 -300.24627,300.24627 28.29388,-0.01 271.76223,-271.76171 z"
                       style="fill:${colour1};stroke-width:0.583395"
                       id="path18" />
                  </g>
                </svg>
              '';
          };
      home.file.".config/rainbow-wallpaper-5120x1440.svg" =
        lib.mkIf (variant == "rainbow" && resolution == "5120x1440")
          {
            text =
              let
                colourbg = bg_dim;
                colour1 = red;
                colour2 = yellow;
                colour3 = green;
                colour4 = blue;
                colour5 = purple;
              in
              # svg
              ''
                <?xml version="1.0" encoding="UTF-8" standalone="no"?>
                <svg
                   width="5120"
                   height="1440"
                   viewBox="0 0 1354.4974 380.95238"
                   version="1.1"
                   id="svg1"
                   inkscape:version="1.3.2 (091e20ef0f, 2023-11-25)"
                   sodipodi:docname="wallpaper-5120x1440.svg"
                   xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape"
                   xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"
                   xmlns="http://www.w3.org/2000/svg"
                   xmlns:svg="http://www.w3.org/2000/svg">
                  <sodipodi:namedview
                     id="namedview1"
                     pagecolor="${colourbg}"
                     bordercolor="${colourbg}"
                     borderopacity="0.25"
                     inkscape:showpageshadow="2"
                     inkscape:pageopacity="0.0"
                     inkscape:pagecheckerboard="0"
                     inkscape:deskcolor="${colourbg}"
                     inkscape:document-units="mm"
                     inkscape:zoom="0.24519795"
                     inkscape:cx="2465.355"
                     inkscape:cy="611.75063"
                     inkscape:window-width="1904"
                     inkscape:window-height="1147"
                     inkscape:window-x="0"
                     inkscape:window-y="0"
                     inkscape:window-maximized="1"
                     inkscape:current-layer="layer2" />
                  <defs
                     id="defs1" />
                  <g
                     inkscape:groupmode="layer"
                     id="layer2"
                     inkscape:label="background">
                    <rect
                       style="fill:${colourbg};fill-opacity:1;stroke-width:0.880531"
                       id="rect6"
                       width="1354.4974"
                       height="380.95239"
                       x="0"
                       y="0" />
                  </g>
                  <g
                     inkscape:label="rainbow"
                     inkscape:groupmode="layer"
                     id="layer1">
                    <path
                       d="m 1353.7362,194.60456 -186.3086,186.30856 28.2939,-0.01 157.8246,-157.8245 z"
                       style="fill:${colour5};stroke-width:2.83292"
                       id="path22" />
                    <path
                       d="m 1353.9264,166.12981 -214.7932,214.79312 28.2944,-0.01 186.3086,-186.30856 z"
                       style="fill:${colour4};stroke-width:2.62435"
                       id="path21" />
                    <path
                       d="m 1354.1171,137.65455 -243.2783,243.2782 28.2944,-0.01 214.7932,-214.79312 z"
                       style="fill:${colour3};stroke-width:4.33177"
                       id="path20" />
                    <path
                       d="m 1354.3073,109.18082 -271.7624,271.76175 28.2939,-0.01 243.2783,-243.2782 z"
                       style="fill:${colour2};stroke-width:0.434521"
                       id="path19" />
                    <path
                       d="m 1354.4974,80.706086 -300.2463,300.246304 28.2938,-0.01 271.7624,-271.76175 z"
                       style="fill:${colour1};stroke-width:0.583395"
                       id="path18" />
                  </g>
                </svg>
              '';
          };
    };
  };
}
