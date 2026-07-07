package sidecar

// host_api_prompt_proxy_test.go — regression tests for issue #2359
// review-code / review-context follow-up.
//
// The sidecar's cross-session /prompt handler shells to host-side
// `prism prompt`. Round-2 review flagged that the branch discarded the
// child's stdout and returned an empty envelope on success — so a
// sandboxed caller (worker → coordinator, sandboxed coordinator → worker)
// saw a plain "prompt delivered" line even when the target sidecar had
// buffered the delivery.
//
// The fix passes --json to the child and forwards the child's stdout
// envelope through the response. This file locks that contract in with a
// stub binary that emits a scripted --json envelope and asserts the
// forwarded response carries the expected fields.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// TestHostAPI_Prompt_CrossSession_ForwardsBufferedEnvelope stubs the
// prism binary with a shell script that emits a --json envelope
// containing {"buffered": true, ...} on stdout. The sidecar's cross-
// session /prompt handler must parse that stdout and forward the
// buffered field in its own response body.
func TestHostAPI_Prompt_CrossSession_ForwardsBufferedEnvelope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stubs are POSIX-only")
	}
	// Stub script: emit a JSON envelope on stdout that mimics what
	// direct-host `prism prompt --json` would print when the target
	// sidecar responded {"buffered": true}.
	stub := filepath.Join(t.TempDir(), "prism-stub-buffered")
	script := "#!/bin/sh\n" +
		"echo '{\"delivered_to\":\"target-repo@worker\"," +
		"\"delivery_id\":\"stub-id\",\"buffered\":true," +
		"\"replayed\":false,\"transport\":\"socket-pipe\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	bus := sidecartest.NewIsolated(t, "")
	sc := newCrossSessionSidecar(t, "prism-test@coord-forward-buffered",
		"target-repo", "coordinator", stub, bus.DB)

	// Seed a target session in the same repo so the cross-session
	// permission gate passes. Coordinator can prompt any session in its
	// own repo.
	seedTargetSession(t, bus.DB, "target-repo@worker", "target-repo")

	body := `{"session":"target-repo@worker","prompt":"hello"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-session /prompt: got status %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// The response body MUST carry buffered=true. Before the fix, the
	// handler returned {} regardless of the child's output.
	var parsed map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse response body: %v (body=%s)", err, rr.Body.String())
	}
	buffered, _ := parsed["buffered"].(bool)
	if !buffered {
		t.Errorf("cross-session /prompt response missing buffered=true: %v", parsed)
	}
	if replayed, _ := parsed["replayed"].(bool); replayed {
		t.Errorf("cross-session /prompt response has replayed=true but stub emitted false: %v", parsed)
	}
}

// TestHostAPI_Prompt_CrossSession_ForwardsReplayedEnvelope is the sibling
// test for the replayed:true path — a repeat delivery_id dropped by the
// target sidecar's dedup ledger.
func TestHostAPI_Prompt_CrossSession_ForwardsReplayedEnvelope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stubs are POSIX-only")
	}
	stub := filepath.Join(t.TempDir(), "prism-stub-replayed")
	script := "#!/bin/sh\n" +
		"echo '{\"delivered_to\":\"target-repo@worker\",\"delivery_id\":\"stub-id\"," +
		"\"buffered\":false,\"replayed\":true,\"transport\":\"socket-pipe\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	bus := sidecartest.NewIsolated(t, "")
	sc := newCrossSessionSidecar(t, "prism-test@coord-forward-replayed",
		"target-repo", "coordinator", stub, bus.DB)
	seedTargetSession(t, bus.DB, "target-repo@worker", "target-repo")

	body := `{"session":"target-repo@worker","prompt":"hello"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-session /prompt: got status %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse response body: %v", err)
	}
	replayed, _ := parsed["replayed"].(bool)
	if !replayed {
		t.Errorf("cross-session /prompt response missing replayed=true: %v", parsed)
	}
}

