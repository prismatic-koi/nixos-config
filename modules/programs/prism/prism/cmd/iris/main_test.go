package main

// main_test.go — daemon entry-point tests for the pi_extension_path
// fail-fast guard (issue #1753).
//
// Background: when iris was deployed with no `pi_extension_path` in its
// config, the daemon happily came up, spawned pi children without
// `--extension`, and every session appeared "active" while producing zero
// useful output. We lost ~1h to this before identifying the cause from
// `/proc/<pid>/environ`. The fix is to refuse to start at all when the
// config is missing or stale, so `journalctl --user -u iris` shows the
// cause on the very first restart.
//
// These tests cover the three cases from AC #1753:
//
//   1. PIExtensionPath unset       → runDaemon/startup exit non-zero with
//                                    an error naming `pi_extension_path`
//                                    and the config-file path.
//   2. PIExtensionPath nonexistent → distinct error text naming the missing
//                                    path so the operator can tell the two
//                                    failure modes apart.
//   3. PIExtensionPath valid       → validation passes; the rest of the
//                                    startup sequence is exercised by
//                                    higher-level integration tests.
//
// Isolation: each test uses iristest.NewIsolated to redirect HOME,
// XDG_STATE_HOME, and XDG_CONFIG_HOME under a t.TempDir(), so we never
// read or write the operator's real iris config.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// writeConfig writes an iris config.json containing the given
// pi_extension_path value at the location iris.ResolvePaths() expects.
// If extPath is the empty string, the JSON omits the field entirely
// (equivalent to the "config absent" failure mode after LoadConfig
// merges with defaults).
func writeConfig(t *testing.T, configPath, extPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("MkdirAll config dir: %v", err)
	}
	cfg := map[string]any{}
	if extPath != "" {
		cfg["pi_extension_path"] = extPath
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
}

// TestRunDaemon_FailsWhenExtensionPathUnset is AC #1: with no
// pi_extension_path set, runDaemon must return non-nil and the error
// message must name both the JSON key and the config file path.
//
// We exercise two unset shapes:
//   - config file absent entirely (LoadConfig returns defaults)
//   - config file present with the key omitted
//
// Both should produce the same "unset" error.
func TestRunDaemon_FailsWhenExtensionPathUnset(t *testing.T) {
	cases := []struct {
		name        string
		writeConfig bool
	}{
		{name: "config-file-absent", writeConfig: false},
		{name: "config-file-present-key-omitted", writeConfig: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iristest.NewIsolated(t)
			p := iris.ResolvePaths()
			if tc.writeConfig {
				writeConfig(t, p.ConfigFile, "")
			}

			err := runDaemon()
			if err == nil {
				t.Fatal("runDaemon returned nil; want error")
			}
			msg := err.Error()
			// Must name the JSON key verbatim so the operator can
			// grep their config for it.
			if !strings.Contains(msg, "pi_extension_path") {
				t.Errorf("error %q does not mention `pi_extension_path`", msg)
			}
			// Must name the config-file path so the operator knows
			// which file to edit.
			if !strings.Contains(msg, p.ConfigFile) {
				t.Errorf("error %q does not mention config path %q", msg, p.ConfigFile)
			}
			// "unset" wording distinguishes this case from the
			// "set-but-missing" case below.
			if !strings.Contains(msg, "unset") {
				t.Errorf("error %q does not use the word \"unset\" (distinguishes from the nonexistent-path case)", msg)
			}
		})
	}
}

