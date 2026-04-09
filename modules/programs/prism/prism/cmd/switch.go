package cmd

// prism switch — context switcher (replaces cli.tmux.contextSwitcher)
//
// Project layout is read at runtime from ~/.config/prism/config.json
// (or $PRISM_CONFIG_FILE) via the internal/config package.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── runtime config values ─────────────────────────────────────────────────────

func switchWorktreeExcludeSet() map[string]bool {
	m := map[string]bool{}
	for _, s := range config.Load().WorktreeExclude {
		m[s] = true
	}
	return m
}

func switchProjectLocations() []string {
	return config.Load().ProjectLocations
}

func switchProjectSpecific() []string {
	return config.Load().ProjectSpecific
}

// ── project list ──────────────────────────────────────────────────────────────

// entry is a selectable item in the picker.
type entry struct {
	display string // shown in the list
	path    string // resolved filesystem path (empty for specials)
	special string // "[dashboard]", "[scratchpad]", "[+ create new worktree]"
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func projectEntries() []entry {
	var entries []entry
	// Special entries first.
	entries = append(entries,
		entry{display: "[dashboard]", special: "[dashboard]"},
		entry{display: "[scratchpad]", special: "[scratchpad]"},
		entry{display: "[+ clone repo]", special: "[+ clone repo]"},
	)

	// Scan location dirs.
	for _, loc := range switchProjectLocations() {
		dir := expandHome(loc)
		des, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, de := range des {
			if !de.IsDir() {
				continue
			}
			p := filepath.Join(dir, de.Name())
			entries = append(entries, entry{display: p, path: p})
		}
	}

	// Specific dirs.
	for _, spec := range switchProjectSpecific() {
		dir := expandHome(spec)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			entries = append(entries, entry{display: dir, path: dir})
		}
	}

	return entries
}

// ── fuzzy picker model ────────────────────────────────────────────────────────

type pickerModel struct {
	prompt  string
	items   []entry
	matched []entry
	filter  string
	cursor  int
	chosen  *entry
	width   int
	height  int
}

func newPickerModel(prompt string, items []entry) pickerModel {
	m := pickerModel{prompt: prompt, items: items}
	m.matched = items
	return m
}

func (m pickerModel) Init() tea.Cmd { return nil }

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit

		case "enter":
			if len(m.matched) > 0 {
				chosen := m.matched[m.cursor]
				m.chosen = &chosen
			}
			return m, tea.Quit

		case "up", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "ctrl+n":
			if m.cursor < len(m.matched)-1 {
				m.cursor++
			}

		case "backspace", "ctrl+h":
			if len(m.filter) > 0 {
				// Remove last rune.
				runes := []rune(m.filter)
				m.filter = string(runes[:len(runes)-1])
				m.refilter()
			}

		default:
			if msg.Type == tea.KeyRunes {
				m.filter += msg.String()
				m.refilter()
			}
		}
	}
	return m, nil
}

