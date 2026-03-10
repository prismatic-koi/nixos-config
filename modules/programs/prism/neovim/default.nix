{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.neovim.enable = lib.mkEnableOption "Set up Neovim" // {
      default = true;
    };
  };
  imports = [
    ./abbreviations.nix
    ./autocmd.nix
    ./keymaps.nix
    ./options.nix
    ./plugins
  ];
  config = lib.mkIf config.nx.programs.prism.neovim.enable {
    home-manager.users.${config.nx.username} = {
      programs.neovim = {
        enable = true;
        defaultEditor = true;
      };
      home.persistence."/persist" = {
        directories = [
          ".config/nvim/spell"
          ".local/share/nvim"
          ".local/state/nvim"
        ];
      };
      xdg.mimeApps.defaultApplications = lib.mkIf config.programs.neovim.enable {
        "text/plain" = "nvim.desktop";
        "text/markdown" = "nvim.desktop";
      };
    };
  };
}
