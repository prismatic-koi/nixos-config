// Tests for the "prism quick pr" command (issue #2118).
//
// The seam pattern mirrors PR #2113 (cmd/investigate.go's
// investigateSpawnSessionFn). Test bodies swap the package-level
// piLookPathFn / piExecFn / gitRunFn / gitOutputFn / ghRunFn / ghOutputFn
// / openBrowserFn function vars with stubs, exercise Run() (or the
// helpers it calls), then assert on captured calls and returned errors.
//
// These tests do NOT exec real binaries — they verify Run()'s control
// flow, the structured-output parse, the >72-char title truncation,
// and the legacy-profiles.json JSON unmarshal compatibility.
//
// Test-suite isolation contract (AGENTS.md, issue #1608): these tests do
// not touch the host bus, DB, tmux, or HOME. profiles.json is loaded from
// XDG_CONFIG_HOME, which we redirect to t.TempDir().

package quick

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// ── Test-seam helpers ──────────────────────────────────────────────────────

// piCall records one invocation of piExecFn.
type piCall struct {
	args  []string
	stdin string
}

// callRecorder captures the call order across all seams so we can assert
// (e.g.) "git/gh were never called" after a pi-missing error.
type callRecorder struct {
	mu        sync.Mutex
	piCalls   []piCall
	gitCalls  [][]string // each call: argv after "git "
	ghCalls   [][]string // each call: argv after "gh "
	browser   []string   // URLs opened
	piLookErr error
}

func newRecorder() *callRecorder { return &callRecorder{} }

// installSeams swaps every package-level seam in this package with a stub
// recording into r. Returns a cleanup function the test should call via
// t.Cleanup. The stubs read scripted behaviour from r — happy-path tests
// can override individual stubs by re-assigning the package var.
func installSeams(t *testing.T, r *callRecorder) {
	t.Helper()

	prevPiLook := piLookPathFn
	prevPiExec := piExecFn
	prevGitRun := gitRunFn
	prevGitOut := gitOutputFn
	prevGhRun := ghRunFn
	prevGhOut := ghOutputFn
	prevBrowser := openBrowserFn

	piLookPathFn = func() error {
		return r.piLookErr
	}

	// Default piExec: not used directly; tests that need it override the
	// var after calling installSeams.
	piExecFn = func(args []string, stdin string) piResult {
		r.mu.Lock()
		r.piCalls = append(r.piCalls, piCall{args: append([]string(nil), args...), stdin: stdin})
		r.mu.Unlock()
		return piResult{err: fmt.Errorf("piExecFn not configured in this test")}
	}

	gitRunFn = func(args ...string) error {
		r.mu.Lock()
		r.gitCalls = append(r.gitCalls, append([]string(nil), args...))
		r.mu.Unlock()
		return nil
	}
	gitOutputFn = func(args ...string) (string, error) {
		r.mu.Lock()
		r.gitCalls = append(r.gitCalls, append([]string(nil), args...))
		r.mu.Unlock()
		// Default canned outputs for the read-only pre-flight calls.
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--abbrev-ref":
			return "main\n", nil
		case len(args) >= 3 && args[0] == "diff" && args[1] == "--cached" && args[2] == "--name-only":
			return "some/file.go\n", nil
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--cached":
			return "diff --git a/x b/x\n+hello\n", nil
		}
		return "", nil
	}
	ghRunFn = func(args ...string) error {
		r.mu.Lock()
		r.ghCalls = append(r.ghCalls, append([]string(nil), args...))
		r.mu.Unlock()
		return nil
	}
	ghOutputFn = func(args ...string) (string, error) {
		r.mu.Lock()
		r.ghCalls = append(r.ghCalls, append([]string(nil), args...))
		r.mu.Unlock()
		// Default: gh pr view --json url -q .url returns a fake URL.
		return "https://example/pr/1\n", nil
	}
	openBrowserFn = func(url string) error {
		r.mu.Lock()
		r.browser = append(r.browser, url)
		r.mu.Unlock()
		return nil
	}

	t.Cleanup(func() {
		piLookPathFn = prevPiLook
		piExecFn = prevPiExec
		gitRunFn = prevGitRun
		gitOutputFn = prevGitOut
		ghRunFn = prevGhRun
		ghOutputFn = prevGhOut
		openBrowserFn = prevBrowser
	})
}

