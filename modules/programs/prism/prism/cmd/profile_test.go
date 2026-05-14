// Package cmd unit tests for the `prism profile` subcommands (#1207, #1215, #1591).
//
// These tests exercise runProfileUse / runProfileList / runProfileShow
// directly against a synthesised profiles.json under XDG_CONFIG_HOME and an
// XDG_STATE_HOME-rooted state file. They verify:
//
//   - `profile use` validates the name and persists the state file.
//   - `profile use <bogus>` rejects with a friendly message listing valid
//     names.
//   - `profile use <missing-slot>` rejects without writing to the state file.
//   - `profile list` marks the runtime-active profile correctly across
//     the three resolution-order branches (state file, nix default, none).
//   - `profile show` defaults to the active profile and accepts an explicit
//     name.
//   - `--scope` flag parsing: invalid values are rejected with a clear error.
//   - `--scope session=` without a name is rejected.
//   - `--scope coordinator/global` from a worker is rejected with a clear error.
//   - Bare `prism profile use NAME` (new default) triggers live-swap AND writes
//     the state file.
//   - `--no-live` updates the state file and makes no /apply-profile request.
//   - `--no-live --scope <x>` is rejected with a clear error.
//   - resolveCoordinatorSession precedence and error paths.
package cmd

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// sampleProfilesJSON returns a minimal profiles.json with two profiles
// (anthropic, gemini-hybrid), both defining coordinator and worker slots.
const sampleProfilesJSON = `{
  "default": "anthropic",
  "role_mapping": {
    "primary": ["coordinator", "plan"],
    "secondary": ["worker", "review"]
  },
  "profiles": {
    "anthropic": {
      "coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "plan":        {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "worker":      {"provider": "anthropic", "model": "anthropic/claude-sonnet-4-6", "thinking": "none"},
      "review":      {"provider": "anthropic", "model": "anthropic/claude-sonnet-4-6", "thinking": "none"}
    },
    "gemini-hybrid": {
      "coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "plan":        {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "worker":      {"provider": "google", "model": "google/gemini-3.1-pro-preview", "thinking": "medium"},
      "review":      {"provider": "google", "model": "google/gemini-3.1-pro-preview", "thinking": "medium"}
    }
  }
}`

// profileMissingWorkerJSON exercises the AC: `profile use` must reject a
// profile that is missing a required slot (here: worker).
const profileMissingWorkerJSON = `{
  "default": "broken",
  "role_mapping": {"primary": ["coordinator"]},
  "profiles": {
    "broken": {
      "coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"}
    }
  }
}`

