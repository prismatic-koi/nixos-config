{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.rmpc.enable = lib.mkEnableOption "enables rmpc music player" // {
      default = true;
    };
  };
  config =
    lib.mkIf
      (
        config.nx.programs.rmpc.enable
        # no point in installing if mpd is not
        && config.nx.services.mpd.enable
      )
      {
        home-manager.users.${config.nx.username} = {
          programs.rmpc = {
            enable = true;
            config =
              # rust
              ''
                #![enable(implicit_some)]
                #![enable(unwrap_newtypes)]
                #![enable(unwrap_variant_newtypes)]
                (
                    address: "127.0.0.1:6600",
                    theme: "custom",
                    cache_dir: None,
                    on_song_change: None,
                    volume_step: 5,
                    max_fps: 30,
                    scrolloff: 0,
                    wrap_navigation: false,
                    enable_mouse: true,
                    enable_config_hot_reload: true,
                    status_update_interval_ms: 1000,
                    rewind_to_start_sec: None,
                    reflect_changes_to_playlist: false,
                    select_current_song_on_change: false,
                    browser_song_sort: [Disc, Track, Artist, Title],
                    directories_sort: SortFormat(group_by_type: true, reverse: false),
                    album_art: (
                        method: Auto,
                        max_size_px: (width: 1200, height: 1200),
                        disabled_protocols: ["http://", "https://"],
                        vertical_align: Center,
                        horizontal_align: Center,
                    ),
                    keybinds: (
                        global: {
                            ":":       CommandMode,
                            "s":       Stop,
                            "<Tab>":   NextTab,
                            "<S-Tab>": PreviousTab,
                            "1":       SwitchToTab("Queue"),
                            "2":       SwitchToTab("Album Artists"),
                            "3":       SwitchToTab("Artists"),
                            "4":       SwitchToTab("Playlists"),
                            "5":       SwitchToTab("Search"),
                            "m":       SwitchToTab("Album Artists"),
                            "q":       Quit,
                            ">":       NextTrack,
                            "p":       TogglePause,
                            "<":       PreviousTrack,
                            "f":       SeekForward,
                            "z":       ToggleRepeat,
                            "x":       ToggleRandom,
                            "c":       ToggleConsume,
                            "v":       ToggleSingle,
                            "b":       SeekBack,
                            "~":       ShowHelp,
                            "u":       Update,
                            "U":       Rescan,
                            "I":       ShowCurrentSongInfo,
                            "O":       ShowOutputs,
                            "P":       ShowDecoders,
                            "R":       AddRandom,
                        },
                        navigation: {
                            "k":         Up,
                            "j":         Down,
                            "h":         Left,
                            "l":         Right,
                            "<Up>":      Up,
                            "<Down>":    Down,
                            "<Left>":    Left,
                            "<Right>":   Right,
                            "<C-k>":     PaneUp,
                            "<C-j>":     PaneDown,
                            "<C-h>":     PaneLeft,
                            "<C-l>":     PaneRight,
                            "<C-u>":     UpHalf,
                            "N":         PreviousResult,
                            "a":         Add,
                            "A":         AddAll,
                            "r":         Rename,
                            "n":         NextResult,
                            "g":         Top,
                            "<Space>":   Select,
                            "<C-Space>": InvertSelection,
                            "G":         Bottom,
                            "<CR>":      Confirm,
                            "i":         FocusInput,
                            "J":         MoveDown,
                            "<C-d>":     DownHalf,
                            "/":         EnterSearch,
                            "<C-c>":     Close,
                            "<Esc>":     Close,
                            "K":         MoveUp,
                            "D":         Delete,
                            "B":         ShowInfo,
                        },
                        queue: {
                            "D":       DeleteAll,
                            "<CR>":    Play,
                            "<C-s>":   Save,
                            "a":       AddToPlaylist,
                            "d":       Delete,
                            "C":       JumpToCurrent,
                            "X":       Shuffle,
                        },
                    ),
                    search: (
                        case_sensitive: false,
                        mode: Contains,
                        tags: [
                            (value: "any",         label: "Any Tag"),
                            (value: "artist",      label: "Artist"),
                            (value: "album",       label: "Album"),
                            (value: "albumartist", label: "Album Artist"),
                            (value: "title",       label: "Title"),
                            (value: "filename",    label: "Filename"),
                            (value: "genre",       label: "Genre"),
                        ],
                    ),
                    artists: (
                        album_display_mode: SplitByDate,
                        album_sort_by: Date,
                    ),
                    tabs: [
                        (
                            name: "Queue",
                            pane: Split(
                                direction: Horizontal,
                                panes: [
                                  (size: "70%", pane: Pane(Queue)),
                                  (size: "30%", pane: Pane(AlbumArt))
                                ],
                            ),
                        ),
                        (
                            name: "Album Artists",
                            pane: Pane(AlbumArtists),
                        ),
                        (
                            name: "Artists",
                            pane: Pane(Artists),
                        ),
                        (
                            name: "Playlists",
                            pane: Pane(Playlists),
                        ),
                        (
                            name: "Search",
                            pane: Pane(Search),
                        ),
                    ],
                )
              '';
          };
          xdg.configFile."rmpc/themes/custom.ron".text =
            with config.theme;
            # rust
            ''
              #![enable(implicit_some)]
              #![enable(unwrap_newtypes)]
              #![enable(unwrap_variant_newtypes)]
              (
                  default_album_art_path: None,
                  show_song_table_header: true,
                  draw_borders: true,
                  format_tag_separator: " | ",
                  browser_column_widths: [33, 33, 33],
                  background_color: None,
                  text_color: None,
                  header_background_color: None,
                  modal_background_color: None,
                  preview_label_style: (fg: "yellow"),
                  preview_metadata_group_style: (fg: "yellow", modifiers: "Bold"),
                  tab_bar: (
                      active_style: (fg: "black", bg: "${primary}", modifiers: "Bold"),
                      inactive_style: (),
                      border_style: None,
                  ),
                  highlighted_item_style: (fg: "${primary}", modifiers: "Bold"),
                  current_item_style: (fg: "black", bg: "${primary}", modifiers: "Bold"),
                  borders_style: (fg: "${secondary}"),
                  highlight_border_style: (fg: "blue"),
                  symbols: (song: "", dir: "", playlist: "", marker: "M", ellipsis: "..."),
                  progress_bar: (
                      symbols: ["[", "=", ">", " ", "]"],
                      track_style: None,
                      elapsed_style: (fg: "${primary}"),
                      thumb_style: (fg: "${primary}"),
                  ),
                  scrollbar: None,
                  song_table_format: [
                      (
                          prop: (kind: Property(Artist),
                              default: (kind: Text("Unknown"))
                          ),
                          width: "20%",
                      ),
                      (
                          prop: (kind: Property(Title),
                              default: (kind: Text("Unknown"))
                          ),
                          width: "35%",
                      ),
                      (
                          prop: (kind: Property(Album), style: (fg: "${secondary}"),
                              default: (kind: Text("Unknown Album"), style: (fg: "${secondary}"))
                          ),
                          width: "30%",
                      ),
                      (
                          prop: (kind: Property(Duration),
                              default: (kind: Text("-"))
                          ),
                          width: "15%",
                          alignment: Right,
                      ),
                  ],
                  header: (
                      rows: [
                          (
                              left: [
                                  (kind: Text("["), style: (fg: "yellow", modifiers: "Bold")),
                                  (kind: Property(Status(State)), style: (fg: "yellow", modifiers: "Bold")),
                                  (kind: Text("]"), style: (fg: "yellow", modifiers: "Bold"))
                              ],
                              center: [
                                  (kind: Property(Song(Title)), style: (modifiers: "Bold"),
                                      default: (kind: Text("No Song"), style: (modifiers: "Bold"))
                                  )
                              ],
                              right: [
                                  (kind: Property(Widget(ScanStatus)), style: (fg: "blue")),
                              ]
                          ),
                          (
                              left: [
                                  (kind: Property(Status(Elapsed))),
                                  (kind: Text(" / ")),
                                  (kind: Property(Status(Duration))),
                                  (kind: Text(" (")),
                                  (kind: Property(Status(Bitrate))),
                                  (kind: Text(" kbps)"))
                              ],
                              center: [
                                  (kind: Property(Song(Artist)), style: (fg: "yellow", modifiers: "Bold"),
                                      default: (kind: Text("Unknown"), style: (fg: "yellow", modifiers: "Bold"))
                                  ),
                                  (kind: Text(" - ")),
                                  (kind: Property(Song(Album)),
                                      default: (kind: Text("Unknown Album"))
                                  )
                              ],
                              right: [
                                  (
                                      kind: Property(Widget(States(
                                          active_style: (fg: "white", modifiers: "Bold"),
                                          separator_style: (fg: "white")))
                                      ),
                                      style: (fg: "dark_gray")
                                  ),
                              ]
                          ),
                      ],
                  ),
                  browser_song_format: [
                      (
                          kind: Group([
                              (kind: Property(Track)),
                              (kind: Text(" ")),
                          ])
                      ),
                      (
                          kind: Group([
                              (kind: Property(Title)),
                          ]),
                          default: (kind: Property(Filename))
                      ),
                  ],
                )
              )
            '';
        };
      };
}
