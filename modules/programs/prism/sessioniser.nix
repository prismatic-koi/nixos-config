{
  lib,
  ...
}:
{
  # Project discovery is now handled directly by `prism switch` using
  # nx.programs.prism.projects.locations and .specific options.
  # This stub retains the option for backwards compatibility.
  options = {
    nx.programs.prism.sessioniser.enable = lib.mkEnableOption "enables tmux sessioniser" // {
      default = true;
    };
  };
}
