//go:build darwin

package integration

// sandbox_exec_pi_agent_darwin_test.go — Darwin integration test that proves the
// shared PI agent ~/.pi/agent read-write SBPL allow round-trips through a real
// /usr/bin/sandbox-exec invocation.
//
// PI stores auth credentials in ~/.pi/agent/auth.json. Design #2031 (PR3 #2034)
// collapsed the former per-session pi-agent staging directory into a single
// shared mount: under sandbox-exec the host filesystem is shared, so
// PI_CODING_AGENT_DIR points at ~/.pi/agent directly. For OAuth token refreshes
// performed inside the sandbox to write back to the host file, the SBPL profile
// must grant read-write access to ~/.pi/agent (section "6a. PI agent dir allow"
// in generateProfile). This is the #1 regression risk called out in #2034.
//
// This test verifies the positive case (auth.json under ~/.pi/agent is readable
// inside the sandbox under the production profile) and the negative case
// (stripping the ~/.pi/agent allow makes the same read fail) — proving the rule
// is load-bearing per the #1192 convention.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// TestSandboxExecPIAgentAuthReadable_Positive proves that ~/.pi/agent/auth.json
// (the shared PI agent config dir, design #2031 PR3 #2034) is readable inside a
// real sandbox-exec invocation under the production SBPL profile.
//
// The test:
//  1. Creates ~/.pi/agent/auth.json on the host.
//  2. Resolves the shared config dir via SharedPIAgentConfigDir (no per-session
//     staging dir is created — that is the whole point of #2034).
//  3. Generates the production SBPL profile for a pi session.
//  4. Runs `/usr/bin/sandbox-exec -f <profile> cat ~/.pi/agent/auth.json` and
//     asserts the read succeeds and returns the expected content.
//
// This is the positive half of the #1192 paired integration test.
func TestSandboxExecPIAgentAuthReadable_Positive(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available; skipping Darwin integration test")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(fakeHome, ".local", "state"))

	// Create ~/.pi/agent/auth.json on the host.
	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	authContent := `{"access_token":"sekret","refresh_token":"r3fr3sh"}`
	authPath := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(authContent), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	// Build a staging HOME so the profile's staging-derived rules resolve.
	stagingHome := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", "pi-agent-it", "home")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatalf("mkdir stagingHome: %v", err)
	}

	sessionName := "nixos-config@pi-agent-it"

	// Resolve the shared config dir. No per-session staging dir is created
	// (design #2031, PR3 #2034); the host path is ~/.pi/agent directly.
	sharedHostDir, _, sharedErr := container.SharedPIAgentConfigDir()
	if sharedErr != nil {
		t.Fatalf("SharedPIAgentConfigDir: %v", sharedErr)
	}
	if sharedHostDir != piAgentDir {
		t.Fatalf("SharedPIAgentConfigDir host dir = %q, want %q", sharedHostDir, piAgentDir)
	}

	// Generate the production SBPL profile for a pi session.
	mgr := container.New(container.Config{
		SessionName: sessionName,
		Worktree:    t.TempDir(),
		InstanceID:  "pi-agent-it",
		Harness:     "pi",
	})
	if _, err := mgr.PrepareSandboxExec(); err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	profilePath := mgr.SandboxExecProfilePathForTest()

	// Run sandbox-exec to cat the host auth.json (the path pi reads inside the
	// sandbox via PI_CODING_AGENT_DIR=~/.pi/agent).
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, "/bin/cat", authPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox-exec cat ~/.pi/agent/auth.json failed: %v\noutput: %s", err, out)
	}
	if string(out) != authContent {
		t.Errorf("auth.json content mismatch:\n got: %s\nwant: %s", out, authContent)
	}

	// Sanity check: ensure the profile actually contains the pi-agent rule
	// before the dedicated negative test.
	profileBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if !strings.Contains(string(profileBytes), ".pi/agent") &&
		!strings.Contains(string(profileBytes), piAgentDir) {
		t.Errorf("profile does not appear to grant pi-agent access:\n%s", profileBytes)
	}
}

// TestSandboxExecPIAgentAuthWriteable_Positive proves the ~/.pi/agent allow is
// read-WRITE, not just read — the load-bearing property for OAuth token refresh
// write-back (the #1 regression risk in #2034). It writes a new auth.json from
// inside the sandbox and asserts the host file is updated.
func TestSandboxExecPIAgentAuthWriteable_Positive(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available; skipping Darwin integration test")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(fakeHome, ".local", "state"))

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	authPath := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"old"}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	stagingHome := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", "pi-agent-it-rw", "home")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatalf("mkdir stagingHome: %v", err)
	}

	mgr := container.New(container.Config{
		SessionName: "nixos-config@pi-agent-it-rw",
		Worktree:    t.TempDir(),
		InstanceID:  "pi-agent-it-rw",
		Harness:     "pi",
	})
	if _, err := mgr.PrepareSandboxExec(); err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}
	profilePath := mgr.SandboxExecProfilePathForTest()

	// Simulate an OAuth token refresh: write a new auth.json from inside the
	// sandbox. The shell redirect lands on the host file via the read-write
	// ~/.pi/agent allow.
	refreshed := `{"access_token":"refreshed"}`
	script := "printf %s " + shellQuote(refreshed) + " > " + shellQuote(authPath)
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, "/bin/sh", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox-exec write ~/.pi/agent/auth.json failed: %v\noutput: %s", err, out)
	}

	// The host file must reflect the in-sandbox write (no staleness regression).
	got, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read host auth.json after in-sandbox write: %v", err)
	}
	if string(got) != refreshed {
		t.Errorf("host auth.json not updated by in-sandbox write:\n got: %s\nwant: %s", got, refreshed)
	}
}

// shellQuote single-quotes s for safe inclusion in a /bin/sh -c command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestSandboxExecPIAgentAuthDenied_Negative proves the ~/.pi/agent SBPL rule is
// load-bearing: with the rule stripped from the profile, the same read is
// denied.
func TestSandboxExecPIAgentAuthDenied_Negative(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available; skipping Darwin integration test")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(fakeHome, ".local", "state"))

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	authContent := `{"access_token":"sekret"}`
	authPath := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(authContent), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	stagingHome := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", "pi-agent-it-neg", "home")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatalf("mkdir stagingHome: %v", err)
	}

	mgr := container.New(container.Config{
		SessionName: "nixos-config@pi-agent-it-neg",
		Worktree:    t.TempDir(),
		InstanceID:  "pi-agent-it-neg",
		Harness:     "pi",
	})
	if _, err := mgr.PrepareSandboxExec(); err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}
	profilePath := mgr.SandboxExecProfilePathForTest()

	profileBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}

	// Strip the (subpath ".../.pi/agent") allow line(s) from the profile.
	lines := strings.Split(string(profileBytes), "\n")
	var kept []string
	for _, ln := range lines {
		if strings.Contains(ln, ".pi/agent") || strings.Contains(ln, piAgentDir) {
			continue // drop the pi-agent allow
		}
		kept = append(kept, ln)
	}
	strippedProfile := strings.Join(kept, "\n")
	strippedPath := filepath.Join(t.TempDir(), "stripped.sb")
	if err := os.WriteFile(strippedPath, []byte(strippedProfile), 0o600); err != nil {
		t.Fatalf("write stripped profile: %v", err)
	}

	// Run sandbox-exec with the stripped profile — the read must now be denied.
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", strippedPath, "/bin/cat", authPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("expected read to be denied with stripped profile, but it succeeded: %s", out)
	}
}