// TestRunDaemon_FailsWhenExtensionPathMissingOnDisk is AC #3: when the
// config sets pi_extension_path but the file does not exist, the daemon
// must surface this as a distinct error (different wording from the
// "unset" case) and must name the missing path.
func TestRunDaemon_FailsWhenExtensionPathMissingOnDisk(t *testing.T) {
	iristest.NewIsolated(t)
	p := iris.ResolvePaths()
	missing := filepath.Join(t.TempDir(), "does-not-exist", "prism.ts")
	writeConfig(t, p.ConfigFile, missing)

	err := runDaemon()
	if err == nil {
		t.Fatal("runDaemon returned nil; want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pi_extension_path") {
		t.Errorf("error %q does not mention `pi_extension_path`", msg)
	}
	if !strings.Contains(msg, missing) {
		t.Errorf("error %q does not mention the missing path %q", msg, missing)
	}
	// "does not exist" wording is the distinguisher between this case
	// and the "unset" case — AC #3 requires distinct error text.
	if !strings.Contains(msg, "does not exist") {
		t.Errorf("error %q does not use the phrase \"does not exist\" (must be distinct from the unset-path case)", msg)
	}
	// And it must NOT use the unset wording — if both cases produced
	// the same error the AC would not be satisfied.
	if strings.Contains(msg, "unset") {
		t.Errorf("error %q uses \"unset\" wording; the nonexistent-path case must read distinctly", msg)
	}
}

// TestValidateExtensionPath_PassesWhenSetAndExists is AC #4: a valid
// pi_extension_path that points to a real file must produce no error
// from the fail-fast check. We exercise the helper directly here
// rather than running runDaemon end-to-end, because runDaemon proceeds
// to open sockets and block on signals — the helper is what gates the
// rest of startup.
//
// (Issue #1766 removed the bare-`iris` startup() entry point — bare
// `iris` now launches the TUI. The guard remains exercised end-to-end
// by TestRunDaemon_FailsWhenExtensionPathUnset above; `iris daemon` is
// now the only entry point that runs the validateExtensionPath gate at
// process start.)
func TestValidateExtensionPath_PassesWhenSetAndExists(t *testing.T) {
	iristest.NewIsolated(t)
	p := iris.ResolvePaths()
	extFile := filepath.Join(t.TempDir(), "prism.ts")
	if err := os.WriteFile(extFile, []byte("// fake extension\n"), 0o600); err != nil {
		t.Fatalf("WriteFile extension: %v", err)
	}
	writeConfig(t, p.ConfigFile, extFile)

	cfg, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PIExtensionPath != extFile {
		t.Fatalf("LoadConfig: PIExtensionPath = %q, want %q", cfg.PIExtensionPath, extFile)
	}
	if err := validateExtensionPath(cfg, p.ConfigFile); err != nil {
		t.Errorf("validateExtensionPath: unexpected error: %v", err)
	}
}

// TestValidateExtensionPath_DirectErrors pins down the helper's two
// error shapes at unit level, independent of runDaemon. This is the
// cheap, fast regression net — if someone reworks the message text to
// drop `pi_extension_path` or the config path, these fire immediately.
func TestValidateExtensionPath_DirectErrors(t *testing.T) {
	configPath := "/some/iris/config.json"

	t.Run("unset", func(t *testing.T) {
		err := validateExtensionPath(iris.Config{}, configPath)
		if err == nil {
			t.Fatal("expected error for empty PIExtensionPath")
		}
		msg := err.Error()
		for _, want := range []string{"pi_extension_path", "unset", configPath} {
			if !strings.Contains(msg, want) {
				t.Errorf("unset error %q missing substring %q", msg, want)
			}
		}
	})

	t.Run("nonexistent", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope.ts")
		err := validateExtensionPath(iris.Config{PIExtensionPath: missing}, configPath)
		if err == nil {
			t.Fatal("expected error for nonexistent PIExtensionPath")
		}
		msg := err.Error()
		for _, want := range []string{"pi_extension_path", "does not exist", missing, configPath} {
			if !strings.Contains(msg, want) {
				t.Errorf("nonexistent error %q missing substring %q", msg, want)
			}
		}
		if strings.Contains(msg, "unset") {
			t.Errorf("nonexistent error %q must not use \"unset\" wording", msg)
		}
	})
}

