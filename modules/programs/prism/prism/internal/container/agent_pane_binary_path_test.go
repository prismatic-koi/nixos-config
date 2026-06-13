package container

// agent_pane_binary_path_test.go — issue #2260 regression test for the
// absolute-path-of-prism that the agent-run pane command must carry.
//
// Pre-#2260, AgentPaneCmd emitted a bare `prism agent-run ...` token; the
// tmux pane shell then PATH-resolved that to whatever lived first on $PATH
// at exec time. On a worker host this is almost always the deployed binary,
// even when the operator's shell has `result/bin` ahead on PATH for a branch
// build — because `prism spawn` creates the tmux session from a non-tmux
// client, inheriting env from the spawn process, NOT from any
// `tmux set-environment` push.
//
// The structural fix is to emit the ABSOLUTE PATH of the currently-running
// prism binary (via os.Executable) into the pane command. This file pins
// that contract for both pane-owned isolation modes (bwrap, sandbox-exec).

import (
	"errors"
	"strings"
	"testing"
)

// TestBwrapAgentPaneCmd_StartsWithAbsolutePrismPath verifies the AgentPaneCmd
// for bwrap begins with a shell-quoted absolute path (starts with `/`),
// followed immediately by ` agent-run --session <name>`. This is the core
// invariant: the tmux shell must exec the binary by absolute path, not by
// PATH lookup.
func TestBwrapAgentPaneCmd_StartsWithAbsolutePrismPath(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/xxxx-prism-0.0.0/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	// Must begin with `'/...` — a shell-quoted absolute path. No bare
	// "prism" token at the start.
	if !strings.HasPrefix(got, "'/") {
		t.Errorf("bwrap pane cmd must start with a shell-quoted absolute path, got %q", got)
	}
	if strings.HasPrefix(got, "prism ") {
		t.Errorf("bwrap pane cmd starts with bare \"prism\" — PATH-shadow class (#2260) re-introduced; got %q", got)
	}
	// Full shape contract.
	want := "'/nix/store/xxxx-prism-0.0.0/bin/prism' agent-run --session 'prism-test@bwrap'"
	if got != want {
		t.Errorf("bwrap pane cmd shape changed: got %q, want %q", got, want)
	}
}

// TestSandboxExecAgentPaneCmd_StartsWithAbsolutePrismPath mirrors the bwrap
// assertion for sandbox-exec. Both pane-owned modes share the same fix.
func TestSandboxExecAgentPaneCmd_StartsWithAbsolutePrismPath(t *testing.T) {
	withFakePrismBinary(t, "/nix/store/yyyy-prism-0.0.0/bin/prism")
	iso := &sandboxExecIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
	})
	if err != nil {
		t.Fatalf("sandbox-exec AgentPaneCmd: unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "'/") {
		t.Errorf("sandbox-exec pane cmd must start with a shell-quoted absolute path, got %q", got)
	}
	if strings.HasPrefix(got, "prism ") {
		t.Errorf("sandbox-exec pane cmd starts with bare \"prism\" — PATH-shadow class (#2260) re-introduced; got %q", got)
	}
	want := "'/nix/store/yyyy-prism-0.0.0/bin/prism' agent-run --session 'prism-test@sbx'"
	if got != want {
		t.Errorf("sandbox-exec pane cmd shape changed: got %q, want %q", got, want)
	}
}

// TestBwrapAgentPaneCmd_BinaryPath_WithSingleQuote_ShellEscaped verifies the
// edge-case AC: a binary path containing a single quote (improbable in a
// real Nix store path, but the shell quoting must still be correct) is
// escaped via the standard '\'' sequence so the rendered command does not
// break out of its single-quote context.
//
// This is defence-in-depth: if a future deployment lands prism under a path
// containing a quote — or a developer's branch build is in such a path — the
// pane command must still parse correctly.
func TestBwrapAgentPaneCmd_BinaryPath_WithSingleQuote_ShellEscaped(t *testing.T) {
	withFakePrismBinary(t, "/tmp/weird's-build/result/bin/prism")
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd: unexpected error: %v", err)
	}
	// The standard '\'' escape: close the single-quote string, emit a
	// literal single quote, reopen the single-quote string. shellQuoteContainer
	// implements this; we assert it actually fired on the binary path.
	want := `'/tmp/weird'\''s-build/result/bin/prism' agent-run --session 'prism-test@bwrap'`
	if got != want {
		t.Errorf("bwrap pane cmd must shell-escape single quote in binary path\ngot:  %q\nwant: %q", got, want)
	}
}

