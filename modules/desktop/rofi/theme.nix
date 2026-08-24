{
  config,
  pkgs,
  lib,
  ...
}:
with config.theme;
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.hostPlatform.isLinux) {
    # placeholder-color uses neutrals.foreground_dim (v1 grey1 -> dim text).
    home-manager.users.${config.nx.username}.home.file.".config/rofi/theme.rasi".text = ''
      * {
        background-color: transparent;
        text-color: ${neutrals.foreground};
      }
      window {
        location: 0;
        background-color: ${neutrals.background_0};
        border-color: ${neutrals.background_dim};
        border: 1;
        border-radius: 10px;
        width: 1042px;
      }
      mainbox {
        margin: 5px;
      }
      inputbar {
        border-color: ${hues.green};
        border: 2px;
        border-radius: 5px;
        children: [prompt, entry];
      }
      prompt {
        color: ${neutrals.foreground};
        padding: 10px;
      }
      entry {
        padding: 10px;
        placeholder-color: ${neutrals.foreground_dim};
      }
      listview {
        margin: 5px 0px 0px 0px;
        lines: 7;
      }
      element {
        padding: 7px;
      }
      element-text {
        padding: 7px;
      }
      element-icon {
        size: 35px;
      }
      element selected {
        background-color: ${roles.primary};
        border-radius: 5px;
      }
      element-text selected {
        color: ${neutrals.background_0};
      }
    '';
  };
}
