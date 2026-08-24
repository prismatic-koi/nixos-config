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
              "1" = "${hues.yellow}";
              "2" = "${hues.green}";
              "3" = "${hues.blue}";
              "4" = "${hues.purple}";
              "5" = "${hues.red}";
              "6" = "${hues.orange}";
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