// TestSandboxExecAgentPaneCmd_BinaryPath_WithSingleQuote_ShellEscaped mirrors
// the single-quote-in-path test for sandbox-exec — both pane-owned modes
// share the same shellQuoteContainer helper.
func TestSandboxExecAgentPaneCmd_BinaryPath_WithSingleQuote_ShellEscaped(t *testing.T) {
	withFakePrismBinary(t, "/tmp/weird's-build/result/bin/prism")
	iso := &sandboxExecIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
	})
	if err != nil {
		t.Fatalf("sandbox-exec AgentPaneCmd: unexpected error: %v", err)
	}
	want := `'/tmp/weird'\''s-build/result/bin/prism' agent-run --session 'prism-test@sbx'`
	if got != want {
		t.Errorf("sandbox-exec pane cmd must shell-escape single quote in binary path\ngot:  %q\nwant: %q", got, want)
	}
}

// TestBwrapAgentPaneCmd_BinaryPathError_PropagatesAsError verifies the
// edge-case AC: when os.Executable returns an error, AgentPaneCmd must
// return a non-nil error rather than fall back to a bare "prism" string.
// Silent fallback would re-introduce the PATH-shadow class structurally.
func TestBwrapAgentPaneCmd_BinaryPathError_PropagatesAsError(t *testing.T) {
	sentinel := errors.New("readlink /proc/self/exe: file deleted")
	withErrorPrismBinary(t, sentinel)
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@bwrap",
	})
	if err == nil {
		t.Fatalf("bwrap AgentPaneCmd: expected non-nil error when prismBinaryPathFn fails; got cmd=%q", got)
	}
	if got != "" {
		t.Errorf("bwrap AgentPaneCmd: expected empty cmd on error, got %q", got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("bwrap AgentPaneCmd: expected wrapped sentinel error, got %v", err)
	}
	// The error must NOT have silently rendered to a bare-prism fallback.
	if strings.Contains(got, "prism agent-run") {
		t.Errorf("bwrap AgentPaneCmd: silently fell back to bare-prism command on os.Executable error (#2260 silent-fallback regression): %q", got)
	}
}

// TestSandboxExecAgentPaneCmd_BinaryPathError_PropagatesAsError mirrors the
// error-propagation test for sandbox-exec.
func TestSandboxExecAgentPaneCmd_BinaryPathError_PropagatesAsError(t *testing.T) {
	sentinel := errors.New("os.Executable: not implemented")
	withErrorPrismBinary(t, sentinel)
	iso := &sandboxExecIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@sbx",
	})
	if err == nil {
		t.Fatalf("sandbox-exec AgentPaneCmd: expected non-nil error when prismBinaryPathFn fails; got cmd=%q", got)
	}
	if got != "" {
		t.Errorf("sandbox-exec AgentPaneCmd: expected empty cmd on error, got %q", got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("sandbox-exec AgentPaneCmd: expected wrapped sentinel error, got %v", err)
	}
	if strings.Contains(got, "prism agent-run") {
		t.Errorf("sandbox-exec AgentPaneCmd: silently fell back to bare-prism command on os.Executable error (#2260 silent-fallback regression): %q", got)
	}
}

// TestHostAgentPaneCmd_DoesNotResolvePrismBinary verifies that host mode
// does NOT consult prismBinaryPathFn — the host isolator returns DirectCmd
// unchanged, so an os.Executable failure must not affect host-mode spawn
// (which has no `prism agent-run` indirection at all).
//
// Stubbed to return an error: if host mode accidentally started calling
// prismBinaryPathFn, this test would surface a non-nil error and we would
// know about the regression.
func TestHostAgentPaneCmd_DoesNotResolvePrismBinary(t *testing.T) {
	withErrorPrismBinary(t, errors.New("must not be called"))
	iso := &hostIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "prism-test@host",
		DirectCmd:   "pi --agent worker",
	})
	if err != nil {
		t.Fatalf("host AgentPaneCmd: unexpected error: %v (host mode must not call prismBinaryPathFn)", err)
	}
	if got != "pi --agent worker" {
		t.Errorf("host AgentPaneCmd: expected DirectCmd unchanged; got %q", got)
	}
}

// TestBwrapAgentPaneCmd_EmptySessionName_BypassesBinaryResolution verifies
// that the defensive fallback (empty SessionName → return DirectCmd) does
// NOT call prismBinaryPathFn either. This is a pre-existing fast-path that
// the #2260 change must preserve — an unrelated os.Executable failure should
// not affect callers that pass an empty SessionName.
func TestBwrapAgentPaneCmd_EmptySessionName_BypassesBinaryResolution(t *testing.T) {
	withErrorPrismBinary(t, errors.New("must not be called"))
	iso := &bwrapIsolator{}
	got, err := iso.AgentPaneCmd(AgentPaneOpts{
		SessionName: "",
		DirectCmd:   "fallback direct cmd",
	})
	if err != nil {
		t.Fatalf("bwrap AgentPaneCmd (empty SessionName): unexpected error: %v", err)
	}
	if got != "fallback direct cmd" {
		t.Errorf("bwrap AgentPaneCmd (empty SessionName): expected DirectCmd unchanged; got %q", got)
	}
}
