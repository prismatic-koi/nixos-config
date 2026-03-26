package cmd

// prism switch — context switcher (replaces cli.tmux.contextSwitcher)
//
// Injected at build time via ldflags:
//
//	SwitchWorktreeExclude  colon-separated repo names to skip bare conversion
//	SwitchProjectLocations colon-separated dirs whose subdirs become entries
//	SwitchProjectSpecific  colon-separated dirs shown as direct entries

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── build-time injected values ────────────────────────────────────────────────

var (
	SwitchWorktreeExclude  = "obsidian"
	SwitchProjectLocations = "~/code"
	SwitchProjectSpecific  = "~/documents/obsidian"
)

func switchWorktreeExcludeSet() map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Split(SwitchWorktreeExclude, ":") {
		if s != "" {
			m[s] = true
		}
	}
	return m
}

func switchProjectLocations() []string {
	var out []string
	for _, s := range strings.Split(SwitchProjectLocations, ":") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func switchProjectSpecific() []string {
	var out []string
	for _, s := range strings.Split(SwitchProjectSpecific, ":") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
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

func (m *pickerModel) refilter() {
	m.cursor = 0
	if m.filter == "" {
		m.matched = m.items
		return
	}
	lower := strings.ToLower(m.filter)
	var out []entry
	for _, e := range m.items {
		if strings.Contains(strings.ToLower(e.display), lower) {
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
	// Selected row: primary fg on a subtle background, full width.
	styleRowSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorPrimary)).
		Background(lipgloss.Color(ColorSecondary)).
		Bold(true).
		Width(m.width)
	styleRowSpecialSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorYellow)).
		Background(lipgloss.Color(ColorSecondary)).
		Bold(true).
		Width(m.width)
	styleRowNormal := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorSecondary)).
		Width(m.width)
	styleRowSpecial := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorYellow)).
		Width(m.width)

	var sb strings.Builder

	// Prompt + filter input — pad with a leading space to match row indent.
	sb.WriteString("\n")
	sb.WriteString(stylePrompt.Render(" " + m.prompt))
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
		// Pad display with a leading space for breathing room.
		text := " " + e.display
		var row string
		if i == m.cursor {
			if e.special != "" {
				row = styleRowSpecialSelected.Render(text)
			} else {
				row = styleRowSelected.Render(text)
			}
		} else {
			if e.special != "" {
				row = styleRowSpecial.Render(text)
			} else {
				row = styleRowNormal.Render(text)
			}
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
	prompt string
	value  string
	done   bool
	width  int
}

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
		case "backspace", "ctrl+h":
			if len(m.value) > 0 {
				runes := []rune(m.value)
				m.value = string(runes[:len(runes)-1])
			}
		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.value += msg.String()
			}
		}
	}
	return m, nil
}

func (m inputModel) View() string {
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleCursor := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleHint := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary)).Italic(true)
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(styleCursor.Render(m.prompt))
	sb.WriteString(m.value)
	sb.WriteString(styleDim.Render("█"))
	sb.WriteString("\n")
	// Sanitised preview on its own line — keeps the cursor on the input line.
	sanitised := git.SanitiseBranch(m.value)
	if sanitised != "" && sanitised != m.value {
		sb.WriteString(styleHint.Render("  → " + sanitised))
		sb.WriteString("\n")
	}
	sb.WriteString(styleDim.Render("  enter confirm  esc cancel"))
	sb.WriteString("\n")
	return sb.String()
}

// promptInput shows a single-line text input and returns the typed string,
// or empty string if cancelled.
func promptInput(prompt string) string {
	m := inputModel{prompt: prompt}
	p := tea.NewProgram(m, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		return ""
	}
	final, ok := result.(inputModel)
	if !ok || !final.done {
		return ""
	}
	return final.value
}

// ── session management ────────────────────────────────────────────────────────

