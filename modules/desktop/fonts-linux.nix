{
  pkgs,
  ...
}:
{
  fonts = {
    fontDir.enable = true;
    packages = [
      pkgs.noto-fonts
      pkgs.noto-fonts-color-emoji
      pkgs.nerd-fonts.jetbrains-mono
    ];
    fontconfig = {
      enable = true;
      defaultFonts = {
        serif = [
          "Noto Sans"
          "JetBrainsMono Nerd Font"
        ];
        sansSerif = [
          "Noto Sans"
          "JetBrainsMono Nerd Font"
        ];
        monospace = [ "JetBrainsMono Nerd Font" ];
        emoji = [ "Noto Color Emoji" ];
      };
    };
  };
}