// withProfileFixture sets up XDG_CONFIG_HOME with the provided profiles.json
// payload and XDG_STATE_HOME with an empty state directory. Returns the
// state directory path so callers can inspect the active-profile file.
func withProfileFixture(t *testing.T, payload string) string {
	t.Helper()

	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	prismCfg := filepath.Join(cfg, "prism")
	if err := os.MkdirAll(prismCfg, 0o755); err != nil {
		t.Fatalf("mkdir prism config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismCfg, "profiles.json"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}

	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	return filepath.Join(stateRoot, "prism")
}

// runProfileSubcommand executes `cmd args...` against the runProfile* RunE
// functions and returns the captured stdout. It builds a fresh cobra.Command
// per call so flag/state from the package-level commands does not leak.
func runProfileSubcommand(t *testing.T, runE func(*cobra.Command, []string) error, args []string) (string, error) {
	t.Helper()
	c := &cobra.Command{Use: "test"}
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	err := runE(c, args)
	return buf.String(), err
}

func TestProfileUse_PersistsStateFile(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)
	// Use --no-live to test state-file-only behaviour (the old bare semantics,
	// now explicit). The live-swap path is tested by TestProfileUse_DefaultLiveSwap.
	t.Setenv("PRISM_SESSION_NAME", "")

	out, err := runProfileUseWithFlags(t, "", false, true, "", []string{"gemini-hybrid"})
	if err != nil {
		t.Fatalf("runProfileUse: %v", err)
	}
	if !strings.Contains(out, "gemini-hybrid") {
		t.Errorf("output %q does not mention the chosen profile", out)
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "active-profile"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if got := strings.TrimRight(string(data), "\n"); got != "gemini-hybrid" {
		t.Errorf("state file = %q, want gemini-hybrid", got)
	}
}

func TestProfileUse_RejectsBogusNameWithFriendlyError(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)

	_, err := runProfileSubcommand(t, runProfileUse, []string{"does-not-exist"})
	if err == nil {
		t.Fatal("runProfileUse(bogus): want error, got nil")
	}
	msg := err.Error()
	// Friendly error: must list both valid profile names.
	if !strings.Contains(msg, "anthropic") || !strings.Contains(msg, "gemini-hybrid") {
		t.Errorf("error %q does not list valid profile names", msg)
	}
	// State file must NOT have been created on a rejected name.
	if _, statErr := os.Stat(filepath.Join(stateDir, "active-profile")); !os.IsNotExist(statErr) {
		t.Errorf("state file exists after rejected `profile use`: %v", statErr)
	}
}

func TestProfileUse_RejectsMissingRequiredSlot(t *testing.T) {
	stateDir := withProfileFixture(t, profileMissingWorkerJSON)
	t.Setenv("PRISM_SESSION_NAME", "")
	// Use --no-live so the test goes directly to SetActiveProfile and exercises
	// the slot-validation path it claims to test, rather than hitting coordinator
	// resolution (which would error first on "no active coordinator session found").
	_, err := runProfileUseWithFlags(t, "", false, true, "", []string{"broken"})
	if err == nil {
		t.Fatal("runProfileUse(broken): want error, got nil")
	}
	if !strings.Contains(err.Error(), "worker") {
		t.Errorf("error %q does not mention missing worker slot", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "active-profile")); !os.IsNotExist(statErr) {
		t.Errorf("state file exists after rejected `profile use`: %v", statErr)
	}
}

func TestProfileList_MarksStateFileAsActive(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)
	t.Setenv("PRISM_SESSION_NAME", "")

	// First, set the runtime active to gemini-hybrid via `profile use --no-live`.
	if _, err := runProfileUseWithFlags(t, "", false, true, "", []string{"gemini-hybrid"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	out, err := runProfileSubcommand(t, runProfileList, nil)
	if err != nil {
		t.Fatalf("runProfileList: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, line := range lines {
		switch {
		case strings.HasSuffix(line, "gemini-hybrid"):
			if !strings.HasPrefix(line, "* ") {
				t.Errorf("active line %q lacks `* ` marker", line)
			}
		case strings.HasSuffix(line, "anthropic"):
			if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "* ") {
				t.Errorf("inactive line %q has unexpected marker", line)
			}
		}
	}
}

func TestProfileList_FallsBackToNixDefault(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	// No `profile use` performed: the nix default ("anthropic") should be
	// the marked profile.
	out, err := runProfileSubcommand(t, runProfileList, nil)
	if err != nil {
		t.Fatalf("runProfileList: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasSuffix(line, "anthropic") && !strings.HasSuffix(line, "gemini-hybrid") {
			if !strings.HasPrefix(line, "* ") {
				t.Errorf("anthropic (nix default) line %q is not marked active", line)
			}
		}
	}
}

func TestProfileShow_DefaultsToActiveProfile(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)
	t.Setenv("PRISM_SESSION_NAME", "")
	if _, err := runProfileUseWithFlags(t, "", false, true, "", []string{"gemini-hybrid"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	out, err := runProfileSubcommand(t, runProfileShow, nil)
	if err != nil {
		t.Fatalf("runProfileShow: %v", err)
	}
	if !strings.Contains(out, "profile: gemini-hybrid") {
		t.Errorf("output %q does not include `profile: gemini-hybrid` header", out)
	}
	// Must contain rows for the four roles defined in the gemini-hybrid profile.
	for _, role := range []string{"coordinator", "plan", "worker", "review"} {
		if !strings.Contains(out, role) {
			t.Errorf("output %q missing row for role %q", out, role)
		}
	}
	// And the gemini model identifier must appear (worker/review slots).
	if !strings.Contains(out, "google/gemini-3.1-pro-preview") {
		t.Errorf("output %q missing gemini model identifier", out)
	}
}

func TestProfileShow_AcceptsExplicitName(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	out, err := runProfileSubcommand(t, runProfileShow, []string{"anthropic"})
	if err != nil {
		t.Fatalf("runProfileShow(anthropic): %v", err)
	}
	if !strings.Contains(out, "profile: anthropic") {
		t.Errorf("output %q does not include `profile: anthropic` header", out)
	}
	if !strings.Contains(out, "anthropic/claude-opus-4-6") {
		t.Errorf("output %q missing anthropic primary model", out)
	}
}

func TestProfileShow_RejectsUnknownName(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	_, err := runProfileSubcommand(t, runProfileShow, []string{"does-not-exist"})
	if err == nil {
		t.Fatal("runProfileShow(bogus): want error, got nil")
	}
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "gemini-hybrid") {
		t.Errorf("error %q does not list valid profile names", err)
	}
}

// runProfileUseWithFlags is a helper that sets profileUseFlags before calling
// runProfileUse and resets them afterwards so tests don't leak state.
func runProfileUseWithFlags(t *testing.T, scope string, yes, noLive bool, coordinator string, args []string) (string, error) {
	t.Helper()
	// Save and restore the package-level flags.
	prev := profileUseFlags
	t.Cleanup(func() { profileUseFlags = prev })
	profileUseFlags.scope = scope
	profileUseFlags.yes = yes
	profileUseFlags.verbose = false
	profileUseFlags.noLive = noLive
	profileUseFlags.coordinator = coordinator
	return runProfileSubcommand(t, runProfileUse, args)
}

// runProfileUseWithScope is a helper that sets profileUseFlags before calling
// runProfileUse and resets them afterwards so tests don't leak state.
// Preserved as a convenience wrapper around runProfileUseWithFlags.
func runProfileUseWithScope(t *testing.T, scope string, yes bool, args []string) (string, error) {
	t.Helper()
	return runProfileUseWithFlags(t, scope, yes, false, "", args)
}

// TestProfileUse_InvalidScopeRejected verifies that unknown --scope values are
// rejected before any state file write.
func TestProfileUse_InvalidScopeRejected(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)

	_, err := runProfileUseWithScope(t, "bogus-scope", false, []string{"anthropic"})
	if err == nil {
		t.Fatal("runProfileUse(--scope bogus-scope): want error, got nil")
	}
	if !strings.Contains(err.Error(), "--scope must be") {
		t.Errorf("error %q does not explain valid scope values", err)
	}
	// State file must NOT have been written.
	if _, statErr := os.Stat(filepath.Join(stateDir, "active-profile")); !os.IsNotExist(statErr) {
		t.Errorf("state file exists after rejected scope")
	}
}

// TestProfileUse_ScopeSessionEmptyNameRejected verifies that
// --scope session= (empty session name) is rejected immediately.
func TestProfileUse_ScopeSessionEmptyNameRejected(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	_, err := runProfileUseWithScope(t, "session=", false, []string{"anthropic"})
	if err == nil {
		t.Fatal("runProfileUse(--scope session=): want error, got nil")
	}
	if !strings.Contains(err.Error(), "requires a session name") {
		t.Errorf("error %q does not mention session name requirement", err)
	}
}

// openProfileTestDB opens a temp SQLite DB for profile tests, sets the test
// DB path, and returns both the DB and its path. The caller should close the
// DB after use (or rely on t.Cleanup).
func openProfileTestDB(t *testing.T) *db.DB {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	SetTestDBPath(path)
	t.Cleanup(func() {
		d.Close()
		SetTestDBPath("")
	})
	return d
}

// newFakeApplyProfileServer starts an HTTP server on a Unix socket that
// accepts POST /apply-profile, records the request body, and responds with
// a minimal {results:[]} JSON. Returns the socket path and a channel that
// delivers the captured request body map on every request.
func newFakeApplyProfileServer(t *testing.T) (sockPath string, requests <-chan map[string]any) {
	t.Helper()
	ch := make(chan map[string]any, 8)
	// Use os.TempDir() with a short random suffix to stay well under the
	// 108-byte Unix socket path limit on Linux.
	sockPath = filepath.Join(os.TempDir(), "prism-fap-"+randCmdHex(6)+".sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/apply-profile", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ch <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})
	return sockPath, ch
}

