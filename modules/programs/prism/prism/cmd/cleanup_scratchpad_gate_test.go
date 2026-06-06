package cmd

// Tests for the scratchpad-swing + SwitchClient gating in cleanup.go
// (#2176). The cutover PR (#2175) fixed the KillSession sites but
// left the sibling scratchpad-ensure + SwitchClient blocks at four
// line ranges (211-225, 672-685, 795-809, 891-901) ungated. Under
// PRISM_USE_MUX=1 those blocks have no useful effect (no tmux client
// to redirect; the renderer handles its own focus) and silently
// re-introduce a tmux dependency the cutover was supposed to
// remove. This test pins the corrected invariant: every
// `tmux.NewSessionDetached("scratchpad"…)` call in cleanup.go is
// inside an `if !muxCutoverEnabled()` block.

import (
	"os"
	"strings"
	"testing"
)

// TestCleanupSites_AllScratchpadSwingsAreGated asserts that every
// scratchpad-ensure block in cleanup.go is guarded by
// `if !muxCutoverEnabled()`. The pre-#2176 sites called
// `tmux.NewSessionDetached("scratchpad", …)` unconditionally; the
// post-#2176 sites must skip when the gate is on.
//
// Structural shape mirrors TestCleanupSites_AllTeardownsAreGated
// (the sibling gate test for tmux.KillSession): scan lines, count
// scratchpad sites, assert each one sits inside a
// `!muxCutoverEnabled()` body.
func TestCleanupSites_AllScratchpadSwingsAreGated(t *testing.T) {
	data, err := os.ReadFile("cleanup.go")
	if err != nil {
		t.Fatalf("read cleanup.go: %v", err)
	}
	src := string(data)
	lines := strings.Split(src, "\n")

	// We look for the marker line that starts each scratchpad-
	// ensure block. Every site in cleanup.go uses this exact
	// shape: tmux.HasSession("scratchpad") inside an `if !`
	// branch. The line above the ensure-body is the guard.
	scratchpadSites := 0
	gated := 0
	for i, ln := range lines {
		// Match the inner ensure call (the unique marker that
		// only appears in scratchpad-ensure blocks). Using
		// NewSessionDetached("scratchpad" pins the four sites
		// without false-positives from other tmux calls.
		if !strings.Contains(ln, `tmux.NewSessionDetached("scratchpad"`) {
			continue
		}
		scratchpadSites++
		// Look back up to 25 lines for the `!muxCutoverEnabled()`
		// guard. The window is generous because the gate may sit
		// above a multi-line comment block.
		low := i - 25
		if low < 0 {
			low = 0
		}
		for j := i - 1; j >= low; j-- {
			if strings.Contains(lines[j], "!muxCutoverEnabled()") {
				gated++
				break
			}
		}
	}
	if scratchpadSites == 0 {
		t.Fatal("no tmux.NewSessionDetached(\"scratchpad\"…) sites found in cleanup.go — test is vacuous")
	}
	if scratchpadSites != 4 {
		// The issue body names the four sibling sites explicitly
		// (lines 211-225, 672-685, 795-809, 891-901). A change in
		// the site count means a new ungated site was added or one
		// was deleted — both warrant a manual review.
		t.Errorf("cleanup.go: found %d scratchpad-ensure sites, expected 4 (per issue #2176 — sibling sites at 211-225, 672-685, 795-809, 891-901)", scratchpadSites)
	}
	if gated != scratchpadSites {
		t.Errorf("cleanup.go: %d/%d scratchpad-ensure sites are gated by !muxCutoverEnabled() — the ungated ones silently re-introduce a tmux dependency under PRISM_USE_MUX=1 (issue #2176)", gated, scratchpadSites)
	}
}