// writeProfilesJSON writes a minimal profiles.json into a temp config home
// and points XDG_CONFIG_HOME at it for the duration of the test.
func writeProfilesJSON(t *testing.T, body string) {
	t.Helper()
	cfgHome := t.TempDir()
	prismDir := filepath.Join(cfgHome, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("mkdir prism: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
}

// piNDJSON builds a minimal pi --mode json stdout stream containing a
// single agent_end event with the given assistant text.
func piNDJSON(assistantText string) string {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type envelope struct {
		Type     string    `json:"type"`
		Messages []message `json:"messages"`
	}
	env := envelope{
		Type: "agent_end",
		Messages: []message{
			{
				Role: "user",
				Content: []contentBlock{
					{Type: "text", Text: "diff goes here"},
				},
			},
			{
				Role: "assistant",
				Content: []contentBlock{
					{Type: "text", Text: assistantText},
				},
			},
		},
	}
	// Prepend a couple of unrelated events to look more realistic.
	prefix := `{"type":"session","id":"abc"}` + "\n" +
		`{"type":"agent_start"}` + "\n" +
		`{"type":"turn_start"}` + "\n"
	body, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return prefix + string(body) + "\n"
}

// ── Tests ──────────────────────────────────────────────────────────────────

// TestRun_HappyPath verifies that with a well-formed pi response Run()
// produces a branch, commit, push, and gh pr create with the expected
// title and body, and opens the browser at the gh-returned URL.
func TestRun_HappyPath(t *testing.T) {
	writeProfilesJSON(t, `{
		"default":"anthropic","profiles":{},
		"quick_profiles":{"pr":{"model":"anthropic/claude-sonnet-4-6"}}
	}`)
	r := newRecorder()
	installSeams(t, r)

	wantTitle := "Fix typo in README"
	wantBody := "Recieve was spelled wrong; corrected to receive."
	piExecFn = func(args []string, stdin string) piResult {
		r.mu.Lock()
		r.piCalls = append(r.piCalls, piCall{args: append([]string(nil), args...), stdin: stdin})
		r.mu.Unlock()
		return piResult{
			stdout: piNDJSON(fmt.Sprintf(`{"title":%q,"body":%q}`, wantTitle, wantBody)),
		}
	}

	if err := Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pi must have been invoked.
	if len(r.piCalls) != 1 {
		t.Fatalf("piExecFn called %d times, want 1", len(r.piCalls))
	}
	got := r.piCalls[0].args
	// Required flags (issue #2118 ACs).
	requireFlag(t, got, "--print")
	requireFlagValue(t, got, "--mode", "json")
	requireFlag(t, got, "--no-tools")
	requireFlag(t, got, "--no-skills")
	requireFlagValue(t, got, "--model", "anthropic/claude-sonnet-4-6")
	requireFlagValue(t, got, "--provider", "anthropic")
	// AGENTS.md auto-discovery must be enabled (no --no-context-files).
	for _, a := range got {
		if a == "--no-context-files" || a == "-nc" {
			t.Errorf("pi args contain %q — AGENTS.md auto-discovery must remain enabled (issue #2118)", a)
		}
	}
	// System prompt must be present and materially expanded vs the old
	// 5-line template. The old template was 5 lines including blank
	// separators; the new one is several hundred chars with worked
	// examples. Assert on length + presence of a representative phrase.
	sp := flagValue(t, got, "--system-prompt")
	if len(sp) < 500 {
		t.Errorf("--system-prompt is only %d chars — expected the materially expanded prompt", len(sp))
	}
	if !strings.Contains(sp, "WORKED EXAMPLES") {
		t.Errorf("--system-prompt missing WORKED EXAMPLES section — prompt may have regressed to short template")
	}
	if !strings.Contains(strings.ToLower(sp), "imperative") {
		t.Errorf("--system-prompt does not mention imperative-mood title rule")
	}
	if !strings.Contains(sp, "72") {
		t.Errorf("--system-prompt does not mention the 72-char title constraint")
	}

	// gh pr create must have been invoked with the parsed title and body.
	var prCreateArgs []string
	for _, c := range r.ghCalls {
		if len(c) >= 2 && c[0] == "pr" && c[1] == "create" {
			prCreateArgs = c
			break
		}
	}
	if prCreateArgs == nil {
		t.Fatalf("gh pr create was never invoked; ghCalls=%v", r.ghCalls)
	}
	if v := flagValue(t, prCreateArgs, "--title"); v != wantTitle {
		t.Errorf("gh pr create --title = %q, want %q", v, wantTitle)
	}
	if v := flagValue(t, prCreateArgs, "--body"); v != wantBody {
		t.Errorf("gh pr create --body = %q, want %q", v, wantBody)
	}

	// Browser opened with the URL gh returned.
	if len(r.browser) != 1 || r.browser[0] != "https://example/pr/1" {
		t.Errorf("openBrowser not invoked correctly: %v", r.browser)
	}
}

// TestRun_PiNotOnPath verifies that when piLookPathFn returns ENOENT,
// Run() exits with a clear error naming the missing binary and does NOT
// invoke any destructive git/gh operations.
//
// Negative-mutation check (issue #2118 discipline): if you delete the
// piLookPathFn() pre-flight from Run(), the test below produces a
// different error (something like "exec: \"pi\": file not found") and
// destructive git/gh seams MAY be called — assertions fail.
func TestRun_PiNotOnPath(t *testing.T) {
	writeProfilesJSON(t, `{
		"default":"anthropic","profiles":{},
		"quick_profiles":{"pr":{"model":"anthropic/claude-sonnet-4-6"}}
	}`)
	r := newRecorder()
	installSeams(t, r)
	// Simulate ENOENT for pi.
	r.piLookErr = fmt.Errorf("simulated lookup: %w", &exec.Error{Name: "pi", Err: exec.ErrNotFound})

	err := Run()
	if err == nil {
		t.Fatal("Run() should have returned an error when pi is missing")
	}
	msg := err.Error()
	if !strings.Contains(msg, "quick pr:") {
		t.Errorf("error %q missing 'quick pr:' prefix", msg)
	}
	if !strings.Contains(msg, "pi") {
		t.Errorf("error %q does not name the missing binary 'pi'", msg)
	}

	// Destructive git/gh calls must NOT have happened. Read-only git
	// pre-flight (rev-parse, diff --cached) must also not have run since
	// LookPath is the very first check.
	if len(r.gitCalls) != 0 {
		t.Errorf("git was called %d times despite pi-missing; calls=%v", len(r.gitCalls), r.gitCalls)
	}
	if len(r.ghCalls) != 0 {
		t.Errorf("gh was called %d times despite pi-missing; calls=%v", len(r.ghCalls), r.ghCalls)
	}
	if len(r.piCalls) != 0 {
		t.Errorf("pi was exec'd %d times despite LookPath failure; calls=%v", len(r.piCalls), r.piCalls)
	}
}

// TestRun_PiReturnsMalformedOutput verifies that an unparseable pi stdout
// yields an error that includes the raw output for diagnosis.
//
// Negative-mutation check: if you replace extractTitleBody's JSON parse
// with a "first line is title" fallback, this test fails — the malformed
// blob would be silently accepted as the title.
func TestRun_PiReturnsMalformedOutput(t *testing.T) {
	writeProfilesJSON(t, `{
		"default":"anthropic","profiles":{},
		"quick_profiles":{"pr":{"model":"anthropic/claude-sonnet-4-6"}}
	}`)
	r := newRecorder()
	installSeams(t, r)

	// Pi emits an agent_end event whose assistant text is NOT valid JSON.
	malformed := piNDJSON("this is not a valid json object at all !!!")
	piExecFn = func(args []string, stdin string) piResult {
		r.mu.Lock()
		r.piCalls = append(r.piCalls, piCall{args: append([]string(nil), args...), stdin: stdin})
		r.mu.Unlock()
		return piResult{stdout: malformed}
	}

	err := Run()
	if err == nil {
		t.Fatal("Run() should have errored on malformed pi output")
	}
	msg := err.Error()
	if !strings.Contains(msg, "quick pr:") {
		t.Errorf("error %q missing 'quick pr:' prefix", msg)
	}
	// Error must include enough of the raw output for diagnosis.
	if !strings.Contains(msg, "not a valid json object") {
		t.Errorf("error does not include the raw pi output for diagnosis: %v", msg)
	}

	// Destructive git/gh ops must not have run (we never got a title).
	for _, c := range r.gitCalls {
		if len(c) > 0 && (c[0] == "commit" || c[0] == "push") {
			t.Errorf("destructive git %v ran despite parse failure", c)
		}
		if len(c) >= 2 && c[0] == "switch" && c[1] == "-c" {
			t.Errorf("git switch -c ran despite parse failure: %v", c)
		}
	}
	if len(r.ghCalls) != 0 {
		t.Errorf("gh was called despite parse failure: %v", r.ghCalls)
	}
}

// TestRun_PiExecError verifies that a pi exec failure (e.g. OAuth expired,
// non-zero exit) is propagated to the user with the pi stderr preserved
// and a "quick pr:" prefix.
func TestRun_PiExecError(t *testing.T) {
	writeProfilesJSON(t, `{
		"default":"anthropic","profiles":{},
		"quick_profiles":{"pr":{"model":"anthropic/claude-sonnet-4-6"}}
	}`)
	r := newRecorder()
	installSeams(t, r)

	wantStderr := "ERROR: anthropic-oauth credentials expired; run /login in pi"
	piExecFn = func(args []string, stdin string) piResult {
		r.mu.Lock()
		r.piCalls = append(r.piCalls, piCall{args: append([]string(nil), args...), stdin: stdin})
		r.mu.Unlock()
		return piResult{stdout: "", stderr: wantStderr, err: errors.New("exit status 1")}
	}

	err := Run()
	if err == nil {
		t.Fatal("Run() should have errored on pi exec failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "quick pr:") {
		t.Errorf("error %q missing 'quick pr:' prefix", msg)
	}
	if !strings.Contains(msg, wantStderr) {
		t.Errorf("error does not preserve pi stderr %q; got %q", wantStderr, msg)
	}
	if !strings.Contains(msg, "exit status 1") {
		t.Errorf("error does not preserve underlying exec error: %q", msg)
	}
}

// TestRun_TitleTruncatedAt72Chars verifies that an over-long title from
// pi is truncated to 72 chars and a warning is printed to stderr.
//
// Negative-mutation check: if you remove the truncate-and-warn block, the
// gh pr create --title length will be > 72 — fails len assertion.
func TestRun_TitleTruncatedAt72Chars(t *testing.T) {
	writeProfilesJSON(t, `{
		"default":"anthropic","profiles":{},
		"quick_profiles":{"pr":{"model":"anthropic/claude-sonnet-4-6"}}
	}`)
	r := newRecorder()
	installSeams(t, r)

	// Build a 100-char title.
	longTitle := strings.Repeat("A", 100)
	piExecFn = func(args []string, stdin string) piResult {
		r.mu.Lock()
		r.piCalls = append(r.piCalls, piCall{args: append([]string(nil), args...), stdin: stdin})
		r.mu.Unlock()
		return piResult{stdout: piNDJSON(fmt.Sprintf(`{"title":%q,"body":"body"}`, longTitle))}
	}

	// Capture stderr to assert the warning was emitted. We drain the
	// pipe concurrently per the stdout-capture-testing convention
	// (issue #1798).
	origStderr := os.Stderr
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = wPipe
	doneCh := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(rPipe)
		doneCh <- data
	}()
	t.Cleanup(func() {
		os.Stderr = origStderr
	})

	runErr := Run()

	// Close the write end so the reader goroutine sees EOF.
	_ = wPipe.Close()
	os.Stderr = origStderr
	stderrBytes := <-doneCh

	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	stderr := string(stderrBytes)
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "truncat") {
		t.Errorf("expected truncation warning on stderr; got %q", stderr)
	}

	// Find the gh pr create call and assert title length.
	var prCreate []string
	for _, c := range r.ghCalls {
		if len(c) >= 2 && c[0] == "pr" && c[1] == "create" {
			prCreate = c
			break
		}
	}
	if prCreate == nil {
		t.Fatalf("gh pr create not invoked; ghCalls=%v", r.ghCalls)
	}
	got := flagValue(t, prCreate, "--title")
	if len(got) != 72 {
		t.Errorf("gh pr create --title length = %d, want 72 (got %q)", len(got), got)
	}
}

// TestLegacyProfilesJSONUnmarshal locks in the schema-compatibility
// promise (issue #2118): loading a profiles.json that still carries a
// `providerOrder` field on the pr entry must NOT fail unmarshalling.
//
// Negative-mutation check: if QuickProfile ever gets a
// `json:",disallowunknown"` tag or the decoder is switched to
// DisallowUnknownFields, this test fails — the legacy field would be
// rejected.
func TestLegacyProfilesJSONUnmarshal(t *testing.T) {
	const legacy = `{
		"default":"anthropic",
		"profiles":{},
		"quick_profiles":{
			"pr":{
				"model":"google/gemini-3.1-flash-lite-preview",
				"providerOrder":["google","google-vertex"]
			}
		}
	}`
	writeProfilesJSON(t, legacy)

	pf, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles must tolerate legacy providerOrder field: %v", err)
	}
	qp, ok := pf.QuickProfiles["pr"]
	if !ok {
		t.Fatal("pr entry missing after unmarshal")
	}
	if qp.Model != "google/gemini-3.1-flash-lite-preview" {
		t.Errorf("model = %q, want google/gemini-3.1-flash-lite-preview", qp.Model)
	}
}

