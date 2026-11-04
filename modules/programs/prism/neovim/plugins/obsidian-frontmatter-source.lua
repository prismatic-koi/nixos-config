-- blink.cmp source: completion for Obsidian frontmatter list fields.
-- Loaded through package.preload (see blink-cmp.nix) as "obsidian-frontmatter".
local M = {}

-- One row per completed frontmatter key. Add a field by adding a row.
M.fields = {
	pronouns = { kind = "link", field = "pronoun-sets" },
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

---@param text string frontmatter text without the --- fences
---@return table|nil
function obsidian.parse_yaml(text)
	local ok, parsed = pcall(function()
		return require("obsidian.yaml").loads(text)
	end)
	if ok and type(parsed) == "table" then
		return parsed
	end
end

-- Matches files whose leading frontmatter block holds a `field:` key.
-- A line that is exactly `---` closes the block, so a body line cannot match.
local function frontmatter_pattern(field)
	local not_closer = [[(?:[^-\n][^\n]*|-{1,2}(?:[^-\n][^\n]*)?|---[^\n]+)?\n]]
	return [[\A---[ \t]*\n(?:]] .. not_closer .. [[)*?]] .. vim.pesc(field):gsub("%%", "\\") .. [[[ \t]*:]]
end

---@return string|nil text of the leading frontmatter block
local function frontmatter_text(content)
	local lines = vim.split(content, "\n", { plain = true })
	if not (lines[1] or ""):match("^%-%-%-%s*$") then
		return nil
	end
	local out = {}
	for i = 2, #lines do
		if lines[i]:match("^%-%-%-%s*$") or lines[i]:match("^%.%.%.%s*$") then
			return table.concat(out, "\n")
		end
		out[#out + 1] = lines[i]
	end
end

-- Non-blocking whole-file read. Calls cb(nil) on any error.
local function read_file(path, cb)
	local uv = vim.uv
	uv.fs_open(path, "r", 438, function(oerr, fd)
		if oerr or not fd then
			return cb(nil)
		end
		uv.fs_fstat(fd, function(serr, stat)
			if serr or not stat then
				uv.fs_close(fd, function() end)
				return cb(nil)
			end
			uv.fs_read(fd, stat.size, 0, function(rerr, data)
				uv.fs_close(fd, function() end)
				cb(not rerr and data or nil)
			end)
		end)
	end)
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
		rg, "--multiline", "--files-with-matches", "--no-messages", "--null",
		"--glob", "*.md", frontmatter_pattern(field), dir,
	}
	local ok = pcall(vim.system, cmd, { text = true }, function(res)
		-- rg exits 1 for "no matches" and 2 for partial errors; keep stdout either way.
		if res.code > 2 then
			return vim.schedule(function()
				cb(nil)
			end)
		end
		local paths = {}
		for path in (res.stdout or ""):gmatch("([^%z]+)%z") do
			paths[#paths + 1] = path
		end
		local items, pending = {}, #paths
		local function finish()
			table.sort(items, function(x, y)
				if x.value ~= y.value then
					return x.value < y.value
				end
				return x.stem < y.stem
			end)
			cb(items)
		end
		if pending == 0 then
			return vim.schedule(finish)
		end
		for _, path in ipairs(paths) do
			read_file(path, function(content)
				vim.schedule(function()
					local fm = content and frontmatter_text(content)
					local parsed = fm and obsidian.parse_yaml(fm)
					local raw = parsed and parsed[field]
					if type(raw) ~= "table" then
						raw = { raw }
					end
					for _, v in ipairs(raw) do
						if type(v) == "string" and vim.trim(v) ~= "" then
							items[#items + 1] = { stem = vim.fn.fnamemodify(path, ":t:r"), value = vim.trim(v) }
						end
					end
					pending = pending - 1
					if pending == 0 then
						finish()
					end
				end)
			end)
		end
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
			local label, text, detail, doc, kind
			if spec.kind == "link" then
				label = d.value
				text = string.format('"[[%s|%s]]"', d.stem, d.value)
				detail = d.stem
				doc = "Note: " .. d.stem
				kind = Kind.Reference
			else
				label, text, kind = d, d, Kind.Keyword
			end
			items[#items + 1] = {
				label = label,
				filterText = label,
				kind = kind,
				documentation = doc,
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
