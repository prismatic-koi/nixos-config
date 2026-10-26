# Builds the theme.css.js file for one theme. Used by theme-variables.sh.
# Arguments: root (repository root), theme (nx.desktop.theme value), host.
{
  root,
  theme,
  host ? "tui",
}:
let
  flake = builtins.getFlake (toString root);
  themed = flake.nixosConfigurations.${host}.extendModules {
    modules = [ ({ lib, ... }: { nx.desktop.theme = lib.mkForce theme; }) ];
  };
  inherit (themed) config;
  scripts = config.home-manager.users.${config.nx.username}.programs.qutebrowser.greasemonkey;
in
builtins.head (builtins.filter (d: (d.name or "") == "theme.css.js") scripts)
