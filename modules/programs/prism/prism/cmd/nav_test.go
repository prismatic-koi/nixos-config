// Package cmd tests for runNav — specifically the tmux switch-client call-site
// pattern introduced by the fix for issue #1806.
//
// runNav must call `tmux switch-client -c <client> -t <target>` (the explicit
// -c form) whenever tmux.CurrentClient() returns a non-empty name.  When it
// returns empty, it must fall back to `tmux switch-client -t <target>` (no -c).
//
// The test uses a custom spy tmux stub (withNavSpy) that:
//   - records all invocations to a log file (one arg per line, invocations
//     separated by blank lines — the same format as withSpyTmux)
//   - returns a configured session name for `display-message -p #{session_name}`
//   - returns a configured client name for `display-message -p #{client_name}`
//   - exits 0 for `has-session` (so all sessions appear live)
//   - exits 0 for `switch-client` (so the nav call succeeds)
//
// openNavDB is overridden to return an in-memory test DB seeded with two
// spine sessions so that resolveTarget can pick a non-current target.
//
// These tests must NOT be run in parallel — they mutate package-level state
// (tmux.TmuxBin and openNavDB).
package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// withNavSpy installs a custom spy tmux wrapper that records all invocations
// to the returned log file path, and returns the configured sessionName and
// clientName for display-message queries.
//
// The spy exits 0 for all commands (including has-session and switch-client).
// Only call from non-parallel tests — TmuxBin is a package-level global.
func withNavSpy(t *testing.T, sessionName, clientName string) string {
	t.Helper()
	dir := t.TempDir()
	logFile := dir + "/tmux-nav-args"
	wrapperPath := dir + "/tmux"

	// The spy shell script:
	//   1. logs all args (one per line, followed by a blank separator)
	//   2. for display-message invocations, outputs the configured value
	//   3. exits 0 for everything else
	//
	// display-message is called with:
	//   display-message -p #{session_name}  → CurrentSession()
	//   display-message -p #{client_name}   → CurrentClient()
	//
	// We distinguish the two by checking $3 (the format string argument).
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + logFile + "; done\n" +
		"printf '\\n' >> " + logFile + "\n" +
		"case \"$1\" in\n" +
		"  display-message)\n" +
		"    case \"$3\" in\n" +
		"      '#{session_name}') printf '%s\\n' " + shellQuote(sessionName) + " ;;\n" +
		"      '#{client_name}')  printf '%s\\n' " + shellQuote(clientName) + " ;;\n" +
		"    esac\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"

	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("withNavSpy: write wrapper: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return logFile
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// seedNavDB opens a fresh DB at a temp path, seeds two "running" sessions,
// and overrides openNavDB to return that DB. Both sessions are registered so
// that the nav resolver has two entries and can pick a target distinct from
// the current one.
func seedNavDB(t *testing.T, sessionA, sessionB string) {
	t.Helper()
	dbPath := t.TempDir() + "/prism-nav-test.db"
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("seedNavDB: open: %v", err)
	}
	for _, name := range []string{sessionA, sessionB} {
		if err := d.UpsertStatus(name, "repo", "/tmp/worktree", "running", nil, nil); err != nil {
			t.Fatalf("seedNavDB: UpsertStatus(%q): %v", name, err)
		}
	}
	d.Close()

	orig := openNavDB
	openNavDB = func() (*db.DB, error) { return db.Open(dbPath) }
	t.Cleanup(func() { openNavDB = orig })
}

// parseNavSpyLog reads the spy log file and returns all switch-client
// invocations as slices of arguments (excluding the "switch-client" command
// name itself), so the caller can inspect what flags were passed.
func parseNavSpyLog(t *testing.T, logFile string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("parseNavSpyLog: ReadFile: %v", err)
	}
	// Each invocation is one-arg-per-line, invocations separated by blank lines.
	// We collect only switch-client invocations.
	var result [][]string
	for _, block := range strings.Split(string(data), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		if lines[0] != "switch-client" {
			continue
		}
		result = append(result, lines[1:]) // args after "switch-client"
	}
	return result
}

