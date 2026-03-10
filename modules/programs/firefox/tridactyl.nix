{
  config,
  lib,
  ...
}:
{
  config = lib.mkIf config.nx.programs.firefox.enable {
    home-manager.users.${config.nx.username}.home.file.".config/tridactyl/tridactylrc".text =
      # tridactyl
      ''
        " Unbind
        unbind --mode=normal t
        " Binds
        bind o fillcmdline open
        bind O fillcmdline tabopen
        bind cl tabclosealltoright
        bind ch tabclosealltoleft
        bind tg tabdetach
        bind / fillcmdline find
        bind ? fillcmdline find -?
        bind n findnext 1
        bind N findnext -1
        " this allows clearing of search highlights on escape
        bind <Escape> composite mode normal | nohlsearch
        " Quickmarks
        quickmark c calendar.google.com
        quickmark g github.com
        quickmark i about:blank
        quickmark j https://jardengroup.atlassian.net/jira/software/c/projects/PLAT/boards/191
        quickmark m messenger.com
        quickmark r about:blank
        quickmark w https://www.metservice.com/towns-cities/locations/wellington
        quickmark y about:blank
        " Editor
        set editorcmd kitty --class qute-editor -e nvim
        " Theme
        colors customtheme
        " Disable
        blacklistadd https://monkeytype.com/
        " Search Urls
        setnull searchurls.github
        set searchurls.gh https://github.com/search?q=
        set searchurls.yt https://www.youtube.com/results?search_query=
        set searchurls.hm https://home-manager-options.extranix.com/?query=
        set searchurls.nixpkgs https://search.nixos.org/packages?channel=unstable&from=0&size=50&sort=relevance&type=packages&query=
        set searchurls.nixopt https://search.nixos.org/options?channel=unstable&from=0&size=50&sort=relevance&type=packages&query=
        set searchurls.sx https://search.tinfoilforest.nz/search?q=
        set searchurls.nw https://www.newworld.co.nz/shop/search?q=
        set searchurls.reo https://maoridictionary.co.nz/search?idiom=&phrase=&proverb=&loan=&histLoanWords=&keywords=
      '';
  };
}
