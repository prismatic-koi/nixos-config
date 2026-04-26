package session

// Tests for #1064 — host-mode prompt-file plumbing and launch-command size
// guard. The headline failure mode: prism spawn --isolation host with a
// prompt above ~12 KB silently failed because the entire prompt was
// inlined onto the launch command, which then got truncated by tmux's
// arg handling.
//
// The tests below cover three slices, deliberately small because the same
// machinery (BuildOpencodeCmd, WriteInitialPrompt, the size guard) is
// orthogonal to the rest of the spawn pipeline:
//
//   1. The constructed launch command stays small regardless of prompt
//      size — the prompt body is reachable on disk, not on the command
//      line. (AC-1, AC-3, AC-4, AC-10)
//   2. The pre-spawn size check rejects pathological host-mode launch
//      commands before any tmux state is created. (AC-6, AC-11)
//   3. The readiness-timeout error gets enriched with a prompt-size hint
//      when the launch command was unusual but not pathological. (AC-7)

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── prompt-file helpers ─────────────────────────────────────────────────────

// TestInitialPromptPath_CoLocatedWithAgentLogs verifies that the prompt-file
// path lives in the same per-session run directory as agent-startup.log,
// agent-run.log, and hostapi.sock — see #1066 for the SessionDirName
// alignment that this test pins. A single
// `ls run/<sessionDirName>/` should show every forensic artefact for the
// session.
func TestInitialPromptPath_CoLocatedWithAgentLogs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "myrepo@feat"
	got, err := InitialPromptPath(sessionName)
	if err != nil {
		t.Fatalf("InitialPromptPath: %v", err)
	}
	want := filepath.Join(tmp, "prism", "run", SessionDirName(sessionName), "initial-prompt.txt")
	if got != want {
		t.Errorf("InitialPromptPath = %q, want %q", got, want)
	}

	startup, err := AgentStartupLogPath(sessionName)
	if err != nil {
		t.Fatalf("AgentStartupLogPath: %v", err)
	}
	if filepath.Dir(got) != filepath.Dir(startup) {
		t.Errorf("initial-prompt dir %q != agent-startup dir %q — files should be co-located",
			filepath.Dir(got), filepath.Dir(startup))
	}

	runPath, err := AgentRunLogPath(sessionName)
	if err != nil {
		t.Fatalf("AgentRunLogPath: %v", err)
	}
	if filepath.Dir(got) != filepath.Dir(runPath) {
		t.Errorf("initial-prompt dir %q != agent-run dir %q — files should be co-located",
			filepath.Dir(got), filepath.Dir(runPath))
	}
}

// TestWriteInitialPrompt_RoundtripBytes verifies that a 32 KB prompt with
// every "interesting" byte class round-trips through the prompt file
// byte-for-byte. This is the core AC-4 assertion: the file path is the
// transport, so the content must arrive intact regardless of size.
func TestWriteInitialPrompt_RoundtripBytes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// Build a 32 KB prompt that contains the four byte classes the AC text
	// names explicitly, plus newlines, plus a 32 KB body of repetitive ASCII
	// to exercise the size aspect. Hash both sides for an O(1) comparison.
	prompt := buildLargePrompt(32 * 1024)

	const sessionName = "myrepo@big-prompt"
	path, err := WriteInitialPrompt(sessionName, prompt)
	if err != nil {
		t.Fatalf("WriteInitialPrompt: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != prompt {
		t.Errorf("prompt roundtrip mismatch: input sha=%s len=%d, output sha=%s len=%d",
			sha256hex(prompt), len(prompt),
			sha256hex(string(got)), len(got),
		)
	}
}