// TestNoOpenRouterReferences guards against accidental re-introduction of
// the OPENROUTER_API_KEY env var or openrouter.ai URL in the quick
// package — both must be gone per issue #2118.
//
// Negative-mutation check: re-add either string and this test fails.
func TestNoOpenRouterReferences(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found in this package")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := string(data)
		if strings.Contains(body, "OPENROUTER_API_KEY") {
			t.Errorf("%s still references OPENROUTER_API_KEY", f)
		}
		if strings.Contains(body, "openrouter.ai") {
			t.Errorf("%s still references openrouter.ai", f)
		}
	}
}

// ── Direct extractTitleBody tests ──────────────────────────────────────────

func TestExtractTitleBody_WellFormed(t *testing.T) {
	stdout := piNDJSON(`{"title":"Bump react to 18.3.1","body":"upstream useId fix"}`)
	title, body, err := extractTitleBody(stdout)
	if err != nil {
		t.Fatalf("extractTitleBody: %v", err)
	}
	if title != "Bump react to 18.3.1" {
		t.Errorf("title = %q", title)
	}
	if body != "upstream useId fix" {
		t.Errorf("body = %q", body)
	}
}

func TestExtractTitleBody_WrappedInJSONFence(t *testing.T) {
	// Defensive parse path: model occasionally wraps the JSON in a fence
	// despite the system-prompt prohibition.
	fenced := "```json\n{\"title\":\"Add a thing\",\"body\":\"because\"}\n```"
	stdout := piNDJSON(fenced)
	title, body, err := extractTitleBody(stdout)
	if err != nil {
		t.Fatalf("extractTitleBody: %v", err)
	}
	if title != "Add a thing" || body != "because" {
		t.Errorf("got (%q, %q)", title, body)
	}
}

