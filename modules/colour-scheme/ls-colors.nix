# Themes lf, eza and LS_COLORS from config.theme.
#
# `lsColors` is the single source: kinds -> colour. The LS_COLORS string,
# the lf `colors` file and the eza file-kind colours all derive from it.
{ config, lib, ... }:
let
  inherit (import ./lib.nix) parseHex;
  t = config.theme;
  h = t.hues;

  sgr =
    hex:
    let
      c = parseHex hex;
    in
    "38;2;${toString c.r};${toString c.g};${toString c.b}";

  # { key, colour, bold } — ow/tw/st share the directory entry so no
  # background rule (the unreadable default ow=34;42) can apply.
  dir = {
    colour = h.blue;
    bold = true;
  };
  kinds = {
    di = dir;
    ow = dir;
    tw = dir;
    st = dir;
    ln = {
      colour = h.cyan;
      bold = false;
    };
    mh = {
      colour = h.cyan;
      bold = false;
    };
    or = {
      colour = h.red;
      bold = true;
    };
    mi = {
      colour = h.red;
      bold = false;
    };
    pi = {
      colour = h.yellow;
      bold = false;
    };
    so = {
      colour = h.fuchsia;
      bold = false;
    };
    bd = {
      colour = h.amber;
      bold = true;
    };
    cd = {
      colour = h.amber;
      bold = false;
    };
    do = {
      colour = h.fuchsia;
      bold = true;
    };
    ex = {
      colour = h.green;
      bold = true;
    };
    su = {
      colour = h.red;
      bold = true;
    };
    sg = {
      colour = h.orange;
      bold = true;
    };
  };

  entry = k: v: "${k}=${lib.optionalString v.bold "1;"}${sgr v.colour}";
  rules = lib.mapAttrsToList entry kinds;
  lsColors = lib.concatStringsSep ":" rules;

  # lf `colors` file: one rule per line, same rules as LS_COLORS.
  lfColors = lib.concatStringsSep "\n" rules + "\n";

  fg = colour: { foreground = colour; };
  ethemeKinds = {
    directory = fg kinds.di.colour;
    symlink = fg kinds.ln.colour;
    pipe = fg kinds.pi.colour;
    socket = fg kinds.so.colour;
    block_device = fg kinds.bd.colour;
    char_device = fg kinds.cd.colour;
    executable = fg kinds.ex.colour;
  };

  ezaTheme = {
    filekinds = ethemeKinds;
    perms = {
      user_read = fg h.green;
      user_write = fg h.yellow;
      user_execute_file = fg h.red;
      user_execute_other = fg h.red;
      group_read = fg h.green;
      group_write = fg h.yellow;
      group_execute = fg h.red;
      other_read = fg h.green;
      other_write = fg h.yellow;
      other_execute = fg h.red;
      special_user_file = fg h.purple;
      special_other = fg h.purple;
      attribute = fg t.neutrals.foreground_dim;
    };
    size = {
      major = fg h.green;
      minor = fg h.teal;
      number_byte = fg h.green;
      number_kilo = fg h.green;
      number_mega = fg h.lime;
      number_giga = fg h.amber;
      number_huge = fg h.orange;
      unit_byte = fg h.emerald;
      unit_kilo = fg h.emerald;
      unit_mega = fg h.lime;
      unit_giga = fg h.amber;
      unit_huge = fg h.orange;
    };
    users = {
      user_you = fg h.yellow;
      user_root = fg h.red;
      user_other = fg h.purple;
      group_yours = fg h.yellow;
      group_other = fg t.neutrals.foreground_dim;
      group_root = fg h.red;
    };
    date = {
      time_today = fg h.cyan;
      time_week = fg h.sky;
      time_month = fg h.blue;
      time_year = fg h.indigo;
      time_old = fg t.neutrals.foreground_dim;
      time_future = fg h.violet;
    };
    git = {
      new = fg h.green;
      modified = fg h.yellow;
      deleted = fg h.red;
      renamed = fg h.cyan;
      typechange = fg h.purple;
      ignored = fg t.neutrals.foreground_dim;
      conflicted = fg h.red;
    };
  };
in
{
  options.lsColors = lib.mkOption {
    type = lib.types.str;
    readOnly = true;
    description = "LS_COLORS value generated from config.theme.";
  };

  config = {
    lsColors = lsColors;

    assertions = [
      {
        assertion = !(lib.any (v: v.colour == t.neutrals.background_0) (lib.attrValues kinds));
        message = "LS_COLORS: a foreground colour equals neutrals.background_0";
      }
    ];

    home-manager.users.${config.nx.username} =
      { config, ... }:
      {
        home.sessionVariables = {
          LS_COLORS = lsColors;
          # eza on Darwin defaults to ~/Library/Application Support/eza.
          EZA_CONFIG_DIR = "${config.xdg.configHome}/eza";
        };
        xdg.configFile."lf/colors".text = lfColors;
        programs.eza = {
          enable = true;
          # zsh.nix owns the aliases.
          enableBashIntegration = false;
          enableZshIntegration = false;
          enableFishIntegration = false;
          enableNushellIntegration = false;
          enableIonIntegration = false;
          theme = ezaTheme;
        };
      };
  };
}