// TestRunNav_SwitchClientUsesExplicitClientFlag verifies that runNav passes
// -c <client> -t <target> to tmux switch-client when CurrentClient() returns
// a non-empty client name.
//
// This test FAILS against the pre-fix code (which called SwitchClientCurrent,
// producing switch-client -t <target> without a -c flag).
func TestRunNav_SwitchClientUsesExplicitClientFlag(t *testing.T) {
	// Mutates TmuxBin and openNavDB — must not be parallel.
	const (
		currentSession = "repo@main"
		targetSession  = "repo@feature"
		clientName     = "/dev/pts/42"
	)

	logFile := withNavSpy(t, currentSession, clientName)
	seedNavDB(t, currentSession, targetSession)
	t.Setenv("TMUX", "/tmp/tmux-test,1234,0") // satisfy the $TMUX guard

	if err := runNav(nil, []string{"down"}); err != nil {
		t.Fatalf("runNav: %v", err)
	}

	invocations := parseNavSpyLog(t, logFile)
	if len(invocations) != 1 {
		t.Fatalf("expected exactly 1 switch-client invocation, got %d: %v", len(invocations), invocations)
	}

	args := invocations[0]
	// Assert the explicit -c flag is present with the correct client name.
	// This assertion FAILS against the pre-fix code which produced:
	//   switch-client -t <target>
	// and PASSES against the fix which produces:
	//   switch-client -c <client> -t <target>
	if len(args) < 4 || args[0] != "-c" || args[1] != clientName || args[2] != "-t" || args[3] != targetSession {
		t.Errorf("switch-client args = %v\nwant: [-c %q -t %q]", args, clientName, targetSession)
	}
}

// TestRunNav_SwitchClientFallbackWhenNoClient verifies that runNav falls back
// to switch-client -t <target> (without -c) when CurrentClient() returns empty.
//
// This exercises the single-client / no-client edge case where the tmux
// format expansion fails and returns "".
func TestRunNav_SwitchClientFallbackWhenNoClient(t *testing.T) {
	// Mutates TmuxBin and openNavDB — must not be parallel.
	const (
		currentSession = "repo@main"
		targetSession  = "repo@feature"
		clientName     = "" // empty → falls back to SwitchClientCurrent
	)

	logFile := withNavSpy(t, currentSession, clientName)
	seedNavDB(t, currentSession, targetSession)
	t.Setenv("TMUX", "/tmp/tmux-test,1234,0")

	if err := runNav(nil, []string{"down"}); err != nil {
		t.Fatalf("runNav: %v", err)
	}

	invocations := parseNavSpyLog(t, logFile)
	if len(invocations) != 1 {
		t.Fatalf("expected exactly 1 switch-client invocation, got %d: %v", len(invocations), invocations)
	}

	args := invocations[0]
	// Fallback: switch-client -t <target> (no -c flag).
	if len(args) < 2 || args[0] != "-t" || args[1] != targetSession {
		t.Errorf("switch-client args = %v\nwant: [-t %q] (no -c flag in fallback path)", args, targetSession)
	}
	// Confirm that -c is NOT present.
	for _, a := range args {
		if a == "-c" {
			t.Errorf("switch-client args contain -c but client name was empty: %v", args)
		}
	}
}

// TestRunNav_SilentNoopWhenSingleSession verifies that runNav exits 0 without
// issuing any switch-client call when the spine has only one session (no-op).
func TestRunNav_SilentNoopWhenSingleSession(t *testing.T) {
	// Mutates TmuxBin and openNavDB — must not be parallel.
	const (
		currentSession = "repo@main"
		clientName     = "/dev/pts/7"
	)

	logFile := withNavSpy(t, currentSession, clientName)

	// Seed DB with only one session — resolveTarget returns false.
	dbPath := t.TempDir() + "/prism-nav-noop-test.db"
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.UpsertStatus(currentSession, "repo", "/tmp/worktree", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()
	orig := openNavDB
	openNavDB = func() (*db.DB, error) { return db.Open(dbPath) }
	t.Cleanup(func() { openNavDB = orig })

	t.Setenv("TMUX", "/tmp/tmux-test,1234,0")

	if err := runNav(nil, []string{"down"}); err != nil {
		t.Fatalf("runNav: unexpected error: %v", err)
	}

	invocations := parseNavSpyLog(t, logFile)
	if len(invocations) != 0 {
		t.Errorf("expected 0 switch-client invocations for no-op, got %d: %v", len(invocations), invocations)
	}
}