func ensureAndSwitchSession(path string, projectRoot string) error {
	var sessionName string
	var directory string

	if path == "[scratchpad]" {
		sessionName = "scratchpad"
		home, _ := os.UserHomeDir()
		directory = home
	} else {
		directory = expandHome(path)
		if projectRoot != "" {
			projName := strings.ReplaceAll(filepath.Base(projectRoot), ".", "_")
			wtName := strings.ReplaceAll(filepath.Base(directory), ".", "_")
			sessionName = projName + "@" + wtName
		} else {
			sessionName = strings.ReplaceAll(filepath.Base(directory), ".", "_")
		}
	}

	if !tmux.HasSession(sessionName) {
		if err := tmux.NewSessionDetached(sessionName, directory); err != nil {
			return fmt.Errorf("new-session: %w", err)
		}

		if path == "[scratchpad]" {
			_ = tmux.RenameWindow(sessionName+":0", "term")
		} else {
			_ = tmux.RenameWindow(sessionName+":0", "edit")

			// Auto-open nvim on an obvious file.
			nvimCmd := "nvim"
			if des, err := os.ReadDir(directory); err == nil {
				var files []string
				for _, de := range des {
					if !de.IsDir() {
						files = append(files, filepath.Join(directory, de.Name()))
					}
				}
				switch {
				case len(files) == 1:
					nvimCmd = "nvim '" + files[0] + "'"
				case strings.Contains(directory, "obsidian"):
					landing := filepath.Join(directory, "notes", "landingpage.md")
					if _, err := os.Stat(landing); err == nil {
						nvimCmd = "nvim '" + landing + "'"
					}
				default:
					readme := filepath.Join(directory, "README.md")
					if _, err := os.Stat(readme); err == nil {
						nvimCmd = "nvim '" + readme + "'"
					}
				}
			}
			_ = tmux.SendKeys(sessionName+":0", nvimCmd)

			// Window 1: agent.
			_ = tmux.NewWindow(sessionName, 1, "agent", directory)
			_ = tmux.SendKeys(sessionName+":1", "opencode")

			// Window 2: term.
			_ = tmux.NewWindow(sessionName, 2, "term", directory)

			// Focus: obsidian → edit (0), else → agent (1).
			focusIdx := 1
			if strings.Contains(directory, "obsidian") {
				focusIdx = 0
			}
			_ = tmux.SelectWindow(sessionName, focusIdx)
		}
	}

	client, _ := tmux.CurrentClient()
	if client == "" {
		client = tmux.CallerClient()
	}
	if client != "" {
		return tmux.SwitchClient(client, sessionName)
	}
	_, err := tmux.SwitchClientCurrent(sessionName)
	return err
}

// ── worktree second-level picker ──────────────────────────────────────────────

func handleBareRepo(projectPath string) error {
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
		raw := promptInput("branch name> ")
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
		return ensureAndSwitchSession(worktreePath, projectPath)
	}

	return ensureAndSwitchSession(chosen.path, projectPath)
}

func handleRegularRepo(path string) error {
	exclude := switchWorktreeExcludeSet()
	if exclude[filepath.Base(path)] {
		return ensureAndSwitchSession(path, "")
	}

	openDirect := "[open directly (no worktrees)]"
	convert := "[convert to bare+worktree layout]"
	choice := pickString(filepath.Base(path)+" is a regular repo> ", []string{openDirect, convert})
	switch choice {
	case "":
		return nil
	case convert:
		worktreePath, err := git.ConvertToBare(path, func(msg string) {
			fmt.Println(msg)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "conversion failed: %v\nopening directly\n", err)
			return ensureAndSwitchSession(path, "")
		}
		return ensureAndSwitchSession(worktreePath, path)
	default:
		return ensureAndSwitchSession(path, "")
	}
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
				return ensureAndSwitchSession(worktrees[0], p)
			}
			return ensureAndSwitchSession(p, "")
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
			return ensureAndSwitchSession("[scratchpad]", "")

		default:
			p := chosen.path
			switch {
			case git.IsBareRepo(p):
				return handleBareRepo(p)
			case git.IsRegularRepo(p):
				return handleRegularRepo(p)
			default:
				return ensureAndSwitchSession(p, "")
			}
		}
	},
}

func init() {
	switchCmd.Flags().String("path", "", "Open a specific path directly (skip picker)")
	rootCmd.AddCommand(switchCmd)
}
