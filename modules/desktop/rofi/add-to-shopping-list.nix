{
  config,
  lib,
  pkgs,
  ...
}:
with config.theme;
let
  isLinux = pkgs.stdenv.isLinux;
  isDarwin = pkgs.stdenv.isDarwin;
in
{
  config = lib.mkMerge [
    # Linux: rofi with system-level sops
    (lib.mkIf (config.nx.desktop.rofi.enable && isLinux) {
      # notion API key
      sops.secrets.notion_shopping_list_key = {
        owner = "ben";
        mode = "0600";
        sopsFile = ./secrets/notion.sops.yaml;
      };
      environment.sessionVariables = {
        NOTION_SHOPPING_LIST_KEY = "$(cat ${config.sops.secrets.notion_shopping_list_key.path})";
      };
      home-manager.users.ben.home = {
        file.".local/scripts/home.shoppinglist.addItem" = {
          executable = true;
          text = ''
            #!/bin/sh

            # Function to add item via Notion API
            add_item_to_shopping_list() {
              ITEM_TO_ADD="$*"
              if [ ! -z "$ITEM_TO_ADD" ]; then
                response=$(${pkgs.curl}/bin/curl -s -o /dev/null -w "%{http_code}" \
                  -X PATCH 'https://api.notion.com/v1/blocks/92d98ac3dc86460285a399c0b1176fc5/children' \
                  -H "Authorization: Bearer $NOTION_SHOPPING_LIST_KEY" \
                  -H "Content-Type: application/json" \
                  -H "Notion-Version: 2022-02-22" \
                  --data "{
                  \"children\": [
                    {
                      \"object\": \"block\",
                      \"type\": \"to_do\",
                      \"to_do\": {
                        \"rich_text\": [
                        { \"type\": \"text\", \"text\": { \"content\": \"$ITEM_TO_ADD\" } }
                        ],
                        \"checked\": false
                      }
                    }
                  ]
                }")
                if [ "$response" -eq 200 ]; then
                  ${pkgs.libnotify}/bin/notify-send -i "notes" -t "2000" -e "Notion" "Item added successfully."
                else
                  ${pkgs.libnotify}/bin/notify-send -i "notes" -t "2000" -e "Notion" "Failed. HTTP: $response"
                fi
              fi
            }

            # Spawn rofi menu and get list item
            ROFI_STYLE='listview { enabled: false;} inputbar { children: [entry]; border-color: ${purple};} entry { placeholder: "Add Item to Shopping List"; }'
            selected_item=$(${pkgs.rofi}/bin/rofi -dmenu -i -theme-str "$ROFI_STYLE")
            add_item_to_shopping_list "$selected_item"
          '';
        };
      };
    })

    # Darwin: choose with home-manager sops
    (lib.mkIf (config.nx.desktop.rofi.enable && isDarwin) {
      home-manager.users.ben = {
        sops.secrets = {
          notion_shopping_list_key = {
            sopsFile = ./secrets/notion.sops.yaml;
          };
        };
        home.sessionVariables = {
          NOTION_SHOPPING_LIST_KEY = "$(cat ${config.home-manager.users.ben.sops.secrets.notion_shopping_list_key.path})";
        };
        home.file.".local/scripts/home.shoppinglist.addItem" = {
          executable = true;
          text = ''
            #!/bin/zsh

            # Function to add item via Notion API
            add_item_to_shopping_list() {
              ITEM_TO_ADD="$*"
              if [ ! -z "$ITEM_TO_ADD" ]; then
                response=$(${pkgs.curl}/bin/curl -s -o /dev/null -w "%{http_code}" \
                  -X PATCH 'https://api.notion.com/v1/blocks/92d98ac3dc86460285a399c0b1176fc5/children' \
                  -H "Authorization: Bearer $NOTION_SHOPPING_LIST_KEY" \
                  -H "Content-Type: application/json" \
                  -H "Notion-Version: 2022-02-22" \
                  --data "{
                  \"children\": [
                    {
                      \"object\": \"block\",
                      \"type\": \"to_do\",
                      \"to_do\": {
                        \"rich_text\": [
                        { \"type\": \"text\", \"text\": { \"content\": \"$ITEM_TO_ADD\" } }
                        ],
                        \"checked\": false
                      }
                    }
                  ]
                }")
                if [ "$response" -eq 200 ]; then
                  /usr/bin/osascript -e 'display notification "Item added successfully." with title "Notion"'
                else
                  /usr/bin/osascript -e 'display notification "Failed. HTTP: '"$response"'" with title "Notion"'
                fi
              fi
            }

            # Spawn choose menu and get list item
            selected_item=$(echo \n | ${pkgs.choose-gui}/bin/choose -f "JetbrainsMono Nerd Font" -c "${
              builtins.substring 1 6 config.theme.green
            }" -b "${builtins.substring 1 6 config.theme.bg2}" -s 24 -m -n 0 -p "Add Item to Shopping List")
            add_item_to_shopping_list "$selected_item"
          '';
        };
      };
    })
  ];
}
