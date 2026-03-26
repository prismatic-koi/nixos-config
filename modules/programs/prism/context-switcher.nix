{
  lib,
  ...
}:
{
  # context-switcher functionality is now provided by `prism switch`.
  # This stub retains the option so existing configurations referencing
  # nx.programs.prism.contextSwitcher.enable continue to evaluate.
  options = {
    nx.programs.prism.contextSwitcher.enable = lib.mkEnableOption "enables tmux context switcher" // {
      default = true;
    };
  };
}