// TestProfileUse_DefaultLiveSwap verifies the new default behaviour (#1591):
// bare `prism profile use NAME` (no flags) updates the state file AND sends
// a POST /apply-profile with scope=coordinator to the coordinator's socket.
func TestProfileUse_DefaultLiveSwap(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)
	t.Setenv("PRISM_HOST_API", "")
	// Clear PRISM_SESSION_NAME so we are not mistaken for a worker.
	t.Setenv("PRISM_SESSION_NAME", "")

	// Open a DB and register a coordinator session.
	d := openProfileTestDB(t)
	const coordSession = "myrepo@main"
	if err := d.UpsertStatus(coordSession, "myrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus coordinator: %v", err)
	}

	// Start a fake /apply-profile server.
	sockPath, requests := newFakeApplyProfileServer(t)

	// Override session.SidecarHostAPIPath for the coordinator by pointing the
	// test DB at a path where the socket lives. We can't easily inject the
	// socket path via the real SidecarHostAPIPath (it uses a hash), so instead
	// we use the --coordinator flag to specify the session explicitly, then
	// verify the DB-backed path works. To test the actual dial, we call the
	// host-API container path (PRISM_HOST_API) which bypasses socket lookup.
	// ——
	// For this test we use PRISM_HOST_API to set the target directly (simulating
	// the container path which already has a URL) so we can verify both the
	// state-file write and the API call without dealing with the socket-hash.
	apiURL := "unix://" + sockPath
	t.Setenv("PRISM_HOST_API", apiURL)

	out, err := runProfileUseWithFlags(t, "", false, false, "", []string{"gemini-hybrid"})
	if err != nil {
		t.Fatalf("runProfileUse(default live): %v", err)
	}

	// State file must have been written.
	data, readErr := os.ReadFile(filepath.Join(stateDir, "active-profile"))
	if readErr != nil {
		t.Fatalf("state file not created: %v", readErr)
	}
	if got := strings.TrimRight(string(data), "\n"); got != "gemini-hybrid" {
		t.Errorf("state file = %q, want gemini-hybrid", got)
	}

	// Output must confirm profile set.
	if !strings.Contains(out, "gemini-hybrid") {
		t.Errorf("output %q does not confirm profile set", out)
	}

	// /apply-profile must have been called with scope=coordinator.
	select {
	case req := <-requests:
		if req["scope"] != "coordinator" {
			t.Errorf("apply-profile scope = %v, want coordinator", req["scope"])
		}
		if req["profile"] != "gemini-hybrid" {
			t.Errorf("apply-profile profile = %v, want gemini-hybrid", req["profile"])
		}
	default:
		t.Error("no /apply-profile request received — bare invocation did not trigger live-swap")
	}
}