// fuzzyMatch returns true if all runes in pattern appear in s in order.
func fuzzyMatch(s, pattern string) bool {
	s = strings.ToLower(s)
	pattern = strings.ToLower(pattern)
	si := 0
	for _, r := range pattern {
		found := false
		for ; si < len(s); si++ {
			if rune(s[si]) == r {
				si++
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (m *pickerModel) refilter() {
	m.cursor = 0
	if m.filter == "" {
		m.matched = m.items
		return
	}
	var out []entry
	for _, e := range m.items {
		if fuzzyMatch(e.display, m.filter) {
			out = append(out, e)
		}
	}
	m.matched = out
}

func (m pickerModel) View() string {
	if m.width == 0 {
		return ""
	}

	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	stylePrompt := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	// Selected row: ColorPrimary bg, bg0 as text — bright accent bar, dark readable text on top.
	styleRowSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorBg0)).
		Background(lipgloss.Color(ColorPrimary)).
		Bold(true).
		Width(m.width)
	styleRowNormal := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorForeground)).
		Width(m.width)

	var sb strings.Builder

	// Prompt + filter input.
	sb.WriteString("\n")
	sb.WriteString(stylePrompt.Render(" >> "))
	sb.WriteString(m.filter)
	sb.WriteString(styleDim.Render("█"))
	sb.WriteString("\n\n")

	// Visible window of matched items.
	maxVisible := m.height - 6
	if maxVisible < 1 {
		maxVisible = 10
	}
	start := 0
	if m.cursor >= maxVisible {
		start = m.cursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.matched) {
		end = len(m.matched)
	}

	for i := start; i < end; i++ {
		e := m.matched[i]
		text := " " + e.display
		var row string
		if i == m.cursor {
			row = styleRowSelected.Render(text)
		} else {
			row = styleRowNormal.Render(text)
		}
		sb.WriteString(row + "\n")
	}

	if len(m.matched) == 0 {
		sb.WriteString(styleDim.Render(" no matches") + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(styleDim.Render(" ↑/↓ navigate  enter select  esc cancel"))
	sb.WriteString("\n")

	return sb.String()
}

// pick runs the interactive fuzzy picker and returns the chosen entry, or nil.
func pick(prompt string, items []entry) *entry {
	m := newPickerModel(prompt, items)
	p := tea.NewProgram(m, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		return nil
	}
	final, ok := result.(pickerModel)
	if !ok {
		return nil
	}
	return final.chosen
}

// pickString is a convenience wrapper for simple string lists.
func pickString(prompt string, items []string) string {
	entries := make([]entry, len(items))
	for i, s := range items {
		entries[i] = entry{display: s, special: s}
	}
	chosen := pick(prompt, entries)
	if chosen == nil {
		return ""
	}
	return chosen.display
}

// ── single-line text input model ──────────────────────────────────────────────

type inputModel struct {
	prompt       string
	runes        []rune // full input as runes
	cursor       int    // insertion point (0 = before first rune)
	done         bool
	width        int
	sanitiseHint bool // show branch-name sanitise preview
}

func (m inputModel) value() string { return string(m.runes) }

func (m inputModel) Init() tea.Cmd { return nil }

func (m inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		case "left", "ctrl+b":
			if m.cursor > 0 {
				m.cursor--
			}
		case "right", "ctrl+f":
			if m.cursor < len(m.runes) {
				m.cursor++
			}
		case "home", "ctrl+a":
			m.cursor = 0
		case "end", "ctrl+e":
			m.cursor = len(m.runes)
		case "backspace", "ctrl+h":
			if m.cursor > 0 {
				m.runes = append(m.runes[:m.cursor-1], m.runes[m.cursor:]...)
				m.cursor--
			}
		case "delete", "ctrl+d":
			if m.cursor < len(m.runes) {
				m.runes = append(m.runes[:m.cursor], m.runes[m.cursor+1:]...)
			}
		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				ins := []rune(msg.String())
				m.runes = append(m.runes[:m.cursor], append(ins, m.runes[m.cursor:]...)...)
				m.cursor += len(ins)
			}
		}
	}
	return m, nil
}

func (m inputModel) View() string {
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleCursor := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleHint := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary)).Italic(true)
	styleCaret := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorBg0)).Background(lipgloss.Color(ColorPrimary))
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(styleCursor.Render(m.prompt))
	// Render text with a visible cursor block at the insertion point.
	before := string(m.runes[:m.cursor])
	after := string(m.runes[m.cursor:])
	var caretChar string
	if len(after) > 0 {
		caretRunes := []rune(after)
		caretChar = styleCaret.Render(string(caretRunes[0]))
		after = string(caretRunes[1:])
	} else {
		caretChar = styleDim.Render("█")
	}
	sb.WriteString(before)
	sb.WriteString(caretChar)
	sb.WriteString(after)
	sb.WriteString("\n")
	// Sanitised preview on its own line — keeps the cursor on the input line.
	if m.sanitiseHint {
		sanitised := git.SanitiseBranch(m.value())
		if sanitised != "" && sanitised != m.value() {
			sb.WriteString(styleHint.Render("  → " + sanitised))
			sb.WriteString("\n")
		}
	}
	sb.WriteString(styleDim.Render("  enter confirm  esc cancel"))
	sb.WriteString("\n")
	return sb.String()
}

// promptInput shows a single-line text input and returns the typed string,
// or empty string if cancelled.
func promptInput(prompt string) string {
	m := inputModel{prompt: prompt, sanitiseHint: false}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithoutBracketedPaste())
	result, err := p.Run()
	if err != nil {
		return ""
	}
	final, ok := result.(inputModel)
	if !ok || !final.done {
		return ""
	}
	return final.value()
}

// promptBranchInput is like promptInput but shows a branch-name sanitise preview.
func promptBranchInput(prompt string) string {
	m := inputModel{prompt: prompt, sanitiseHint: true}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithoutBracketedPaste())
	result, err := p.Run()
	if err != nil {
		return ""
	}
	final, ok := result.(inputModel)
	if !ok || !final.done {
		return ""
	}
	return final.value()
}

// ── session management ────────────────────────────────────────────────────────

