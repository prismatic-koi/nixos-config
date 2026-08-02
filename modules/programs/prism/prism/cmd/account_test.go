// Tests for the `prism account` cobra subcommands (#2283).
//
// These exercise the cmd-layer behaviour (output formatting, --json,
// active marker, exit-code via err return) on top of the internal
// account package. Lower-level invariants — atomicity, mode bits,
// snapshot-before-swap — are covered by internal/account/account_test.go.
package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// withAccountFixture redirects $XDG_CONFIG_HOME and $PI_AUTH_JSON into a
// per-test tempdir. The accounts directory is NOT pre-created — Init()
// runs lazily via resolveAndInit on the first subcommand invocation.
func withAccountFixture(t *testing.T) (configDir, authPath string) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)

	piDir := filepath.Join(root, "pi-agent")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatalf("mkdir pi-agent: %v", err)
	}
	auth := filepath.Join(piDir, "auth.json")
	t.Setenv("PI_AUTH_JSON", auth)
	return cfg, auth
}

// runSubcommand invokes a subcommand's RunE against a throwaway cobra command
// and returns its combined stdout+stderr.
//
// `no-refresh` defaults to TRUE here, inverting the production default. Unit
// tests must never make a live Anthropic request, and `prism account usage`
// refreshes a missing or stale snapshot by default (#2541). Tests that need
// the refresh path use runAccountUsageWithRefresh in
// account_usage_refresh_test.go, which registers the real production flag set
// via addAccountUsageFlags and redirects the API and the accounts directory
// into fixtures.
func runSubcommand(t *testing.T, runE func(*cobra.Command, []string) error, args []string) (string, error) {
	t.Helper()
	c := &cobra.Command{Use: "test"}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("no-refresh", true, "")
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	err := runE(c, args)
	return buf.String(), err
}

func writeAuthFile(t *testing.T, path string, top map[string]any) {
	t.Helper()
	data, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

func sampleAnthropicCmd(suffix string) map[string]any {
	return map[string]any{
		"type":    "oauth",
		"access":  "access-" + suffix,
		"refresh": "refresh-" + suffix,
		"expires": 1_000_000_000,
	}
}

// ─── list ─────────────────────────────────────────────────────────────

func TestAccountList_MarksActiveAccount(t *testing.T) {
	_, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})

	// First call runs Init() which creates default.json and current ← default.
	out, err := runSubcommand(t, runAccountList, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "* default") {
		t.Errorf("list output %q does not mark default as active", out)
	}
}

func TestAccountList_JSON(t *testing.T) {
	_, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})

	c := &cobra.Command{}
	c.Flags().Bool("json", true, "")
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}
	// Redirect stdout because printJSON writes to os.Stdout directly.
	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = origStdout })

	doneCh := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		doneCh <- buf.Bytes()
	}()

	if err := runAccountList(c, nil); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	_ = w.Close()
	captured := <-doneCh

	var rows []struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(captured), &rows); err != nil {
		t.Fatalf("parse json: %v (raw: %q)", err, captured)
	}
	if len(rows) != 1 || rows[0].Name != "default" || !rows[0].Active {
		t.Errorf("unexpected json output: %+v", rows)
	}
}

// ─── current ──────────────────────────────────────────────────────────

func TestAccountCurrent_PrintsActiveName(t *testing.T) {
	_, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})

	out, err := runSubcommand(t, runAccountCurrent, nil)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if strings.TrimSpace(out) != "default" {
		t.Errorf("current = %q, want \"default\"", out)
	}
}

func TestAccountCurrent_NoActive_PrintsNoneAndExitsZero(t *testing.T) {
	_, _ = withAccountFixture(t)
	// auth.json absent, so Init creates accounts/ but no current pointer.
	out, err := runSubcommand(t, runAccountCurrent, nil)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if strings.TrimSpace(out) != "none" {
		t.Errorf("current = %q, want \"none\"", out)
	}
}

// ─── save ─────────────────────────────────────────────────────────────

