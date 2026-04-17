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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
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
	display    string // shown in the list
	path       string // resolved filesystem path (empty for specials)
	special    string // "[dashboard]", "[scratchpad]", "[+ create new worktree]"
	sessionRef string // when non-empty, selecting this entry attaches to the named tmux session
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

// injectContainerConfig loads the role-specific opencode.json blob from
// profiles.json and sets opts.ConfigContent when container mode is active.
// This mirrors the pattern in spawn.go and must be called after the final
// worktree path is known (path is the directory passed to ensureAndSwitch).
//
// pf must be non-nil when called; callers are responsible for loading it when
// cfg.ContainerMode is true.
func injectContainerConfig(path string, pf *config.ProfilesFile, opts *session.Opts, cmdName string) error {
	effectiveRole := session.DefaultAgent(path, opts.Agent)
	roleConfig, err := config.ContainerConfigForRole(pf, effectiveRole)
	if err != nil {
		return err
	}
	if roleConfig != "" {
		opts.ConfigContent = roleConfig
	} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
		fmt.Fprintf(os.Stderr, "[%s] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", cmdName, effectiveRole)
	}
	return nil
}

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

	// Open the DB for the startup guard, instance ID generation, and port
	// allocation. The DB handle is passed into opts.DB so that session.Create
	// can check whether an existing tmux session with the same name is a live
	// instance (last_seen within 60s).
	d, dbErr := openDB()
	if dbErr != nil {
		// Non-fatal: log and continue without a DB. The startup guard will fall
		// back to the simple HasSession check (legacy no-op behaviour).
		fmt.Fprintf(os.Stderr, "warning: could not open DB for startup guard: %v\n", dbErr)
	} else {
		defer d.Close()
		opts.DB = d
	}

	// Early-exit for the attach-to-live case on the switch/launch path
	// (ForceFresh=false). If the session already exists in tmux and its DB
	// row shows it is live (last_seen within 60s), skip all session-setup
	// side-effects (UpsertStatus, SetInstanceID, port allocation) and go
	// straight to Attach so the DB row is left intact.
	//
	// This check must mirror the liveness logic in session.startupGuardKillOld
	// so that ensureAndSwitch and session.Create agree on whether a session
	// is live — keeping them in sync prevents a TOCTOU race where
	// ensureAndSwitch pre-generates state for a session that Create then
	// decides not to kill.
	if !opts.ForceFresh && tmux.HasSession(sessionName) {
		isLive := false
		if d != nil {
			if st, stErr := d.CurrentStatus(sessionName); stErr == nil && st != nil {
				isLive = time.Since(st.LastSeen) < 60*time.Second
			}
		}
		if isLive || d == nil {
			// Live session (or no DB to check) — attach without touching DB state.
			if opts.Headless {
				return nil
			}
			return session.Attach(sessionName)
		}
		// Stale or zombie session — fall through to kill+recreate below.
	}

	// Pre-generate a UUID instance_id and write it to agent_status before
	// starting the sidecar. The sidecar is launched inside session.Create
	// (before tmux-session-start fires), so the instance_id must be in the DB
	// before the sidecar reads it. We also pass it via opts.InstanceID so
	// StartSidecarWithOpts can forward it as --instance-id to the sidecar
	// process without needing a DB read.
	//
	// This block is reached only for new sessions or stale zombies (the live
	// early-exit above handles the attach-to-live case).
	if d != nil && opts.Layout == session.LayoutFull {
		instanceID := uuid.New().String()
		// Ensure the agent_status row exists before writing instance_id.
		// allocatePortForSession already upserts it, but call UpsertStatus
		// here defensively in case port allocation is skipped.
		repo := deriveRepo(directory)
		if repo == "" {
			if idx := strings.Index(sessionName, "@"); idx > 0 {
				repo = sessionName[:idx]
			}
		}
		_ = d.UpsertStatus(sessionName, repo, directory, "idle", nil, nil)
		if err := d.SetInstanceID(sessionName, instanceID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not set instance_id for %q: %v\n", sessionName, err)
		} else {
			opts.InstanceID = instanceID
		}
	}

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

