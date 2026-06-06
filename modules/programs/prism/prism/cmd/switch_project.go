package cmd

// switch_project.go — project-discovery and repo-handling for prism switch.
//
// Contains the entry type, project list building, and the handlers for bare
// repos, regular repos, clone, and review-session group picking.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/dashboard"
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
	display     string // shown in the list
	path        string // resolved filesystem path (empty for specials)
	special     string // "[dashboard]", "[scratchpad]", "[+ create new worktree]"
	sessionRef  string // when non-empty, selecting this entry attaches to the named tmux session
	reviewGroup string // when non-empty, selecting this entry opens a sub-picker for the review round group
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

// ── worktree second-level picker ──────────────────────────────────────────────

func handleBareRepo(projectPath string, pf *config.ProfilesFile, opts session.Opts, isoCaps container.Capabilities, cfg config.Config) error {
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

	// Review round group entry: open a sub-picker showing the individual agents.
	if chosen.reviewGroup != "" {
		return handleReviewGroupPick(chosen.reviewGroup)
	}

	// Review session entry: attach directly to the named session.
	// PRISM_USE_MUX cutover (#2158): switchClientOrMuxSession routes
	// through the mux daemon when the gate is on and falls back to
	// tmux's switch-client primitive otherwise. Mirrors the gate
	// pattern used by ensureAndSwitch (cmd/switch.go) and runNav.
	if chosen.sessionRef != "" {
		return switchClientOrMuxSession("prism switch", chosen.sessionRef)
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
		// Apply per-path isolation override for the new worktree.
		effIso, effCaps := applyPathIsolationOverride(worktreePath, cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
		if effCaps.NeedsConfigBlob && pf != nil {
			if err := injectContainerConfig(worktreePath, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		if effCaps.NeedsConfigBlob {
			if err := writeHarnessConfigBlobFor(effIso, session.NameFor(worktreePath, projectPath), opts.ConfigContent, "switch"); err != nil {
				return err
			}
		}
		return ensureAndSwitch(worktreePath, projectPath, opts)
	}

	// Apply per-path isolation override for the chosen worktree.
	effIso, effCaps := applyPathIsolationOverride(chosen.path, cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
	if effCaps.NeedsConfigBlob && pf != nil {
		if err := injectContainerConfig(chosen.path, pf, &opts, "prism switch"); err != nil {
			return err
		}
	}
	if effCaps.NeedsConfigBlob {
		if err := writeHarnessConfigBlobFor(effIso, session.NameFor(chosen.path, projectPath), opts.ConfigContent, "switch"); err != nil {
			return err
		}
	}
	return ensureAndSwitch(chosen.path, projectPath, opts)
}

// activeReviewSessionEntries returns picker entries for active review agent
// sessions associated with worktrees in projectPath.
//
// Per-agent review sessions (e.g. <repo>@<branch>~review-N-review-goal) are
// grouped by round into collapsed group entries. Each group entry has reviewGroup
// set to the group key (e.g. "<repo>@<branch>~review-1"); selecting it opens a
// sub-picker with the individual agents for that round.
//
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

	// Group per-agent sessions by review round key.
	// groupSessions maps groupKey → []child entries (sessionRef + state).
	type childInfo struct {
		sessionName string
		state       string
	}
	groupSessions := map[string][]childInfo{}
	groupOrder := []string{} // preserve insertion order for sorted output

	// PRISM_USE_MUX cutover (#2158): use liveSessionPredicate so a
	// mux-hosted session is visible in the picker. tmux.HasSession
	// returns false for every mux session and would silently empty
	// the picker, leaving the round-1 routing fix in handleBareRepo
	// unreachable. Captured once for the whole loop — one round-trip
	// per CLI invocation.
	live := liveSessionPredicate()
	for _, s := range all {
		// Only per-agent review sessions: name must contain "~review-" and
		// resolve to a non-empty ReviewRoundKey.
		rk := dashboard.ReviewRoundKey(s.SessionName)
		if rk == "" {
			continue
		}
		// The session must also be alive in the active substrate (tmux
		// when the gate is off, the mux daemon's session tree when on).
		if !live(s.SessionName) {
			continue
		}
		if _, exists := groupSessions[rk]; !exists {
			groupOrder = append(groupOrder, rk)
		}
		st := s.State
		if st == "" {
			st = "idle"
		}
		groupSessions[rk] = append(groupSessions[rk], childInfo{sessionName: s.SessionName, state: st})
	}

	// Sort group keys for stable output.
	for i := 1; i < len(groupOrder); i++ {
		key := groupOrder[i]
		j := i - 1
		for j >= 0 && groupOrder[j] > key {
			groupOrder[j+1] = groupOrder[j]
			j--
		}
		groupOrder[j+1] = key
	}

	// Build one collapsed entry per group.
	var entries []entry
	for _, rk := range groupOrder {
		children := groupSessions[rk]
		// Compute escalated state.
		states := make([]string, len(children))
		for i, ch := range children {
			states[i] = ch.state
		}
		esc := dashboard.EscalatedState(states)
		if esc == "" {
			esc = "idle"
		}
		// Display label: extract "~review-N" portion from the group key.
		label := rk
		if idx := strings.Index(rk, "~review-"); idx >= 0 {
			label = rk[idx:] // e.g. "~review-1"
		}
		display := fmt.Sprintf("%s  [%s]", label, esc)
		entries = append(entries, entry{
			display:     display,
			reviewGroup: rk,
		})
	}

	return entries
}

// handleReviewGroupPick opens a sub-picker showing the individual agents for a
// review round group, then attaches to the chosen agent's tmux session.
// groupKey is the group key (e.g. "nixos-config@feature~review-1").
func handleReviewGroupPick(groupKey string) error {
	// Re-query the DB for child sessions under this group key.
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Find the repo prefix of the group key.
	repoName := groupKey
	if idx := strings.Index(groupKey, "@"); idx >= 0 {
		repoName = groupKey[:idx]
	}

	all, err := d.AllActiveStatusForRepo(repoName)
	if err != nil {
		return fmt.Errorf("query sessions: %w", err)
	}

	// PRISM_USE_MUX cutover (#2158): same liveness predicate as
	// reviewGroupEntries — see the comment there for why this cannot
	// stay as tmux.HasSession.
	live := liveSessionPredicate()
	var items []entry
	for _, s := range all {
		rk := dashboard.ReviewRoundKey(s.SessionName)
		if rk != groupKey {
			continue
		}
		if !live(s.SessionName) {
			continue
		}
		// Label: e.g. "~review-1-review-goal"
		label := s.SessionName
		if idx := strings.Index(s.SessionName, "~review-"); idx >= 0 {
			label = s.SessionName[idx:]
		}
		st := s.State
		if st == "" {
			st = "idle"
		}
		display := fmt.Sprintf("%s  [%s]", label, st)
		items = append(items, entry{
			display:    display,
			sessionRef: s.SessionName,
		})
	}

	// Sort alphabetically.
	for i := 1; i < len(items); i++ {
		key := items[i]
		j := i - 1
		for j >= 0 && items[j].display > key.display {
			items[j+1] = items[j]
			j--
		}
		items[j+1] = key
	}

	if len(items) == 0 {
		return nil // No live agents; do nothing.
	}

	chosen := pick("agent> ", items)
	if chosen == nil || chosen.sessionRef == "" {
		return nil
	}

	// PRISM_USE_MUX cutover (#2158): same gate pattern as the
	// review-session-entry path above.
	return switchClientOrMuxSession("prism switch", chosen.sessionRef)
}

func handleRegularRepo(path string, pf *config.ProfilesFile, opts session.Opts, isoCaps container.Capabilities, cfg config.Config) error {
	exclude := switchWorktreeExcludeSet()
	if exclude[filepath.Base(path)] {
		// Apply per-path isolation override for excluded-from-conversion paths.
		effIso, effCaps := applyPathIsolationOverride(path, cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
		if effCaps.NeedsConfigBlob && pf != nil {
			if err := injectContainerConfig(path, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		if effCaps.NeedsConfigBlob {
			if err := writeHarnessConfigBlobFor(effIso, session.NameFor(path, ""), opts.ConfigContent, "switch"); err != nil {
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
			// Apply per-path isolation override for the fallback direct-open.
			effIso, effCaps := applyPathIsolationOverride(path, cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
			if effCaps.NeedsConfigBlob && pf != nil {
				if err := injectContainerConfig(path, pf, &opts, "prism switch"); err != nil {
					return err
				}
			}
			if effCaps.NeedsConfigBlob {
				if err := writeHarnessConfigBlobFor(effIso, session.NameFor(path, ""), opts.ConfigContent, "switch"); err != nil {
					return err
				}
			}
			return ensureAndSwitch(path, "", opts)
		}

		// Conversion succeeded — clean up the pre-conversion session,
		// mirroring the pattern used by cleanup.go for intentional teardowns.
		// PRISM_USE_MUX cutover (#2158): under the gate the pre-conversion
		// session is owned by the mux daemon, not tmux — destroy it via
		// the daemon's API (cascades PTY teardown). Without this gate,
		// `prism switch` after a bare-repo conversion under PRISM_USE_MUX=1
		// leaves an orphan row in the daemon's session tree.
		if muxCutoverEnabled() {
			_ = session.TeardownMuxSession(oldSessionName)
		} else {
			_ = tmux.KillSession(oldSessionName)
		}
		// Kill any sidecar process associated with the old session (no-op if
		// no PID file exists).
		session.KillSidecar(oldSessionName)
		// Release the port allocation, mark the DB row as ended, purge
		// undelivered bus messages, and clean up any container. All
		// operations are best-effort: errors are logged but do not prevent
		// the switch to the new worktree session.
		if d, dbErr := openDB(); dbErr == nil {
			if isolationModeFromDB(d, oldSessionName) != "host" {
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

		// Apply per-path isolation override for the converted worktree.
		effIso, effCaps := applyPathIsolationOverride(worktreePath, cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
		if effCaps.NeedsConfigBlob && pf != nil {
			if err := injectContainerConfig(worktreePath, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		if effCaps.NeedsConfigBlob {
			if err := writeHarnessConfigBlobFor(effIso, session.NameFor(worktreePath, path), opts.ConfigContent, "switch"); err != nil {
				return err
			}
		}
		return ensureAndSwitch(worktreePath, path, opts)
	default:
		// Apply per-path isolation override for direct-open.
		effIso, effCaps := applyPathIsolationOverride(path, cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
		if effCaps.NeedsConfigBlob && pf != nil {
			if err := injectContainerConfig(path, pf, &opts, "prism switch"); err != nil {
				return err
			}
		}
		if effCaps.NeedsConfigBlob {
			if err := writeHarnessConfigBlobFor(effIso, session.NameFor(path, ""), opts.ConfigContent, "switch"); err != nil {
				return err
			}
		}
		return ensureAndSwitch(path, "", opts)
	}
}

// ── clone repo ────────────────────────────────────────────────────────────────

func handleCloneRepo(pf *config.ProfilesFile, opts session.Opts, isoCaps container.Capabilities, cfg config.Config) error {
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
	// Apply per-path isolation override for the cloned worktree. In practice
	// the worktree lives under a project location (e.g. ~/code/<name>/main)
	// and is unlikely to match an override key, but the check is applied
	// consistently at every session-creation entry point.
	effIso, effCaps := applyPathIsolationOverride(worktrees[0], cfg, &opts, config.IsolationMode(opts.IsolationMode), isoCaps, pf)
	if effCaps.NeedsConfigBlob && pf != nil {
		if err := injectContainerConfig(worktrees[0], pf, &opts, "prism switch"); err != nil {
			return err
		}
	}
	if effCaps.NeedsConfigBlob {
		if err := writeHarnessConfigBlobFor(effIso, session.NameFor(worktrees[0], targetDir), opts.ConfigContent, "switch"); err != nil {
			return err
		}
	}
	return ensureAndSwitch(worktrees[0], targetDir, opts)
}
