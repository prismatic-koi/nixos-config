{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.forgecode.enable = lib.mkEnableOption "enables forgecode AI coding agent" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.prism.forgecode.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = lib.optional (pkgs.forgecode != null) pkgs.forgecode ++ [
        # fzf powers forgecode's @file tab-completion and :conversation picker.
        pkgs.fzf
        # fd is used by forgecode's fast file listing path.
        pkgs.fd
      ];

      # Order 1100 runs after the base zsh init (order 1000) so we can safely
      # reference the forge binary, XDG dirs, and any zsh hooks set up earlier.
      # Note: p10k plugins are sourced before initContent in the generated
      # .zshrc, so POWERLEVEL9K_* arrays are already populated at this point.
      # Nevertheless, the `forge` segment is registered statically in
      # modules/programs/files/p10k.zsh rather than appended here — this keeps
      # its position explicit and reviewable in the diff.
      programs.zsh.initContent = lib.mkOrder 1100 (builtins.readFile ./files/forgecode-init.zsh);

      # Persist all forgecode state — credentials, config, conversation history,
      # snapshots, cache, etc. all live under ~/forge/ (the base_path).
      #
      # CONTAINER NOTE (future work): if forgecode is eventually run inside
      # containers that bind-mount ~/ from the host, both host and container
      # shells will share this state directory. forgecode's conversation store
      # (SQLite WAL, per-conversation UUIDs) handles concurrent reads and writes
      # across different conversation IDs safely via a connection pool with a
      # 30s busy timeout. The risk is same-conversation-ID races: two shells
      # that deliberately resume the same conversation can lose updates
      # (last-write-wins, not corruption). When container support lands, either
      # enforce per-shell conversation isolation or give each container its own
      # volume for ~/forge/ to fully isolate state from the host.
      home.persistence."/persist" = {
        directories = [
          "forge"
        ];
      };
    };
  };
}