// TestWriteInitialPrompt_128KB verifies the same roundtrip at 128 KB so the
// fix is visibly an architectural removal of the limit, not a slightly
// larger threshold (AC-3).
func TestWriteInitialPrompt_128KB(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	prompt := buildLargePrompt(128 * 1024)
	const sessionName = "myrepo@bigger-prompt"
	path, err := WriteInitialPrompt(sessionName, prompt)
	if err != nil {
		t.Fatalf("WriteInitialPrompt: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(prompt) {
		t.Fatalf("prompt size mismatch: input=%d, output=%d", len(prompt), len(got))
	}
	if sha256hex(string(got)) != sha256hex(prompt) {
		t.Errorf("128 KB prompt roundtrip hash mismatch")
	}
}

// TestWriteInitialPrompt_OverwritesStaleFile verifies that a re-spawn of the
// same session name produces a fresh file rather than leaving stale prompt
// content from the previous incarnation.
func TestWriteInitialPrompt_OverwritesStaleFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "myrepo@reused-name"
	if _, err := WriteInitialPrompt(sessionName, "first incarnation"); err != nil {
		t.Fatalf("first WriteInitialPrompt: %v", err)
	}
	path, err := WriteInitialPrompt(sessionName, "second incarnation")
	if err != nil {
		t.Fatalf("second WriteInitialPrompt: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second incarnation" {
		t.Errorf("stale prompt survived overwrite: got %q, want %q", string(got), "second incarnation")
	}
}

// ── BuildOpencodeCmd: host mode + prompt file ──────────────────────────────

// TestBuildOpencodeCmd_HostMode_PromptFile verifies that BuildOpencodeCmd in
// host mode with PromptFilePath set emits `--prompt "$(cat <quoted path>)"`
// and does NOT inline the prompt body. This is the core change: the launch
// command size becomes O(1) in prompt size. (AC-1, AC-10)
func TestBuildOpencodeCmd_HostMode_PromptFile(t *testing.T) {
	prompt := buildLargePrompt(32 * 1024)
	opts := Opts{
		IsolationMode:  "host",
		Agent:          "worker",
		Port:           14000,
		SessionName:    "myrepo@feat",
		Prompt:         prompt,
		PromptFilePath: "/var/state/prism/run/myrepo@feat/initial-prompt.txt",
	}
	cmd := BuildOpencodeCmd(opts)

	// The command must reference the file via $(cat …) — operators (and
	// the size guard) rely on this contract so the launch command size
	// stays O(1) in prompt size.
	wantSubstr := `--prompt "$(cat '/var/state/prism/run/myrepo@feat/initial-prompt.txt')"`
	if !strings.Contains(cmd, wantSubstr) {
		t.Errorf("host-mode cmd does not contain %q\ngot: %q", wantSubstr, cmd)
	}

	// The prompt body itself must NOT appear on the command line — that's
	// the whole point of the file-based plumbing.
	if strings.Contains(cmd, prompt) {
		t.Errorf("host-mode cmd contains the prompt body inline; expected to read it via $(cat …). cmd len=%d, prompt len=%d", len(cmd), len(prompt))
	}

	// The constructed command must be small even though the prompt is 32 KB.
	// 1 KB is a generous upper bound that comfortably accommodates the
	// opencode invocation, env-var prefixes, and the cat-substitution.
	if len(cmd) > 1024 {
		t.Errorf("host-mode cmd unexpectedly large (%d bytes) — expected O(1) in prompt size", len(cmd))
	}
}

// TestBuildOpencodeCmd_HostMode_NoPromptFile verifies the legacy inline path
// (PromptFilePath empty) still works for small prompts. This covers the
// no-regression AC-2: small prompts that worked before the fix continue to
// work without the file plumbing.
func TestBuildOpencodeCmd_HostMode_NoPromptFile(t *testing.T) {
	opts := Opts{
		IsolationMode: "host",
		Agent:         "worker",
		Port:          14000,
		SessionName:   "myrepo@feat",
		Prompt:        "small prompt",
	}
	cmd := BuildOpencodeCmd(opts)

	if strings.Contains(cmd, "$(cat") {
		t.Errorf("host-mode cmd unexpectedly uses cat-substitution for inline prompt: %q", cmd)
	}
	if !strings.Contains(cmd, "'small prompt'") {
		t.Errorf("host-mode cmd missing inline prompt: %q", cmd)
	}
}

// TestBuildOpencodeCmd_HostMode_PromptFile_PathQuoted verifies that a path
// containing single quotes is shell-quoted so the cat substitution does not
// terminate early. This is a defence-in-depth check: per-session run dirs
// derived from session names (e.g. "myrepo@branch") will not contain quotes
// in normal use, but if a future caller ever passes a path with quotes the
// surrounding $() must not get confused.
func TestBuildOpencodeCmd_HostMode_PromptFile_PathQuoted(t *testing.T) {
	opts := Opts{
		IsolationMode:  "host",
		Agent:          "worker",
		Port:           14000,
		SessionName:    "myrepo@feat",
		Prompt:         "x",
		PromptFilePath: `/tmp/weird's-path/initial-prompt.txt`,
	}
	cmd := BuildOpencodeCmd(opts)

	// Single quote must be escaped with the standard '\'' sequence.
	if !strings.Contains(cmd, `'/tmp/weird'\''s-path/initial-prompt.txt'`) {
		t.Errorf("expected escaped single-quote in path within cmd, got: %q", cmd)
	}
}

// TestBuildOpencodeCmd_HostMode_PromptFile_IgnoredWhenPromptEmpty verifies
// that PromptFilePath has no effect when Prompt is empty — the cat call
// only fires when there is actually a prompt to deliver.
func TestBuildOpencodeCmd_HostMode_PromptFile_IgnoredWhenPromptEmpty(t *testing.T) {
	opts := Opts{
		IsolationMode:  "host",
		Agent:          "worker",
		Port:           14000,
		SessionName:    "myrepo@feat",
		PromptFilePath: "/tmp/initial-prompt.txt",
	}
	cmd := BuildOpencodeCmd(opts)
	if strings.Contains(cmd, "$(cat") {
		t.Errorf("expected no cat-substitution when Prompt is empty, got: %q", cmd)
	}
	if strings.Contains(cmd, "--prompt") {
		t.Errorf("expected no --prompt flag when Prompt is empty, got: %q", cmd)
	}
}

// ── pre-spawn size guard ────────────────────────────────────────────────────

// TestSpawnSession_HostMode_RejectsOversizedLaunchCmd verifies AC-6 / AC-11:
// a constructed host-mode launch command exceeding HostLaunchCmdSafeBound
// produces a HostLaunchCmdTooLargeError before any tmux state is created.
//
// The test inflates AgentEnvVars with a single huge value to force the
// constructed cmd above the safe bound — this is the only realistic post-fix
// path that can still produce an oversized command (the prompt body is no
// longer on the launch line). It mirrors the future-regression scenario the
// guard was designed to catch.
func TestSpawnSession_HostMode_RejectsOversizedLaunchCmd(t *testing.T) {
	d, _ := openSpawnTestDB(t)

	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	argsFile := spyTmuxBin(t)

	const sessionName = "myrepo@oversize"
	bigEnvValue := strings.Repeat("X", HostLaunchCmdSafeBound+1024)
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      tmp,
		AgentRole:     "worker",
		Prompt:        "tiny prompt",
		Layout:        LayoutFull,
		IsolationMode: "host",
		AgentEnvVars: map[string]string{
			"OVERSIZE_VAR": bigEnvValue,
		},
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession: got nil, want HostLaunchCmdTooLargeError")
	}
	if !IsHostLaunchCmdTooLarge(err) {
		t.Fatalf("SpawnSession err type = %T (%v), want *HostLaunchCmdTooLargeError", err, err)
	}

	// Error must carry the exact bytes for operator pattern-matching.
	var hltl *HostLaunchCmdTooLargeError
	if !errors.As(err, &hltl) {
		t.Fatalf("errors.As failed for %v", err)
	}
	if hltl.SessionName != sessionName {
		t.Errorf("err.SessionName = %q, want %q", hltl.SessionName, sessionName)
	}
	if hltl.SafeBound != HostLaunchCmdSafeBound {
		t.Errorf("err.SafeBound = %d, want %d", hltl.SafeBound, HostLaunchCmdSafeBound)
	}
	if hltl.CmdSize <= HostLaunchCmdSafeBound {
		t.Errorf("err.CmdSize = %d, want > %d", hltl.CmdSize, HostLaunchCmdSafeBound)
	}

	// Error message must include the actual size, the safe bound, and a
	// reference to the workaround. Worded loosely so a future copy edit
	// does not cascade into a test failure, but the salient nouns must be
	// present.
	msg := err.Error()
	for _, expected := range []string{
		"host-mode launch command",
		"safe bound",
		"--prompt-file",
		"#1064",
	} {
		if !strings.Contains(msg, expected) {
			t.Errorf("err message missing %q: %s", expected, msg)
		}
	}

	// CRITICAL: no tmux state should have been created. Spawn must reject
	// the request before reaching tmux.NewSessionDetached or NewWindow.
	args := readSpyArgs(argsFile)
	for _, a := range args {
		if a == "new-session" || a == "new-window" {
			t.Errorf("oversized spawn unexpectedly called tmux %q (full args: %v)", a, args)
		}
	}
}

// TestSpawnSession_HostMode_AcceptsBoundedLaunchCmd verifies the no-regression
// half of AC-6: a constructed launch command well within the safe bound is
// not rejected. This is the path most spawns take — small prompts, small
// env-var prefixes — and must continue to work.
//
// We don't actually run a full spawn (that requires a real sidecar/tmux);
// instead we drive the size guard directly through BuildOpencodeCmd and
// assert it would not trip. The DB-side path is covered by other spawn
// tests; the size guard is the new behaviour.
func TestSpawnSession_HostMode_AcceptsBoundedLaunchCmd(t *testing.T) {
	opts := Opts{
		IsolationMode: "host",
		Agent:         "worker",
		Port:          14000,
		SessionName:   "myrepo@feat",
		Prompt:        "small prompt",
		// Realistic env-var prefixes — the same shape spawn produces today.
		AgentEnvVars: map[string]string{
			"AWS_CONFIG_FILE": "/Users/bensherman/.config/aws/readonly-config",
			"GIT_EDITOR":      "true",
		},
	}
	cmd := BuildOpencodeCmd(opts)
	if len(cmd) > HostLaunchCmdSafeBound {
		t.Errorf("realistic host-mode cmd (%d bytes) unexpectedly exceeds safe bound %d — guard would reject normal spawns. cmd=%q",
			len(cmd), HostLaunchCmdSafeBound, cmd)
	}
}

// ── readiness timeout enrichment ────────────────────────────────────────────

// TestReadinessTimeoutError_WithHint verifies that the Hint field, when set,
// surfaces in the error message after the standard "not ready within X"
// prefix — exactly the form the operator sees for AC-7.
func TestReadinessTimeoutError_WithHint(t *testing.T) {
	rte := &ReadinessTimeoutError{
		SessionName: "myrepo@big",
		Timeout:     30_000_000_000, // 30s in nanoseconds
		Hint:        "host-mode launch command was 8400 bytes, above the typical safe range of 1024 bytes; a prompt-size issue is a likely cause (see issue #1064)",
	}
	got := rte.Error()
	if !strings.Contains(got, "not ready within 30s") {
		t.Errorf("error missing standard prefix: %q", got)
	}
	if !strings.Contains(got, "#1064") {
		t.Errorf("error missing issue reference: %q", got)
	}
	if !strings.Contains(got, "8400 bytes") {
		t.Errorf("error missing actual cmd size: %q", got)
	}
}

// TestReadinessTimeoutError_WithoutHint verifies that the Hint field is
// optional — when empty, the error message stays exactly as it was before
// #1064 (so non-host callers see no behavioural change).
func TestReadinessTimeoutError_WithoutHint(t *testing.T) {
	rte := &ReadinessTimeoutError{
		SessionName: "myrepo@small",
		Timeout:     30_000_000_000,
	}
	got := rte.Error()
	want := "not ready within 30s"
	if got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// buildLargePrompt returns a prompt of approximately size bytes that
// includes the byte classes named in #1064 AC-4 (single quotes, backticks,
// dollar signs, double quotes, newlines) so a faithful roundtrip
// demonstrably preserves all of them.
func buildLargePrompt(size int) string {
	header := "Mixed payload: ' \" ` $ \n with a newline, $(cmd), \"quoted\", `back`. "
	body := strings.Repeat("a", size-len(header))
	if body == "" {
		return header[:size]
	}
	return header + body
}

// sha256hex returns the hex SHA-256 of s — used for size-agnostic
// comparison in roundtrip assertions.
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}


