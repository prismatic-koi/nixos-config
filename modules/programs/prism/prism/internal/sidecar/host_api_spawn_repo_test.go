package sidecar

// Tests for issue #2982: `prism spawn --repo <name>` was silently dropped on
// the host-API proxy path — the sidecar always substituted its own repo, so
// a sandboxed coordinator's --repo request landed in the caller's own repo
// with a success message.
//
// Coverage here is host-API-handler-level (mirrors
// host_api_containers_test.go's pattern): a stub bound to
// Config.PrismBinaryPath captures the argv the handler builds, so these
// tests pin the --repo forwarding decision without a real host-side prism
// spawn subprocess. The unresolvable-value and self-delivery behaviours
// live in the real `resolveRepo` / `selfDeliveryError` functions in
// cmd/spawn.go (see cmd/spawn_repo_messages_test.go) — this file only
// verifies the handler forwards (or omits) the field correctly and surfaces
// a non-zero-exit subprocess failure as an HTTP error rather than a 200.

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHostAPI_Spawn_RepoForwardedWhenSet verifies that when the /spawn
// request body carries {"repo": "other-repo"}, the sidecar forwards
// "--repo other-repo" to the host-side prism spawn subprocess instead of
// substituting its own (derived-from-session-name) repo.
func TestHostAPI_Spawn_RepoForwardedWhenSet(t *testing.T) {
	d := openTestDB(t)

	argsFile := filepath.Join(t.TempDir(), "captured-args")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := `#!/bin/sh
echo "$*" > ` + argsFile + `
echo 'session "other-repo@repo-branch" created'
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	body := `{"branch":"repo-branch","prompt":"hi","repo":"other-repo"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	capturedArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(capturedArgs), "--repo other-repo") {
		t.Errorf("captured args %q do not contain '--repo other-repo'; the field was silently dropped at the host-API boundary (issue #2982)",
			string(capturedArgs))
	}
	// The sidecar's own (derived) repo must NOT be what was forwarded.
	if strings.Contains(string(capturedArgs), "--repo test-repo") {
		t.Errorf("captured args %q forward the sidecar's own repo instead of the requested one", string(capturedArgs))
	}
}

// TestHostAPI_Spawn_RepoDefaultsToOwnRepoWhenAbsent verifies that when the
// request body omits "repo", the sidecar still derives the repo from its
// own session name and forwards that — the pre-existing, unchanged
// behaviour for a client that does not pass --repo.
func TestHostAPI_Spawn_RepoDefaultsToOwnRepoWhenAbsent(t *testing.T) {
	d := openTestDB(t)

	argsFile := filepath.Join(t.TempDir(), "captured-args")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := `#!/bin/sh
echo "$*" > ` + argsFile + `
echo 'session "test-repo@no-repo-branch" created'
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	rr := doHostAPI(t, sc, http.MethodPost, "/spawn",
		`{"branch":"no-repo-branch","prompt":"hi"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	capturedArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(capturedArgs), "--repo test-repo") {
		t.Errorf("captured args %q do not contain '--repo test-repo'; the derive-from-session-name fallback regressed", string(capturedArgs))
	}
}

// TestHostAPI_Spawn_UnresolvableRepo_ReturnsErrorNotSuccess verifies that
// when the forwarded --repo value cannot be resolved host-side (the
// host-side prism spawn subprocess exits non-zero), the handler surfaces an
// HTTP error rather than a 200 — it must not fall back to the sidecar's own
// repo and report success. The real resolveRepo error text (see
// cmd/spawn_repo_messages_test.go) is exercised in cmd; this test only pins
// the handler's propagation of a non-zero exit through this specific
// request shape.
func TestHostAPI_Spawn_UnresolvableRepo_ReturnsErrorNotSuccess(t *testing.T) {
	d := openTestDB(t)

	stubPath := filepath.Join(t.TempDir(), "prism-stub-fail")
	stubScript := `#!/bin/sh
echo "repo \"does-not-exist\" not found under ~/code -- no session was created, so nothing was delivered anywhere" >&2
exit 1
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           clk,
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	body := `{"branch":"whatever","prompt":"hi","repo":"does-not-exist"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/spawn", body)
	if rr.Code == http.StatusOK {
		t.Fatalf("status = 200, want non-2xx for an unresolvable --repo value; body = %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "does-not-exist") {
		t.Errorf("error body %q does not name the unresolvable repo value", rr.Body.String())
	}
}

// TestHostAPI_Spawn_RepoMustBeAPlainName: the host-side prism spawn runs
// `git worktree add` in the root that --repo names. A path lets the caller
// name a .bare that it planted in its own worktree, so the handler refuses
// it before it starts the subprocess. See
// docs/invariants/host-git-commands.md.
func TestHostAPI_Spawn_RepoMustBeAPlainName(t *testing.T) {
	d := openTestDB(t)

	ranFile := filepath.Join(t.TempDir(), "ran")
	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	stubScript := "#!/bin/sh\ntouch " + ranFile + "\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	sc := New(Config{
		SessionName:     "test-repo@main",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@main",
		HarnessURL:      "http://localhost:14000",
		DB:              d,
		Clock:           newTestClock(),
		AgentRole:       "coordinator",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	})

	for _, repo := range []string{
		"test-repo/main/x",
		"/home/u/code/test-repo/main/x",
		"~/code/x",
		"..",
		".",
		`test-repo\main`,
		"-x",
	} {
		body := `{"branch":"b","prompt":"hi","repo":` + strconv.Quote(repo) + `}`
		rr := doHostAPI(t, sc, http.MethodPost, "/spawn", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("repo %q: status = %d, want 400; body = %s", repo, rr.Code, rr.Body.String())
		}
	}
	if _, err := os.Stat(ranFile); err == nil {
		t.Error("the handler started prism spawn for a repo value that is a path")
	}
}
