-- blink.cmp source: completion for Obsidian frontmatter list fields.
-- Loaded through package.preload (see blink-cmp.nix) as "obsidian-frontmatter".
local M = {}

-- One row per completed frontmatter key. Add a field by adding a row.
M.fields = {
	pronouns = { kind = "link", field = "pronoun-set" },
	tags = { kind = "tag" },
}

M.ttl_ms = 30000

-- Every call into obsidian.nvim internals lives in this table. They are not a
-- stable API: a plugin update is fixed here and nowhere else.
local obsidian = {}

---@return string|nil root of the active vault
function obsidian.vault_root()
	local ok, root = pcall(function()
		return tostring(Obsidian.workspace.root)
	end)
	if ok and root and root ~= "" then
		return (root:gsub("/+$", ""))
	end
end

---@param dir string
---@param cb fun(tags: string[]|nil)
function obsidian.find_tags(dir, cb)
	local ok = pcall(function()
		require("obsidian.search").find_tags_async("", function(locations)
			local seen, out = {}, {}
			for _, loc in ipairs(locations or {}) do
				if loc.tag and not seen[loc.tag] then
					seen[loc.tag] = true
					out[#out + 1] = loc.tag
				end
			end
			table.sort(out)
			cb(out)
		end, { dir = dir })
	end)
	if not ok then
		cb(nil)
	end
end

-- Matches the first `field:` line inside the leading frontmatter block only.
-- A line that is exactly `---` closes the block, so a body line cannot match.
local function frontmatter_pattern(field)
	local not_closer = [[(?:[^-\n][^\n]*|-{1,2}(?:[^-\n][^\n]*)?|---[^\n]+)?\n]]
	return [[\A---[ \t]*\n(?:]] .. not_closer .. [[)*?]] .. vim.pesc(field):gsub("%%", "\\") .. [[[ \t]*:[ \t]*([^\n]+)]]
end

local function unquote(value)
	value = vim.trim(value)
	local q = value:sub(1, 1)
	if (q == '"' or q == "'") and value:sub(-1) == q and #value >= 2 then
		value = value:sub(2, -2)
	end
	return value
end

---@param dir string
---@param field string
---@param cb fun(items: table[]|nil)
local function scan_links(dir, field, cb)
	local rg = vim.fn.exepath("rg")
	if rg == "" then
		return cb(nil)
	end
	local cmd = {
		rg, "--multiline", "--only-matching", "--no-messages", "--null", "--no-heading",
		"--with-filename", "--no-line-number", "--glob", "*.md",
		"--replace", "$1", frontmatter_pattern(field), dir,
	}
	local ok = pcall(vim.system, cmd, { text = true }, function(res)
		vim.schedule(function()
			-- rg exits 1 for "no matches" and 2 for partial errors; keep stdout either way.
			if res.code > 2 then
				return cb(nil)
			end
			local items = {}
			for line in (res.stdout or ""):gmatch("[^\n]+") do
				local path, value = line:match("^(.-)%z(.*)$")
				value = value and unquote(value)
				if path and value ~= "" then
					items[#items + 1] = { stem = vim.fn.fnamemodify(path, ":t:r"), value = value }
				end
			end
			cb(items)
		end)
	end)
	if not ok then
		cb(nil)
	end
end

-- cache[kind_key] = { items = table|nil, at = ms, pending = callbacks|nil }
local cache = {}

local function now()
	return vim.uv.now()
end

local function refresh(key, dir, spec, cb)
	local entry = cache[key] or {}
	cache[key] = entry
	if cb then
		entry.waiting = entry.waiting or {}
		table.insert(entry.waiting, cb)
	end
	if entry.loading then
		return
	end
	entry.loading = true
	local function done(items)
		entry.loading = false
		if items then
			entry.items = items
			entry.at = now()
		end
		local waiting = entry.waiting or {}
		entry.waiting = nil
		for _, w in ipairs(waiting) do
			w(entry.items or {})
		end
	end
	if spec.kind == "link" then
		scan_links(dir, spec.field, done)
	else
		obsidian.find_tags(dir, done)
	end
end

-- Serves from cache. Starts an async refresh when the cache is missing or stale.
local function get_items(key, dir, spec, cb)
	local entry = cache[key]
	if entry and entry.items then
		if now() - entry.at > M.ttl_ms then
			refresh(key, dir, spec)
		end
		return cb(entry.items)
	end
	refresh(key, dir, spec, cb)
end

local function in_vault(bufnr)
	local root = obsidian.vault_root()
	if not root then
		return nil
	end
	local name = vim.api.nvim_buf_get_name(bufnr)
	if name:sub(1, #root + 1) ~= root .. "/" then
		return nil
	end
	return root
end

--- Returns the owning key and the 0-based column where the list item text starts.
---@return string|nil, integer|nil
local function list_context(bufnr, row, col)
	local lines = vim.api.nvim_buf_get_lines(bufnr, 0, row, false)
	if lines[1] == nil or not lines[1]:match("^%-%-%-%s*$") then
		return nil
	end
	for i = 2, row - 1 do
		if lines[i]:match("^%-%-%-%s*$") or lines[i]:match("^%.%.%.%s*$") then
			return nil -- frontmatter closed above the cursor
		end
	end
	local cur = lines[row]
	if not cur then
		return nil
	end
	local prefix = cur:sub(1, col)
	local dash = prefix:match("^%s*%-%s+")
	if not dash then
		return nil
	end
	for i = row - 1, 2, -1 do
		local key = lines[i]:match("^([%w_%-]+):%s*$")
		if key then
			return key, #dash
		end
		if not (lines[i]:match("^%s*%-") or lines[i]:match("^%s*$")) then
			return nil
		end
	end
end

local Source = {}

function Source.new()
	return setmetatable({}, { __index = Source })
end

function Source:enabled()
	return vim.bo.filetype == "markdown"
end

function Source:get_trigger_characters()
	return { " " }
end

function Source:get_completions(ctx, callback)
	local empty = function()
		callback({ items = {}, is_incomplete_forward = false, is_incomplete_backward = false })
	end
	local bufnr = ctx.bufnr
	local row, col = ctx.cursor[1], ctx.cursor[2]
	local dir = in_vault(bufnr)
	if not dir then
		return empty()
	end
	local key, start_col = list_context(bufnr, row, col)
	local spec = key and M.fields[key]
	if not spec then
		return empty()
	end

	local line = vim.api.nvim_buf_get_lines(bufnr, row - 1, row, false)[1] or ""
	local trailing = #line:sub(col + 1):match('^[%]"]*')
	local range = {
		start = { line = row - 1, character = start_col },
		["end"] = { line = row - 1, character = col + trailing },
	}
	local Kind = vim.lsp.protocol.CompletionItemKind

	get_items(key, dir, spec, function(data)
		local items = {}
		for _, d in ipairs(data) do
			local label, text, detail, kind
			if spec.kind == "link" then
				label = d.value
				text = string.format('"[[%s|%s]]"', d.stem, d.value)
				detail = d.stem
				kind = Kind.Reference
			else
				label, text, kind = d, d, Kind.Keyword
			end
			items[#items + 1] = {
				label = label,
				filterText = label,
				kind = kind,
				labelDetails = detail and { description = detail } or nil,
				textEdit = { newText = text, range = range },
			}
		end
		callback({ items = items, is_incomplete_forward = false, is_incomplete_backward = false })
	end)
end

M.new = Source.new
M._cache = cache
M._obsidian = obsidian
return M
