package cmd

// switch_tui.go — TUI rendering and interaction for prism switch.
//
// Contains the fuzzy picker (pickerModel) and single-line text input
// (inputModel) bubbletea models, plus their convenience wrappers.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/git"
)

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