func TestExtractTitleBody_EmptyStdout(t *testing.T) {
	_, _, err := extractTitleBody("   \n  \n")
	if err == nil {
		t.Fatal("expected error on empty stdout")
	}
	if !strings.Contains(err.Error(), "empty stdout") {
		t.Errorf("error %q does not name the failure mode", err)
	}
}

func TestExtractTitleBody_NoAssistantMessage(t *testing.T) {
	// Valid NDJSON but no assistant message anywhere.
	stdout := `{"type":"session"}` + "\n" + `{"type":"agent_start"}` + "\n"
	_, _, err := extractTitleBody(stdout)
	if err == nil {
		t.Fatal("expected error when no assistant message present")
	}
}

// TestGenerateDescription_PrintsProgressBeforePiExec verifies that a
// progress line reaches stdout before the (seamed) pi call runs, so the
// user sees output during the model call instead of a silent hang
// (issue #2777).
func TestGenerateDescription_PrintsProgressBeforePiExec(t *testing.T) {
	r := newRecorder()
	installSeams(t, r)

	piStarted := false
	piExecFn = func(args []string, stdin string) piResult {
		piStarted = true
		return piResult{stdout: piNDJSON(`{"title":"Fix x","body":""}`)}
	}

	// Capture stdout, draining concurrently per the stdout-capture-testing
	// convention (issue #1798) — the printed line is tiny here, but the
	// convention is followed regardless of size.
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = pw

	done := make(chan string, 1)
	go func() {
		buf, _ := io.ReadAll(pr)
		done <- string(buf)
	}()

	_, _, err = generateDescription(config.QuickProfile{Model: "anthropic/claude-sonnet-4-6"}, "diff")

	os.Stdout = origStdout
	_ = pw.Close()
	captured := <-done
	_ = pr.Close()

	if err != nil {
		t.Fatalf("generateDescription: %v", err)
	}
	if !piStarted {
		t.Fatal("generateDescription: piExecFn was never called")
	}
	if strings.TrimSpace(captured) == "" {
		t.Fatal("generateDescription: expected a progress line on stdout before the pi call, got none")
	}
}

// ── Small assertion helpers ────────────────────────────────────────────────

func requireFlag(t *testing.T, argv []string, flag string) {
	t.Helper()
	for _, a := range argv {
		if a == flag {
			return
		}
	}
	t.Errorf("argv missing required flag %q; argv=%v", flag, argv)
}

func requireFlagValue(t *testing.T, argv []string, flag, want string) {
	t.Helper()
	got := flagValue(t, argv, flag)
	if got != want {
		t.Errorf("%s = %q, want %q", flag, got, want)
	}
}

func flagValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