func handleBareRepo(projectPath string, pf *config.ProfilesFile, opts session.Opts) error {
	worktrees := git.Worktrees(projectPath)
	createNew := entry{display: "[+ create new worktree]", special: "[+ create new worktree]"}

	var items []entry
	for _, w := range worktrees {
		items = append(items, entry{display: filepath.Base(w), path: w})
	}

	// Include any active review sessions for worktrees in this bare repo.
	// These are per-agent top-level sessions named <repo>@<branch>~review-N-<agent>.
	// They appear as selectable entries in the picker; selecting one attaches
	// directly to that tmux session without creating a new one.
	items = append(items, activeReviewSessionEntries(projectPath, worktrees)...)

	items = append(items, createNew)

	chosen := pick("worktree> ", items)
	if chosen == nil {
		return nil
	}

	// Review session entry: attach directly to the named tmux session.
	if chosen.sessionRef != "" {
		client, _ := tmux.CurrentClient()
		if client == "" {
			client = tmux.CallerClient()
		}
		if client != "" {
			return tmux.SwitchClient(client, chosen.sessionRef)
		}
		_, err := tmux.SwitchClientCurrent(chosen.sessionRef)
		return err
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
		if opts.ContainerMode && pf != nil {
			if err := injectContainerConfig(worktreePath, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		return ensureAndSwitch(worktreePath, projectPath, opts)
	}

	if opts.ContainerMode && pf != nil {
		if err := injectContainerConfig(chosen.path, pf, &opts, "prism switch"); err != nil {
			return err
		}
	}
	return ensureAndSwitch(chosen.path, projectPath, opts)
}

// activeReviewSessionEntries returns picker entries for active review agent
// sessions associated with worktrees in projectPath. Each entry has sessionRef
// set to the tmux session name; selecting it attaches to that session directly.
// Returns an empty slice if the DB is unavailable or no review sessions exist.
func activeReviewSessionEntries(projectPath string, worktrees []string) []entry {
	// Derive the repo name from the project path (last component).
	repoName := filepath.Base(projectPath)

	d, err := openDB()
	if err != nil {
		return nil
	}
	defer d.Close()

	// Query all active sessions for this repo.
	all, err := d.AllActiveStatusForRepo(repoName)
	if err != nil {
		return nil
	}

	var entries []entry
	for _, s := range all {
		// Only include review agent sessions: name must contain "~review-"
		// followed by a round number and agent name.
		if !strings.Contains(s.SessionName, "~review-") {
			continue
		}
		// Verify it matches the new per-agent shape: <repo>@<branch>~review-N-<agent>
		// The session must also exist in tmux (not just the DB).
		if !tmux.HasSession(s.SessionName) {
			continue
		}
		// Extract the label: everything from the first "~review-" onwards.
		label := s.SessionName
		if idx := strings.Index(s.SessionName, "~review-"); idx >= 0 {
			label = s.SessionName[idx:] // e.g. "~review-1-review-goal"
		}
		state := s.State
		if state == "" {
			state = "idle"
		}
		display := fmt.Sprintf("%s  [%s]", label, state)
		entries = append(entries, entry{
			display:    display,
			sessionRef: s.SessionName,
		})
	}

	// Sort entries alphabetically by display label for stable output.
	for i := 1; i < len(entries); i++ {
		key := entries[i]
		j := i - 1
		for j >= 0 && entries[j].display > key.display {
			entries[j+1] = entries[j]
			j--
		}
		entries[j+1] = key
	}

	return entries
}

func handleRegularRepo(path string, pf *config.ProfilesFile, opts session.Opts) error {
	exclude := switchWorktreeExcludeSet()
	if exclude[filepath.Base(path)] {
		if opts.ContainerMode && pf != nil {
			if err := injectContainerConfig(path, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
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
			if opts.ContainerMode && pf != nil {
				if err := injectContainerConfig(path, pf, &opts, "prism switch"); err != nil {
					return err
				}
			}
			return ensureAndSwitch(path, "", opts)
		}

		// Conversion succeeded — clean up the pre-conversion session,
		// mirroring the pattern used by cleanup.go for intentional teardowns.
		// Kill the tmux session if it exists; ignore "no such session" errors.
		_ = tmux.KillSession(oldSessionName)
		// Kill any sidecar process associated with the old session (no-op if
		// no PID file exists).
		session.KillSidecar(oldSessionName)
		// Release the port allocation, mark the DB row as ended, purge
		// undelivered bus messages, and clean up any container. All
		// operations are best-effort: errors are logged but do not prevent
		// the switch to the new worktree session.
		if d, dbErr := openDB(); dbErr == nil {
			if !hostModeFromDB(d, oldSessionName) {
				removeContainerIfExists(oldSessionName)
			}
			if releaseErr := d.ReleasePort(oldSessionName); releaseErr != nil {
				fmt.Fprintf(os.Stderr, "warning: release port for old session %q: %v\n", oldSessionName, releaseErr)
			}
			_ = d.SetEnded(oldSessionName)
			_ = d.PurgeBusMessages(oldSessionName)
			d.Close()
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not open DB to clean up old session %q: %v\n", oldSessionName, dbErr)
			removeContainerIfExists(oldSessionName)
		}

		if opts.ContainerMode && pf != nil {
			if err := injectContainerConfig(worktreePath, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		return ensureAndSwitch(worktreePath, path, opts)
	default:
		if opts.ContainerMode && pf != nil {
			if err := injectContainerConfig(path, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		return ensureAndSwitch(path, "", opts)
	}
}

// ── clone repo ────────────────────────────────────────────────────────────────

func handleCloneRepo(pf *config.ProfilesFile, opts session.Opts) error {
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
	if opts.ContainerMode && pf != nil {
		if err := injectContainerConfig(worktrees[0], pf, &opts, "prism switch"); err != nil {
			return err
		}
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

		// Load profiles.json for container config injection (container mode)
		// and agent env var injection (host mode). Always attempt to load;
		// treat missing file as fatal only in container mode.
		var pf *config.ProfilesFile
		{
			var pfErr error
			pf, pfErr = config.LoadProfiles()
			if pfErr != nil {
				if cfg.ContainerMode {
					return pfErr
				}
				fmt.Fprintf(os.Stderr, "[prism switch] warning: could not load profiles.json (agent env vars will not be injected): %v\n", pfErr)
				pf = nil
			}
		}

		opts := session.Opts{
			Fresh:          fresh,
			ContainerMode:  cfg.ContainerMode,
			PluginHostPath: cfg.SidecarPluginPath,
		}
		// AgentEnvVars only applies to host-mode sessions; container sessions
		// receive env vars via podman --env flags in the sidecar.
		if pf != nil && !cfg.ContainerMode {
			opts.AgentEnvVars = pf.AgentEnvVars
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
				o := opts
				if cfg.ContainerMode && pf != nil {
					if err := injectContainerConfig(worktrees[0], pf, &o, "prism switch"); err != nil {
						return err
					}
				}
				return ensureAndSwitch(worktrees[0], p, o)
			}
			if bareRoot := git.BareRoot(p); bareRoot != "" {
				o := opts
				if cfg.ContainerMode && pf != nil {
					if err := injectContainerConfig(p, pf, &o, "prism switch"); err != nil {
						return err
					}
				}
				return ensureAndSwitch(p, bareRoot, o)
			}
			o := opts
			if cfg.ContainerMode && pf != nil {
				if err := injectContainerConfig(p, pf, &o, "prism switch"); err != nil {
					return err
				}
			}
			return ensureAndSwitch(p, "", o)
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
			return handleCloneRepo(pf, opts)

		default:
			p := chosen.path
			switch {
			case git.IsBareRepo(p):
				return handleBareRepo(p, pf, opts)
			case git.IsRegularRepo(p):
				return handleRegularRepo(p, pf, opts)
			default:
				o := opts
				if cfg.ContainerMode && pf != nil {
					if err := injectContainerConfig(p, pf, &o, "prism switch"); err != nil {
						return err
					}
				}
				return ensureAndSwitch(p, "", o)
			}
		}
	},
}

func init() {
	switchCmd.Flags().String("path", "", "Open a specific path directly (skip picker)")
	switchCmd.Flags().Bool("fresh", false, "Start a fresh opencode session, ignoring any stored session ID")
	rootCmd.AddCommand(switchCmd)
}
