{ config, pkgs, ... }:
{
  home-manager.users.${config.nx.username}.programs.neovim.plugins = [
    {
      plugin = pkgs.vimPlugins.obsidian-nvim;
      type = "lua";
      config =
        # lua
        ''
          -- Helper function to get the date of the next specified weekday
          local getNextWeekday = function(dayOfWeek)
          	local currentWeekday = os.date("%w", os.time())
          	-- Calculate the timestamp for the next Monday relative to now, then adjust for the target dayOfWeek
          	-- os.time() + (seconds until next Monday relative to current weekday) + (seconds for offset to target dayOfWeek)
          	local secondsToNextMonday = (7 - currentWeekday + 1) % 7 * 24 * 60 * 60 -- Handle Sunday (0) to ensure next Monday is correct
          	if secondsToNextMonday == 0 then
          		secondsToNextMonday = 7 * 24 * 60 * 60
          	end -- If current day is Monday, get next Monday

          	local nextWeekStart = os.time() + secondsToNextMonday
          	-- Adjust from Monday (dayOfWeek 1) to the target dayOfWeek
          	local targetDayTimestamp = nextWeekStart + (dayOfWeek - 1) * 24 * 60 * 60
          	return os.date("%Y-%m-%d", targetDayTimestamp)
          end

          -- Define note_id_func separately for clarity
          local noteIdFunction = function(title)
          	-- lowercase and replace spaces with underscores
          	return string.lower(title:gsub("%s", "_"))
          end

          -- Notes whose id is YYYY-MM-DD live in dailies/ on every creation path;
          -- all other notes go in the directory the plugin picks (notes/).
          local notePathFunction = function(spec)
          	local id = tostring(spec.id)
          	if id:match("^%d%d%d%d%-%d%d%-%d%d$") then
          		return vim.fs.joinpath(tostring(Obsidian.workspace.root), "dailies", id)
          	end
          	return vim.fs.joinpath(tostring(spec.dir), id)
          end

          -- Define note_frontmatter_func separately for clarity
          local noteFrontmatterFunction = function(note)
          	-- Ensure note.path is a string
          	local note_path = tostring(note.path)

          	-- Extract filename (without extension)
          	local filename = vim.fn.fnamemodify(note_path, ":t:r") -- Gets 'YYYY-MM-DD' from 'YYYY-MM-DD.md'

          	-- Check if filename matches YYYY-MM-DD format
          	local is_daily_note = filename:match("^%d%d%d%d%-%d%d%-%d%d$") ~= nil

          	-- Look for the first H1 heading in the note content and add it as an alias
          	-- This restores the pre-v3.15.0 behavior where the title was automatically added
          	-- Guard: note.contents triggers io.lines() via __index, which fails if the file
          	-- doesn't exist on disk yet (e.g. first save of a new note in BufWritePre).
          	local file_exists = note.path and vim.fn.filereadable(tostring(note.path)) == 1
          	if file_exists and note.contents then
          		for _, line in ipairs(note.contents) do
          			local h1_match = line:match("^#%s+(.+)$")
          			if h1_match then
          				local title = vim.trim(h1_match)
          				if title and not vim.tbl_contains(note.aliases, title) then
          					note:add_alias(title)
          				end
          				break -- Only use the first H1 heading
          			end
          		end
          	end

          	-- Get current date and time
          	local current_date = os.date("%Y-%m-%d") -- Matches daily note format
          	local current_time = os.date("%Y-%m-%d %H:%M:%S") -- Full timestamp

          	local out = {
          		aliases = note.aliases,
          		tags = note.tags,
          		modified = current_time, -- Timestamp for modifications
          	}

          	-- Add "daily-note" tag if it's a daily note and not already tagged
          	if is_daily_note and not vim.tbl_contains(note.tags, "daily-note") then
          		table.insert(out.tags, "daily-note")
          	end

          	-- Preserve existing metadata first (including existing 'created' if it exists)
          	if note.metadata ~= nil and not vim.tbl_isempty(note.metadata) then
          		for k, v in pairs(note.metadata) do
          			-- Only copy if it's not 'modified', which we explicitly set above
          			-- and also ensure we don't overwrite 'created' if it already exists
          			-- and this isn't a new note.
          			if k ~= "modified" then
          				out[k] = v
          			end
          		end
          	end

          	if vim.tbl_contains(out.tags, "person") and out.pronouns == nil then
          		out.pronouns = {}
          	end

          	-- Use note.new to determine if it's a newly created note.
          	-- Add 'created' ONLY if it's a new note AND it doesn't already have a 'created' field
          	if note.new and out.created == nil then
          		out.created = "[[" .. current_date .. "]]"
          	end

          	return out
          end

          -- Define img_name_func separately
          local imgNameFunction = function()
          	return string.format(os.date("%Y-%m-%d_%H%M%S"), "_pasted_image")
          end

          -- Define substitutions separately for clarity
          local templateSubstitutions = {
          	nextweekyear = function()
          		return os.date("%Y", os.time() + 7 * 24 * 60 * 60)
          	end,
          	nextweeknum = function()
          		return os.date("%V", os.time() + 7 * 24 * 60 * 60)
          	end,
          	nextweekmonday = function()
          		return getNextWeekday(1)
          	end,
          	nextweektuesday = function()
          		return getNextWeekday(2)
          	end,
          	nextweekwednesday = function()
          		return getNextWeekday(3)
          	end,
          	nextweekthursday = function()
          		return getNextWeekday(4)
          	end,
          	nextweekfriday = function()
          		return getNextWeekday(5)
          	end,
          	nextweeksaturday = function()
          		return getNextWeekday(6)
          	end,
          	nextweeksunday = function()
          		return getNextWeekday(7)
          	end,
          }

          require("obsidian").setup({
          	workspaces = {
          		{
          			name = "personal",
          			path = "~/documents/obsidian",
          		},
          	},
          	note_id_func = noteIdFunction,
          	note_path_func = notePathFunction,
          	notes_subdir = "notes",
          	daily_notes = {
              folder = "dailies",
              date_format = "%Y-%m-%d",
              template = "daily_note.md",
              default_tags = { "daily-note" },
              workdays_only = false,
            },
          	templates = {
          		subdir = "templates",
          		substitutions = templateSubstitutions,
          	},
          	new_notes_location = "notes_subdir",
          	link = {
          		style = "wiki",
          	},
            frontmatter = {
              enabled = true,
              func = noteFrontmatterFunction,
            },
          	ui = {
          		-- don't use obsidian.nvim colours
          		hl_groups = {},
          	},
            footer = {
              -- https://github.com/obsidian-nvim/obsidian.nvim/issues/566
              enabled = false,
            },
            statusline = {
              enabled = false, -- Disable statusline to prevent it hammering ripgrep every second
            },
          	checkbox = {
          		order = { " ", "x", ">", "~" },
          	},
          	attachments = {
              folder = "assets",
          		img_name_func = imgNameFunction,
          		confirm_img_paste = true,
          	},
            legacy_commands = false, -- Disable legacy commands
          })

          -- Opens the daily note for `timestamp` (default: today), creating it from the
          -- daily template when missing. Uses :edit in the current window.
          -- Obsidian's api.open_note switches to :drop when any window already shows
          -- the note, which jumps to zen-mode's hidden parent window and closes zen.
          local open_daily = function(timestamp)
          	local note = require("obsidian.daily").daily({ date = timestamp })
          	if not note:exists() then
          		note:write()
          	end
          	vim.cmd.edit(vim.fn.fnameescape(tostring(note.path)))
          	vim.b[vim.api.nvim_get_current_buf()].note = note
          end

          -- Mirrors `Obsidian today [N]`: same offset arithmetic as obsidian.daily.
          local open_daily_offset = function(offset_from_today)
          	open_daily(os.time() + offset_from_today * 24 * 60 * 60)
          end

          -- Midday timestamp for "YYYY-MM-DD", or nil when the date is not real.
          -- os.time() normalises out-of-range fields, so check the round trip.
          local date_to_timestamp = function(str)
          	local y, m, d = str:match("^(%d%d%d%d)%-(%d%d)%-(%d%d)$")
          	if not y then
          		return nil
          	end
          	local ts = os.time({ year = tonumber(y), month = tonumber(m), day = tonumber(d), hour = 12 })
          	if ts and os.date("%Y-%m-%d", ts) == str then
          		return ts
          	end
          	return nil
          end

          -- Shared by <leader>of and gd.
          -- - real date link: open or create the daily note (template, tag)
          -- - labelled link to a missing note: create it with the label as title,
          --   alias and H1
          -- - anything else: plain `Obsidian follow_link`
          local follow_link = function()
          	local fallback = function()
          		vim.cmd("Obsidian follow_link")
          	end
          	local api = require("obsidian.api")
          	local link, _, _, _ = api.cursor_link()
          	if not link then
          		return fallback()
          	end
          	local ref = require("obsidian.parse.refs").parse(link)
          	if not ref or ref.kind ~= "wiki" then
          		return fallback()
          	end
          	local target = vim.uri_decode(ref.target) or ref.target
          	local location = require("obsidian.link.parser").parse(target)

          	local ts = date_to_timestamp(location)
          	if ts then
          		return open_daily(ts)
          	end

          	local label = ref.label
          	if label == nil or label == "" or location == "" or api.is_attachment_path(location) then
          		return fallback()
          	end

          	local bufnr = vim.api.nvim_get_current_buf()
          	local source = vim.api.nvim_buf_get_name(bufnr)
          	require("obsidian.search").resolve_note_async(location, function(notes)
          		if not vim.tbl_isempty(notes) then
          			return fallback()
          		end
          		if api.confirm(("Create new note '%s'?"):format(location), "&Yes\n&No") ~= "Yes" then
          			return require("obsidian.log").warn("Aborted")
          		end
          		local note = require("obsidian.note").create({
          			id = location,
          			title = label,
          			aliases = { label },
          			source_path = source ~= "" and source or nil,
          		})
          		note:write({
          			update_content = function(_)
          				return { "# " .. label }
          			end,
          		})
          		vim.cmd.edit(vim.fn.fnameescape(tostring(note.path)))
          		vim.b[vim.api.nvim_get_current_buf()].note = note
          	end, {
          		dir = api.resolve_workspace_dir(source ~= "" and source or nil),
          		buf_dir = source ~= "" and vim.fs.dirname(source) or nil,
          	})
          end

          -- custom commands for previous and next daily note
          local offset_daily = function(relative_offset_from_current_note)
          	local current_filename = vim.fn.expand("%:t:r") -- e.g., "2017-04-23"
          	local daily_date_format = "%Y-%m-%d"
          	local year_str, month_str, day_str = current_filename:match("(%d%d%d%d)%-(%d%d)%-(%d%d)")

          	if not (year_str and month_str and day_str) then
          		vim.notify(
          			"Current buffer filename is not a YYYY-MM-DD daily note. Cannot calculate relative day.",
          			vim.log.levels.WARN
          		)
          		return false
          	end

          	-- Convert captured strings to numbers
          	local current_year = tonumber(year_str)
          	local current_month = tonumber(month_str)
          	local current_day = tonumber(day_str)

          	-- Get the timestamp for the current note's date (midday to avoid DST issues)
          	local current_note_timestamp = os.time({
          		year = current_year,
          		month = current_month,
          		day = current_day,
          		hour = 12, -- Set to midday to avoid issues with DST changes at midnight
          		min = 0,
          		sec = 0,
          	})

          	-- Calculate the timestamp for the *target* note (previous or next day from current)
          	local target_note_timestamp = current_note_timestamp + (relative_offset_from_current_note * 24 * 60 * 60)

          	-- Get today's timestamp (midday to avoid DST issues)
          	local today_timestamp = os.time({
          		year = os.date("%Y", os.time()),
          		month = os.date("%m", os.time()),
          		day = os.date("%d", os.time()),
          		hour = 12,
          		min = 0,
          		sec = 0,
          	})

          	-- Calculate the difference in days between the target note and today
          	-- Divide by seconds in a day (24*60*60) and round to nearest whole number
          	local total_offset_from_today = math.floor((target_note_timestamp - today_timestamp) / (24 * 60 * 60) + 0.5)

          	open_daily_offset(total_offset_from_today)
          	return true
          end

          vim.api.nvim_create_user_command("ObsidianPrevDay", function(_)
          	offset_daily(-1)
          end, {
          	bang = false,
          	bar = false,
          	register = false,
          	desc = "Create and switch to the previous daily note based on current buffer",
          })

          vim.api.nvim_create_user_command("ObsidianNextDay", function(_)
          	offset_daily(1)
          end, {
          	bang = false,
          	bar = false,
          	register = false,
          	desc = "Create and switch to the next daily note based on current buffer",
          })

          -- Custom command to open a random note from the notes folder
          vim.api.nvim_create_user_command("ObsidianRandomNote", function(_)
          	local notes_dir = vim.fn.expand("~/documents/obsidian/notes")

          	-- Get all markdown files in the notes directory (non-recursive)
          	local notes = vim.fn.glob(notes_dir .. "/*.md", false, true)

          	if #notes == 0 then
          		vim.notify("No notes found in " .. notes_dir, vim.log.levels.WARN)
          		return
          	end

          	-- Seed random number generator with current time for better randomness
          	math.randomseed(os.time())

          	-- Pick a random note
          	local random_note = notes[math.random(#notes)]

          	-- Open the note
          	vim.cmd("edit " .. vim.fn.fnameescape(random_note))
          end, {
          	bang = false,
          	bar = false,
          	register = false,
          	desc = "Open a random note from the notes folder",
          })

          vim.opt.conceallevel = 2
          -- remove default obsidian.nvim keybinds
          vim.api.nvim_create_autocmd("User", {
            pattern = "ObsidianNoteEnter",
            callback = function(ev)
              vim.keymap.del("n", "<CR>", { buffer = true })
              vim.keymap.del("n", "]o", { buffer = true })
              vim.keymap.del("n", "[o", { buffer = true })
              vim.keymap.set("n", "gd", follow_link, { buffer = true, desc = "Obsidian follow link" })
            end,
          })

          local open_today = function()
          	open_daily_offset(0)
          end
          -- offset -1 rather than daily.yesterday(), which honours workdays_only
          local open_yesterday = function()
          	open_daily_offset(-1)
          end
          vim.keymap.set("n", "<leader>od", open_today, { desc = "[O]bsidian [D]aily note for Today" })
          vim.keymap.set("n", "<leader>odt", open_today, { desc = "[O]bsidian [D]aily note for [T]oday" })
          vim.keymap.set("n", "<leader>oy", open_yesterday, { desc = "[O]bsidian [Y]esterday note" })
          vim.keymap.set("n", "<leader>ody", open_yesterday, { desc = "[O]bsidian [D]aily [Y]esterday note" })
          vim.keymap.set("n", "<leader>odr", function()
          	vim.cmd("Obsidian dailies -30 7")
          end, { desc = "[O]bsidian [D]aily [R]ecent" })
          -- <Plug> mappings so ObsidianNextDay/ObsidianPrevDay are dot-repeatable via vim-repeat
          vim.keymap.set("n", "<Plug>(ObsidianNextDay)", function()
          	if offset_daily(1) then
          		pcall(
          			vim.fn["repeat#set"],
          			vim.api.nvim_replace_termcodes("<Plug>(ObsidianNextDay)", true, true, true)
          		)
          	end
          end, { silent = true, desc = "Obsidian next daily note (repeatable)" })
          vim.keymap.set("n", "<Plug>(ObsidianPrevDay)", function()
          	if offset_daily(-1) then
          		pcall(
          			vim.fn["repeat#set"],
          			vim.api.nvim_replace_termcodes("<Plug>(ObsidianPrevDay)", true, true, true)
          		)
          	end
          end, { silent = true, desc = "Obsidian previous daily note (repeatable)" })

          -- keybindings for custom commands
          vim.keymap.set("n", "<leader>on", "<Plug>(ObsidianNextDay)", { remap = true, desc = "[O]bsidian [N]ext Daily Note" })
          vim.keymap.set("n", "<leader>odn", "<Plug>(ObsidianNextDay)", { remap = true, desc = "[O]bsidian [D]aily [N]ext Note" })
          vim.keymap.set("n", "<leader>op", "<Plug>(ObsidianPrevDay)", { remap = true, desc = "[O]bsidian [P]revious Daily Note" })
          vim.keymap.set("n", "<leader>odp", "<Plug>(ObsidianPrevDay)", { remap = true, desc = "[O]bsidian [D]aily [P]revious Note" })
          vim.keymap.set("n", "<leader>or", vim.cmd.ObsidianRandomNote, { desc = "[O]bsidian [R]andom Note" })

          vim.keymap.set("n", "<leader>ob", "<cmd>Obsidian backlinks<cr>", { desc = "[O]bsidian [B]acklinks" })
          vim.keymap.set("n", "<leader>oc", "<cmd>Obsidian toggle_checkbox<cr>", { desc = "[O]bsidian [C]heckbox" })
          vim.keymap.set("n", "<leader>of", follow_link, { desc = "[O]bsidian [F]ollow" })
          vim.keymap.set("v", "<leader>ol", "<cmd>Obsidian link_new<cr>", { desc = "[O]bsidian [L]ink (new note from selection)" })
          vim.keymap.set("n", "<leader>oo", "<cmd>Obsidian open<cr>", { desc = "[O]bsidian [O]pen" })
          vim.keymap.set("n", "<leader>osn", "<cmd>Obsidian search<cr>", { desc = "[O]bsidian [S]earch [N]otes" })
          vim.keymap.set("n", "<leader>ost", "<cmd>Obsidian tags<cr>", { desc = "[O]bsidian [S]earch [T]ags" })
          vim.keymap.set("n", "<leader>ot", "<cmd>Obsidian template<cr>", { desc = "[O]bsidian [T]emplate" })
        '';
    }
  ];
}