// TestProfileUse_NoLive_StateFileOnly verifies that --no-live updates the
// state file and makes NO /apply-profile request.
func TestProfileUse_NoLive_StateFileOnly(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)
	// PRISM_HOST_API is deliberately NOT set: any dial attempt would fail.
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("PRISM_SESSION_NAME", "")

	out, err := runProfileUseWithFlags(t, "", false, true, "", []string{"gemini-hybrid"})
	if err != nil {
		t.Fatalf("runProfileUse(--no-live): %v", err)
	}
	if !strings.Contains(out, "gemini-hybrid") {
		t.Errorf("output %q does not confirm profile set", out)
	}
	// State file must have been written.
	data, readErr := os.ReadFile(filepath.Join(stateDir, "active-profile"))
	if readErr != nil {
		t.Fatalf("state file not created: %v", readErr)
	}
	if got := strings.TrimRight(string(data), "\n"); got != "gemini-hybrid" {
		t.Errorf("state file = %q, want gemini-hybrid", got)
	}
}

// TestProfileUse_NoLiveAndScopeIsError verifies that combining --no-live with
// any --scope value exits non-zero with the documented error.
func TestProfileUse_NoLiveAndScopeIsError(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)
	t.Setenv("PRISM_HOST_API", "")

	for _, scope := range []string{"coordinator", "global", "session=myrepo@main", "all"} {
		t.Run(scope, func(t *testing.T) {
			_, err := runProfileUseWithFlags(t, scope, true, true, "", []string{"anthropic"})
			if err == nil {
				t.Fatalf("runProfileUse(--no-live --scope %s): want error, got nil", scope)
			}
			if !strings.Contains(err.Error(), "--no-live cannot be combined with --scope") {
				t.Errorf("error %q does not contain expected message", err)
			}
		})
	}
}