// ensureAndSwitch creates the session if it doesn't exist (with the appropriate
// layout) and then switches the current client to it, unless opts.Headless is set.
// For full-layout sessions, a port is allocated from the 14000–14999 range and
// passed through to opencode via BuildOpencodeCmd.
func ensureAndSwitch(path string, projectRoot string, opts session.Opts) error {
	var sessionName string
	var directory string

	if path == "[scratchpad]" {
		sessionName = "scratchpad"
		home, _ := os.UserHomeDir()
		directory = home
		opts.Layout = session.LayoutScratchpad
		// Set SessionName for consistency with the full-layout branch.
		// LayoutScratchpad does not call BuildOpencodeCmd today, so this has
		// no runtime effect, but keeps the struct complete in case the
		// scratchpad layout ever gains an opencode agent window.
		opts.SessionName = sessionName
	} else {
		directory = expandHome(path)
		sessionName = session.NameFor(directory, projectRoot)
		opts.SessionName = sessionName
		opts.Layout = session.LayoutFull
	}

	opts.Agent = session.DefaultAgent(directory, opts.Agent)

	// Allocate a port for full-layout sessions. The agent_status row must
	// exist before we can write opencode_port to it, so we allocate after
	// session.Create (which seeds agent_status via `prism event
	// tmux-session-start`). However, BuildOpencodeCmd needs the port at
	// session creation time (it's called inside setupFullLayout). To break
	// this ordering dependency, we pre-allocate: seed the DB row first, then
	// allocate the port, then create the tmux session.
	if opts.Layout == session.LayoutFull {
		port, err := allocatePortForSession(sessionName, directory)
		if err != nil {
			// Non-fatal: log and continue without a port. opencode will still
			// work, just without the serve API.
			fmt.Fprintf(os.Stderr, "warning: port allocation failed for %q: %v\n", sessionName, err)
		} else {
			opts.Port = port
		}
	}

	if err := session.Create(sessionName, directory, opts); err != nil {
		return err
	}

	if opts.Headless {
		fmt.Printf("session %q created\n", sessionName)
		return nil
	}

	return session.Attach(sessionName)
}

// allocatePortForSession ensures the agent_status row exists for sessionName
// and then allocates a port from the DB. If the session already has a port
// allocated (e.g. on restore), it returns the existing port.
func allocatePortForSession(sessionName, directory string) (int, error) {
	d, err := openDB()
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Check if a status row already exists and already has a port.
	existing, err := d.CurrentStatus(sessionName)
	if err != nil {
		return 0, fmt.Errorf("current status: %w", err)
	}
	if existing != nil && existing.OpencodePort != nil {
		return *existing.OpencodePort, nil
	}

	// Ensure the agent_status row exists (idempotent upsert).
	repo := deriveRepo(directory)
	if repo == "" {
		// Not inside a project worktree — derive from session name.
		if idx := strings.Index(sessionName, "@"); idx > 0 {
			repo = sessionName[:idx]
		}
	}
	if err := d.UpsertStatus(sessionName, repo, directory, "idle", nil, nil); err != nil {
		return 0, fmt.Errorf("upsert status: %w", err)
	}

	return d.AllocatePort(sessionName)
}

// ── worktree second-level picker ──────────────────────────────────────────────

func handleBareRepo(projectPath string, opts session.Opts) error {
	worktrees := git.Worktrees(projectPath)
	createNew := entry{display: "[+ create new worktree]", special: "[+ create new worktree]"}

	var items []entry
	for _, w := range worktrees {
		items = append(items, entry{display: filepath.Base(w), path: w})
	}
	items = append(items, createNew)

	chosen := pick("worktree> ", items)
	if chosen == nil {
		return nil
	}

	if chosen.special == "[+ create new worktree]" {
		raw := promptBranchInput("branch name> ")
		if raw == "" {
			return nil
		}
		branch := git.SanitiseBranch(raw)
		if branch == "" {
			return fmt.Errorf("branch name is empty after sanitisation")
		}
		worktreePath, err := git.CreateWorktree(projectPath, branch)
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}
		return ensureAndSwitch(worktreePath, projectPath, opts)
	}

	return ensureAndSwitch(chosen.path, projectPath, opts)
}

func handleRegularRepo(path string, opts session.Opts) error {
	exclude := switchWorktreeExcludeSet()
	if exclude[filepath.Base(path)] {
		return ensureAndSwitch(path, "", opts)
	}

	openDirect := "[open directly (no worktrees)]"
	convert := "[convert to bare+worktree layout]"
	choice := pickString(filepath.Base(path)+" is a regular repo> ", []string{openDirect, convert})
	switch choice {
	case "":
		return nil
	case convert:
		// Record the old session name before conversion so we can clean it up
		// afterwards. The pre-conversion session is named after the repo directory
		// with no "@" component (e.g. "dns-management").
		oldSessionName := session.NameFor(path, "")

		worktreePath, err := git.ConvertToBare(path, func(msg string) {
			fmt.Println(msg)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "conversion failed: %v\nopening directly\n", err)
			return ensureAndSwitch(path, "", opts)
		}

		// Conversion succeeded — clean up the pre-conversion session.
		// Kill the tmux session if it exists; ignore "no such session" errors.
		_ = tmux.KillSession(oldSessionName)
		// Mark the DB row as ended and purge any undelivered bus messages.
		// Both operations are best-effort: errors are logged but do not prevent
		// the switch to the new worktree session.
		if d, dbErr := openDB(); dbErr == nil {
			_ = d.SetEnded(oldSessionName)
			_ = d.PurgeBusMessages(oldSessionName)
			d.Close()
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not open DB to clean up old session %q: %v\n", oldSessionName, dbErr)
		}

		return ensureAndSwitch(worktreePath, path, opts)
	default:
		return ensureAndSwitch(path, "", opts)
	}
}

