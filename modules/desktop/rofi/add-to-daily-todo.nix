{
  config,
  pkgs,
  lib,
  ...
}:
with config.theme;
{
  config = lib.mkIf (config.nx.desktop.rofi.enable && pkgs.stdenv.isLinux) {
    home-manager.users.${config.nx.username}.home = {
      file.".local/scripts/obsidian.dailyTodo.addItem" = {
        executable = true;
        text =
          # python
          ''
            #!/usr/bin/env python3
            import os
            import subprocess
            from datetime import datetime

            ROFI_STYLE = 'listview { enabled: false;} inputbar { children: [entry]; border-color: ${orange};} entry { placeholder: "Add To-do Item"; }'
            OBSIDIAN_DAILIES_DIR = os.path.expanduser("~/documents/obsidian/dailies")
            TODO_MARKER = "## To-do List"


            def add_todo_item(item):
                today = datetime.now().strftime("%Y-%m-%d")
                daily_note_path = os.path.join(OBSIDIAN_DAILIES_DIR, f"{today}.md")

                try:
                    # Check if daily note exists
                    if not os.path.exists(daily_note_path):
                        subprocess.run(
                            ["notify-send", "-i", "notes", "-t", "2000", "-e", "Obsidian", f"Daily note for {today} not found."]
                        )
                        return

                    # Read the file
                    with open(daily_note_path, "r") as f:
                        lines = f.readlines()

                    # Find the "## To-do List" line
                    todo_index = None
                    for i, line in enumerate(lines):
                        if line.strip() == TODO_MARKER:
                            todo_index = i
                            break

                    if todo_index is None:
                        subprocess.run(
                            ["notify-send", "-i", "notes", "-t", "2000", "-e", "Obsidian", f"'## To-do List' heading not found in {today}.md"]
                        )
                        return

                    # Insert the todo item after the "## To-do List" line
                    new_todo = f"- [ ] {item}\n"
                    lines.insert(todo_index + 1, new_todo)

                    # Write back to the file
                    with open(daily_note_path, "w") as f:
                        f.writelines(lines)

                    subprocess.run(
                        ["notify-send", "-i", "notes", "-t", "2000", "-e", "Obsidian", "To-do item added successfully."]
                    )

                except Exception as e:
                    subprocess.run(
                        ["notify-send", "-i", "notes", "-t", "2000", "-e", "Obsidian", f"Error: {str(e)}"]
                    )


            selected_item = subprocess.run(
                ["rofi", "-dmenu", "-i", "-theme-str", ROFI_STYLE], capture_output=True, text=True
            ).stdout.strip()
            if selected_item:
                add_todo_item(selected_item)
          '';
      };
    };
  };
}
