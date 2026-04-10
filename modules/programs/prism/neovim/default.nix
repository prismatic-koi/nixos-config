{
  config,
  lib,
  pkgs,
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
    home-manager.users.${config.nx.username} =
      let
        maorispl = pkgs.fetchurl {
          url = "https://ftp.nluug.nl/pub/vim/runtime/spell/mi.utf-8.spl";
          hash = "sha256:0qr0szs8mfhm90k02z2pab3n5s93pl9ysfsz53b64dr4rkdqys4w";
        };
        maoriSpellDir = pkgs.runCommand "maori-spell" { } ''
          mkdir -p $out/spell
          cp ${maorispl} $out/spell/mi.utf-8.spl
        '';
      in
      {
        programs.neovim = {
          enable = true;
          defaultEditor = true;
          withRuby = false;
          withPython3 = false;
          initLua = lib.mkAfter ''
            vim.opt.runtimepath:append("${maoriSpellDir}")
          '';
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