// ── clone repo ────────────────────────────────────────────────────────────────

func handleCloneRepo(opts session.Opts) error {
	repoURL := promptInput("clone url> ")
	if repoURL == "" {
		return nil
	}

	name := repoNameFromURL(repoURL)
	if name == "" {
		return fmt.Errorf("could not parse repository name from URL: %s", repoURL)
	}

	// Clone into the first configured project location.
	locs := switchProjectLocations()
	if len(locs) == 0 {
		return fmt.Errorf("no project locations configured")
	}
	targetDir := filepath.Join(expandHome(locs[0]), name)

	var cloneErr error
	cloneErr = git.CloneWorktree(repoURL, targetDir, func(msg string) {
		fmt.Println(msg)
	})
	if cloneErr != nil {
		return fmt.Errorf("clone failed: %w", cloneErr)
	}

	// Switch to the default branch worktree.
	worktrees := git.Worktrees(targetDir)
	if len(worktrees) == 0 {
		return fmt.Errorf("clone succeeded but no worktrees found in %s", targetDir)
	}
	return ensureAndSwitch(worktrees[0], targetDir, opts)
}

// ── ensure dashboard session ──────────────────────────────────────────────────

func ensureSwitchDashSession() {
	if !tmux.HasSession(dashSession) {
		// Best-effort; ignore errors.
		_ = tmux.NewSessionDetached(dashSession, "")
		// The session's command loop is set up by the tmux binding, not here.
	}
}

// ── cobra command ─────────────────────────────────────────────────────────────

var switchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Context switcher — open or create a project session",
	RunE: func(cmd *cobra.Command, args []string) error {
		pathArg, _ := cmd.Flags().GetString("path")

		// Inside a container: proxy the switch to the host sidecar.
		// --path is the only flag that makes sense in this context.
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			return proxyToHostAPI(apiURL, "/switch", map[string]any{
				"session": pathArg, // host resolves path → session name
			}, nil)
		}

		fresh, _ := cmd.Flags().GetBool("fresh")
		cfg := config.Load()
		opts := session.Opts{
			Fresh:          fresh,
			ContainerMode:  cfg.ContainerMode,
			PluginHostPath: cfg.SidecarPluginPath,
		}

		// --path: open a specific path directly.
		if pathArg != "" {
			p := expandHome(pathArg)
			info, err := os.Stat(p)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("not a directory: %s", pathArg)
			}
			if git.IsBareRepo(p) {
				worktrees := git.Worktrees(p)
				if len(worktrees) == 0 {
					return fmt.Errorf("no worktrees found in %s", p)
				}
				return ensureAndSwitch(worktrees[0], p, opts)
			}
			if bareRoot := git.BareRoot(p); bareRoot != "" {
				return ensureAndSwitch(p, bareRoot, opts)
			}
			return ensureAndSwitch(p, "", opts)
		}

		// Ensure dashboard exists in background.
		ensureSwitchDashSession()

		// Top-level picker.
		entries := projectEntries()
		chosen := pick("project> ", entries)
		if chosen == nil {
			return nil
		}

		switch chosen.special {
		case "[dashboard]":
			ensureSwitchDashSession()
			client, _ := tmux.CurrentClient()
			if client == "" {
				client = tmux.CallerClient()
			}
			if client != "" {
				return tmux.SwitchClient(client, dashSession)
			}
			_, err := tmux.SwitchClientCurrent(dashSession)
			return err

		case "[scratchpad]":
			return ensureAndSwitch("[scratchpad]", "", opts)

		case "[+ clone repo]":
			return handleCloneRepo(opts)

		default:
			p := chosen.path
			switch {
			case git.IsBareRepo(p):
				return handleBareRepo(p, opts)
			case git.IsRegularRepo(p):
				return handleRegularRepo(p, opts)
			default:
				return ensureAndSwitch(p, "", opts)
			}
		}
	},
}

func init() {
	switchCmd.Flags().String("path", "", "Open a specific path directly (skip picker)")
	switchCmd.Flags().Bool("fresh", false, "Start a fresh opencode session, ignoring any stored session ID")
	rootCmd.AddCommand(switchCmd)
}