// TestHostAPI_Prompt_CrossSession_ChildInvokedWithJSON verifies the
// sidecar invokes the child prism binary with --json. Without --json the
// child's stdout would be a plain human line and the parse would silently
// return synchronous-success, defeating the fix. The stub script records
// its argv to a file we then inspect.
func TestHostAPI_Prompt_CrossSession_ChildInvokedWithJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stubs are POSIX-only")
	}
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv.log")
	stub := filepath.Join(tmp, "prism-stub-argv")
	// Log every arg on its own line, then emit a valid --json envelope.
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> " + argvLog + "; done\n" +
		"echo '{\"delivered_to\":\"target-repo@worker\",\"delivery_id\":\"x\"," +
		"\"buffered\":false,\"replayed\":false,\"transport\":\"socket-pipe\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	bus := sidecartest.NewIsolated(t, "")
	sc := newCrossSessionSidecar(t, "prism-test@coord-argv-check",
		"target-repo", "coordinator", stub, bus.DB)
	seedTargetSession(t, bus.DB, "target-repo@worker", "target-repo")

	body := `{"session":"target-repo@worker","prompt":"hello"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-session /prompt: got status %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	logged := string(raw)
	if !strings.Contains(logged, "\n--json\n") && !strings.HasSuffix(logged, "--json\n") {
		t.Errorf("child prism binary was not invoked with --json; recorded argv:\n%s", logged)
	}
	// Sanity check: the prompt arg should also be present.
	if !strings.Contains(logged, "target-repo@worker") {
		t.Errorf("child argv missing target session name; recorded argv:\n%s", logged)
	}
}

// TestHostAPI_Prompt_CrossSession_UnparseableStdoutFallsBack verifies the
// robustness clause: a child that emits garbage on stdout (non-JSON) does
// NOT fail the request — the handler falls back to synchronous-success
// defaults so pre-#2359 callers see identical behaviour.
func TestHostAPI_Prompt_CrossSession_UnparseableStdoutFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stubs are POSIX-only")
	}
	stub := filepath.Join(t.TempDir(), "prism-stub-garbage")
	script := "#!/bin/sh\necho 'this is not JSON'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	bus := sidecartest.NewIsolated(t, "")
	sc := newCrossSessionSidecar(t, "prism-test@coord-garbage-stdout",
		"target-repo", "coordinator", stub, bus.DB)
	seedTargetSession(t, bus.DB, "target-repo@worker", "target-repo")

	body := `{"session":"target-repo@worker","prompt":"hello"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-session /prompt with garbage child stdout: got status %d, want 200 (fallback); body=%s",
			rr.Code, rr.Body.String())
	}
	// The response must be a valid JSON object with buffered/replayed both
	// false (the synchronous-success default).
	var parsed map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse response body: %v (body=%s)", err, rr.Body.String())
	}
	if b, _ := parsed["buffered"].(bool); b {
		t.Errorf("garbage-stdout fallback should default to buffered=false: %v", parsed)
	}
}

// newCrossSessionSidecar constructs a coordinator sidecar with a stubbed
// prism binary, wired for cross-session /prompt tests. The session name
// uses the prism-test@ prefix per #1608.
func newCrossSessionSidecar(t *testing.T, sessionName, repo, role, prismBinaryPath string, d *db.DB) *Sidecar {
	t.Helper()
	if !strings.HasPrefix(sessionName, "prism-test@") {
		t.Fatalf("cross-session tests must use prism-test@ prefix, got %q", sessionName)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        t.TempDir(),
		DB:              d,
		Clock:           clk,
		AgentRole:       role,
		HarnessName:     "pi",
		Harness:         pih.New("", role, ""),
		PrismBinaryPath: prismBinaryPath,
	}
	// Seed the coordinator's own row so the /prompt gate can look up the
	// coordinator-for-repo.
	agentName := "coordinator"
	if err := d.UpsertStatusWithRootAgent(sessionName, repo, cfg.Worktree,
		"active", nil, nil, &agentName, nil); err != nil {
		t.Fatalf("seed coordinator status: %v", err)
	}
	return New(cfg)
}

// seedTargetSession seeds a target session row in the same repo so the
// coordinator's cross-session permission gate passes.
func seedTargetSession(t *testing.T, d *db.DB, sessionName, repo string) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, repo, "/tmp/target-wt", "active", nil, nil); err != nil {
		t.Fatalf("seed target session %q: %v", sessionName, err)
	}
}

// Compile-time reference to keep io import live even if the helpers
// evolve — we rely on io.Discard when muting cmd stderr in stubs.
var _ = io.Discard

// Compile-time reference to keep httptest import live even if body
// setup evolves in this file — cross-referenced with doHostAPI's use.
var _ = httptest.NewRecorder
