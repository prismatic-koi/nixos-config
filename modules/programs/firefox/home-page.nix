{
  config,
  lib,
  ...
}:
with config.theme;
{
  config = lib.mkIf config.nx.programs.firefox.enable {
    home-manager.users.${config.nx.username} = {
      home.file.".config/tridactyl/home.html".text =
        # html
        ''
          <html>
            <head>
              <link rel="stylesheet" href="chrome://activity-stream/content/css/activity-stream.css">
              <!-- <style> -->
                <!-- --newtab-textbox-focus-color: ${bg0} !important; -->
                <!-- </style> -->
            </head>
            <body style="background-color: ${bg0}; ">
              <div class="outer-wrapper ds-outer-wrapper-breakpoint-override only-search visible-logo">
                <main>
                  <div class="non-collapsible-section">
                    <div class="search-wrapper">
                      <div class="logo-and-wordmark" style="width:100%;">
                        <div class="logo"></div>
                        <div class="wordmark" style="fill: ${foreground} !important;"></div>
                      </div>
                      <div class="search-inner-wrapper" padding-inline-start>
                        <form style="display: block; font-family='JetBrainsMonoNerdFont',monospace; width:100%;">
                          <input
                            style="background-color: ${bg3}; color: ${foreground}; border-radius: 0px !important; --newtab-primary-action-background: ${green}; padding-inline-start:10px; height: 50px;"
                            type="text" size="50" name="q" placeholder="Search the web" id="search-field">
                          <input type="submit" style="display: none;" formaction="https://google.com/search?hl=en&amp;q={}"
                            formmethod="get">
                        </form>
                      </div>
                    </div>
                  </div>
                </main>
              </div>
            </body>
            </html>
        '';
    };
  };
}