func TestAccountSave_WritesBlob(t *testing.T) {
	cfg, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})

	_, err := runSubcommand(t, runAccountSave, []string{"work"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg, "prism", "accounts", "work.json"))
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["access"] != "access-A" {
		t.Errorf("snapshot mismatch: %v", got)
	}
}

func TestAccountSave_NoAnthropicKey_NonZeroExit(t *testing.T) {
	cfg, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"github-copilot": map[string]any{"token": "gh"}})

	_, err := runSubcommand(t, runAccountSave, []string{"work"})
	if err == nil {
		t.Fatal("save: want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(cfg, "prism", "accounts", "work.json")); !os.IsNotExist(statErr) {
		t.Errorf("save created file on error: %v", statErr)
	}
}

// ─── use ──────────────────────────────────────────────────────────────

func TestAccountUse_UnknownName_NonZeroAndDoesNotMutateAuth(t *testing.T) {
	_, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})

	before, _ := os.ReadFile(auth)
	_, err := runSubcommand(t, runAccountUse, []string{"ghost"})
	if err == nil {
		t.Fatal("use(ghost): want error, got nil")
	}
	after, _ := os.ReadFile(auth)
	if string(before) != string(after) {
		t.Errorf("auth.json mutated on failed use")
	}
}

func TestAccountUse_SwapsAndUpdatesCurrent(t *testing.T) {
	cfg, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})
	// Init populates accounts/default.json + current ← default.
	if _, err := runSubcommand(t, runAccountList, nil); err != nil {
		t.Fatalf("init via list: %v", err)
	}

	// Hand-write "work" so Save isn't required for this test.
	workBlob, _ := json.Marshal(sampleAnthropicCmd("B"))
	if err := os.WriteFile(filepath.Join(cfg, "prism", "accounts", "work.json"), workBlob, 0o600); err != nil {
		t.Fatalf("write work: %v", err)
	}

	_, err := runSubcommand(t, runAccountUse, []string{"work"})
	if err != nil {
		t.Fatalf("use: %v", err)
	}

	data, _ := os.ReadFile(auth)
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	anth, _ := got["anthropic"].(map[string]any)
	if anth["access"] != "access-B" {
		t.Errorf("auth.json not swapped: %v", anth)
	}

	curPath := filepath.Join(cfg, "prism", "accounts", "current")
	curData, _ := os.ReadFile(curPath)
	if strings.TrimSpace(string(curData)) != "work" {
		t.Errorf("current = %q, want \"work\"", curData)
	}
}

// ─── rm ───────────────────────────────────────────────────────────────

func TestAccountRm_DeletesFile(t *testing.T) {
	cfg, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})
	// Set up default + a non-active "work" account.
	if _, err := runSubcommand(t, runAccountList, nil); err != nil {
		t.Fatalf("init via list: %v", err)
	}
	workPath := filepath.Join(cfg, "prism", "accounts", "work.json")
	if err := os.WriteFile(workPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write work: %v", err)
	}

	if _, err := runSubcommand(t, runAccountRm, []string{"work"}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := os.Stat(workPath); !os.IsNotExist(err) {
		t.Errorf("rm did not delete file: %v", err)
	}
}

func TestAccountRm_RefusesActive(t *testing.T) {
	cfg, auth := withAccountFixture(t)
	writeAuthFile(t, auth, map[string]any{"anthropic": sampleAnthropicCmd("A")})
	if _, err := runSubcommand(t, runAccountList, nil); err != nil {
		t.Fatalf("init via list: %v", err)
	}
	_, err := runSubcommand(t, runAccountRm, []string{"default"})
	if err == nil {
		t.Fatal("rm(default): want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(cfg, "prism", "accounts", "default.json")); statErr != nil {
		t.Errorf("rm(active) deleted file: %v", statErr)
	}
}

func TestAccountRm_NonExistent_Errors(t *testing.T) {
	_, _ = withAccountFixture(t)
	_, err := runSubcommand(t, runAccountRm, []string{"ghost"})
	if err == nil {
		t.Fatal("rm(ghost): want error, got nil")
	}
}