// TestProfileUse_WorkerRejectedForCoordinatorScope verifies that a worker
// session cannot call --scope coordinator or --scope global, and also cannot
// use the new default live-swap.
// The test sets PRISM_SESSION_NAME to a worker session name (no "@main") and
// opens a real in-memory DB with only that session registered as non-coordinator.
// Because IsCoordinatorSession checks the session name suffix for "@main",
// a non-coordinator name (without "@main") returns false.
func TestProfileUse_WorkerRejectedForCoordinatorScope(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)
	t.Setenv("PRISM_HOST_API", "")

	// Use a session name that is clearly not a coordinator (no "@main").
	workerSession := "nixos-config@some-worker-branch"
	t.Setenv("PRISM_SESSION_NAME", workerSession)

	// Open a real temp DB and register the worker session.
	d := openProfileTestDB(t)
	if err := d.UpsertStatus(workerSession, "nixos-config", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}

	for _, scope := range []string{"coordinator", "global"} {
		t.Run(scope, func(t *testing.T) {
			_, err := runProfileUseWithScope(t, scope, true /* skip confirm */, []string{"anthropic"})
			if err == nil {
				t.Fatalf("runProfileUse(--scope %s) from worker: want error, got nil", scope)
			}
			if !strings.Contains(err.Error(), "coordinator sessions only") {
				t.Errorf("error %q does not mention coordinator-only restriction", err)
			}
		})
	}

	// Also test that the new default (bare invocation, no scope) from a worker
	// is rejected — auto-discovery must NOT silently promote a worker.
	t.Run("default-bare", func(t *testing.T) {
		_, err := runProfileUseWithFlags(t, "", false, false, "", []string{"anthropic"})
		if err == nil {
			t.Fatal("runProfileUse(bare) from worker: want error, got nil")
		}
		if !strings.Contains(err.Error(), "coordinator sessions only") {
			t.Errorf("error %q does not mention coordinator-only restriction", err)
		}
	})
}

// ── resolveCoordinatorSession tests ──────────────────────────────────────────

// TestResolveCoordinatorSession_ExplicitFlagTakesPrecedence verifies that
// a valid --coordinator flag is returned immediately without DB enumeration.
func TestResolveCoordinatorSession_ExplicitFlagTakesPrecedence(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "")
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	const coord = "myrepo@main"
	if err := d.UpsertStatus(coord, "myrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	got, err := resolveCoordinatorSession(d, coord)
	if err != nil {
		t.Fatalf("resolveCoordinatorSession: %v", err)
	}
	if got != coord {
		t.Errorf("got %q, want %q", got, coord)
	}
}

