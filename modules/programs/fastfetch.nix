{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.fastfetch.enable = lib.mkEnableOption "enables fastfetch" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.fastfetch.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        fastfetch
      ];
      programs.fastfetch = {
        enable = true;
        settings = {
          logo = {
            source = pkgs.writeTextFile {
              name = "nixos-logo";
              text = ''
                $1          ▗▄▄▄       $2▗▄▄▄▄    ▄▄▄▖
                $1          ▜███▙       $2▜███▙  ▟███▛
                $1           ▜███▙       $2▜███▙▟███▛
                $1            ▜███▙       $2▜██████▛
                $1     ▟█████████████████▙ $2▜████▛     $3▟▙
                $1    ▟███████████████████▙ $2▜███▙    $3▟██▙
                $6           ▄▄▄▄▖           $2▜███▙  $3▟███▛
                $6          ▟███▛             $2▜██▛ $3▟███▛
                $6         ▟███▛               $2▜▛ $3▟███▛
                $6▟███████████▛                  $3▟██████████▙
                $6▜██████████▛                  $3▟███████████▛
                $6      ▟███▛ $5▟▙               $3▟███▛
                $6     ▟███▛ $5▟██▙             $3▟███▛
                $6    ▟███▛  $5▜███▙           $3▝▀▀▀▀
                $6    ▜██▛    $5▜███▙ $4▜██████████████████▛
                $6     ▜▛     $5▟████▙ $4▜████████████████▛
                           $5▟██████▙       $4▜███▙
                          $5▟███▛▜███▙       $4▜███▙
                         $5▟███▛  ▜███▙       $4▜███▙
                         $5▝▀▀▀    ▀▀▀▀▘       $4▀▀▀▘
              '';
            };
            # config.home-manager.users.${config.nx.username}.home.file."nixos-logo".source;
            color = with config.theme; {
              "1" = "${yellow}";
              "2" = "${green}";
              "3" = "${blue}";
              "4" = "${purple}";
              "5" = "${red}";
              "6" = "${orange}";
            };
          };
          display = {
            color = {
              keys = "green";
              title = "blue";
            };
          };
          modules = [
            "title"
            "separator"
            "os"
            "host"
            "kernel"
            "uptime"
            "packages"
            "shell"
            "display"
            "wm"
            "cursor"
            "terminal"
            "terminalfont"
            "cpu"
            "gpu"
            "memory"
            "swap"
            "disk"
            "battery"
            "poweradapter"
            "locale"
            "break"
            "colors"
          ];
        };
      };
      programs.zsh.shellAliases = {
        ff = "fastfetch";
      };
    };
  };
}