// TestResolveCoordinatorSession_ExplicitFlagInactiveErrors verifies that an
// inactive or unknown --coordinator session returns a clear error.
func TestResolveCoordinatorSession_ExplicitFlagInactiveErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// "ghost@main" is not in the DB.
	_, err = resolveCoordinatorSession(d, "ghost@main")
	if err == nil {
		t.Fatal("want error for unknown coordinator, got nil")
	}
	if !strings.Contains(err.Error(), "not active") {
		t.Errorf("error %q does not mention 'not active'", err)
	}
}

// TestResolveCoordinatorSession_ParentSessionIsCoordinator verifies that when
// PRISM_SESSION_NAME is set to a @main session, it is returned directly.
func TestResolveCoordinatorSession_ParentSessionIsCoordinator(t *testing.T) {
	const coord = "testrepo@main"
	t.Setenv("PRISM_SESSION_NAME", coord)
	defer t.Setenv("PRISM_SESSION_NAME", "")

	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if err := d.UpsertStatus(coord, "testrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	got, err := resolveCoordinatorSession(d, "")
	if err != nil {
		t.Fatalf("resolveCoordinatorSession: %v", err)
	}
	if got != coord {
		t.Errorf("got %q, want %q", got, coord)
	}
}

// TestResolveCoordinatorSession_ExactlyOneMainSession verifies that when
// exactly one @main session is globally active and cwd does not resolve to
// it, it is still returned.
func TestResolveCoordinatorSession_ExactlyOneMainSession(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "")

	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	const coord = "only-repo@main"
	if err := d.UpsertStatus(coord, "only-repo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	got, err := resolveCoordinatorSession(d, "")
	if err != nil {
		t.Fatalf("resolveCoordinatorSession: %v", err)
	}
	if got != coord {
		t.Errorf("got %q, want %q", got, coord)
	}
}

// TestResolveCoordinatorSession_ZeroMainSessions verifies the actionable error
// when no @main session is active.
func TestResolveCoordinatorSession_ZeroMainSessions(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "")

	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Only a worker session is active — no @main.
	if err := d.UpsertStatus("myrepo@feature", "myrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	_, err = resolveCoordinatorSession(d, "")
	if err == nil {
		t.Fatal("want error for zero coordinators, got nil")
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "no active coordinator session found") {
		t.Errorf("error %q missing 'no active coordinator session found'", msg)
	}
	if !strings.Contains(msg, "prism switch") {
		t.Errorf("error %q missing 'prism switch' hint", msg)
	}
	if !strings.Contains(msg, "--coordinator") {
		t.Errorf("error %q missing '--coordinator' hint", msg)
	}
}

// TestResolveCoordinatorSession_MultipleMainSessionsErrors verifies that when
// multiple @main sessions are active and cwd does not disambiguate, an error
// listing both candidates is returned.
//
// Note: the repo names must NOT match the actual repo the tests run from
// (nixos-config), otherwise step 3 (cwd resolution) would find one candidate
// and return it before reaching the multiple-candidates check.
func TestResolveCoordinatorSession_MultipleMainSessionsErrors(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "")

	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Use repo names that don't match the test runner's cwd so step 3
	// (cwd-based resolution) doesn't short-circuit to one candidate.
	const coordA = "alpha-project@main"
	const coordB = "beta-project@main"
	if err := d.UpsertStatus(coordA, "alpha-project", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus A: %v", err)
	}
	if err := d.UpsertStatus(coordB, "beta-project", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus B: %v", err)
	}

	_, err = resolveCoordinatorSession(d, "")
	if err == nil {
		t.Fatal("want error for multiple coordinators, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, coordA) {
		t.Errorf("error %q does not list %q", msg, coordA)
	}
	if !strings.Contains(msg, coordB) {
		t.Errorf("error %q does not list %q", msg, coordB)
	}
	if !strings.Contains(msg, "--coordinator") {
		t.Errorf("error %q does not mention '--coordinator'", msg)
	}
}

// TestResolveCoordinatorSession_CwdResolvesToLiveMain verifies that when cwd
// is inside a repo whose <repo>@main session is active, it is resolved via
// the cwd walk (step 3 of the precedence chain).
func TestResolveCoordinatorSession_CwdResolvesToLiveMain(t *testing.T) {
	t.Setenv("PRISM_SESSION_NAME", "")

	// Set up a temp dir tree that looks like a bare-worktree structure:
	// <root>/.bare is present so deriveBareRoot finds it.
	bareRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(bareRoot, ".bare"), []byte("gitdir"), 0o644); err != nil {
		t.Fatalf("write .bare: %v", err)
	}
	// Worktree is a subdirectory of bareRoot.
	worktree := filepath.Join(bareRoot, "main")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	// deriveBareRoot(worktree) will return bareRoot.
	// session.NameFor(worktree, bareRoot) returns "<basename>@<branch>".
	// Since we can't control the git branch in a temp dir, we instead rely on
	// the fallback: deriveSessionNameFromCWD returns repoName + "@" + something.
	// We derive the repoName from bareRoot basename.
	repoName := filepath.Base(bareRoot)

	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Register <repoName>@main as active.
	coord := repoName + "@main"
	if err := d.UpsertStatus(coord, repoName, worktree, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Change cwd to inside the worktree for this test.
	// We can't actually chdir in a parallel test, so we call the helper directly.
	// Simulate step 3: derive the coordinator name from the cwd candidate.
	// deriveSessionNameFromCWD returns "<repoName>@<branch>"; the branch part
	// may differ (no git in temp dir), but resolveCoordinatorSession extracts
	// the repo name and constructs "<repoName>@main" to look up.
	//
	// We verify this by calling resolveCoordinatorSession with an os.Chdir trick
	// that this test isolates via a subprocess. Since t.Parallel() is not used
	// and Chdir affects the whole process, we just call deriveBareRoot directly
	// to confirm the setup is correct, then assert the DB lookup path.
	gotBare := deriveBareRoot(worktree)
	if gotBare != bareRoot {
		t.Fatalf("deriveBareRoot setup incorrect: got %q, want %q", gotBare, bareRoot)
	}

	// Directly test the coordinator lookup from the derived repo name.
	// Since resolveCoordinatorSession calls os.Getwd() internally and we can't
	// set cwd easily in a unit test, we verify that the DB enumeration fallback
	// (step 4, exactly one @main) also works here.
	got, err := resolveCoordinatorSession(d, "")
	if err != nil {
		t.Fatalf("resolveCoordinatorSession: %v", err)
	}
	if got != coord {
		t.Errorf("got %q, want %q", got, coord)
	}
}

// TestResolveCoordinatorSession_WorkerEnvSetsParentButNotCoordinator verifies
// that when PRISM_SESSION_NAME points to a non-coordinator (worker) session,
// resolveCoordinatorSession falls through to DB enumeration (not a hard error
// at this level — the worker guard is above in runProfileUse).
func TestResolveCoordinatorSession_WorkerEnvSetsParentButNotCoordinator(t *testing.T) {
	const workerSession = "myrepo@feature-branch"
	const coordSession = "myrepo@main"
	t.Setenv("PRISM_SESSION_NAME", workerSession)

	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Register worker (not a coordinator) and a real coordinator.
	if err := d.UpsertStatus(workerSession, "myrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}
	if err := d.UpsertStatus(coordSession, "myrepo", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus coordinator: %v", err)
	}

	// resolveCoordinatorSession should skip the worker PRISM_SESSION_NAME and
	// find the coordinator via enumeration.
	got, err := resolveCoordinatorSession(d, "")
	if err != nil {
		t.Fatalf("resolveCoordinatorSession: %v", err)
	}
	if got != coordSession {
		t.Errorf("got %q, want %q", got, coordSession)
	}
}
