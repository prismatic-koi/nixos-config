package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// withStateHome redirects $XDG_STATE_HOME to a per-test temp dir and returns
// the prism state subdirectory path callers can inspect for the state file.
func withStateHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	return filepath.Join(tmp, "prism")
}

func TestActiveProfilePath_HonoursXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/pretend-state")
	got, err := config.ActiveProfilePath()
	if err != nil {
		t.Fatalf("ActiveProfilePath: %v", err)
	}
	want := "/tmp/pretend-state/prism/active-profile"
	if got != want {
		t.Errorf("ActiveProfilePath = %q, want %q", got, want)
	}
}

func TestActiveProfilePath_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/pretend-home")
	got, err := config.ActiveProfilePath()
	if err != nil {
		t.Fatalf("ActiveProfilePath: %v", err)
	}
	want := "/tmp/pretend-home/.local/state/prism/active-profile"
	if got != want {
		t.Errorf("ActiveProfilePath = %q, want %q", got, want)
	}
}

func TestActiveProfile_AbsentMeansFallthrough(t *testing.T) {
	withStateHome(t)
	name, ok, err := config.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: unexpected error: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (file does not exist)")
	}
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
}

func TestActiveProfile_EmptyFileMeansFallthrough(t *testing.T) {
	stateDir := withStateHome(t)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "active-profile"), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, ok, err := config.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: unexpected error: %v", err)
	}
	if ok || name != "" {
		t.Errorf("got (%q,%v), want empty fallthrough", name, ok)
	}
}

func TestActiveProfile_ReadsTrimmedName(t *testing.T) {
	stateDir := withStateHome(t)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "active-profile"), []byte("  gemini-hybrid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, ok, err := config.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if name != "gemini-hybrid" {
		t.Errorf("name = %q, want gemini-hybrid", name)
	}
}

func TestSetActiveProfile_RejectsUnknownProfile(t *testing.T) {
	withStateHome(t)
	pf := sampleProfilesFile()
	err := config.SetActiveProfile(pf, "bogus")
	if err == nil {
		t.Fatal("SetActiveProfile(bogus): want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("error %q does not mention unknown profile", err)
	}
	// The error should list valid names so the user can copy-paste.
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "gemini-hybrid") {
		t.Errorf("error %q does not list valid profile names", err)
	}
}

func TestSetActiveProfile_RejectsProfileMissingRequiredSlot(t *testing.T) {
	stateDir := withStateHome(t)
	pf := sampleProfilesFile()
	// Mutilate the profile so it lacks the required worker slot.
	entry := pf.Profiles["anthropic"]
	delete(entry, "worker")
	pf.Profiles["anthropic"] = entry

	err := config.SetActiveProfile(pf, "anthropic")
	if err == nil {
		t.Fatal("SetActiveProfile: want error for missing worker slot")
	}
	if !strings.Contains(err.Error(), "worker") {
		t.Errorf("error %q does not mention missing worker slot", err)
	}
	// Critically: nothing should have been written. A failed validation
	// must not corrupt the on-disk state.
	if _, statErr := os.Stat(filepath.Join(stateDir, "active-profile")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("state file exists after failed validation: %v", statErr)
	}
}

func TestSetActiveProfile_RejectsNilProfilesFile(t *testing.T) {
	withStateHome(t)
	err := config.SetActiveProfile(nil, "anthropic")
	if err == nil {
		t.Fatal("SetActiveProfile(nil, ...): want error")
	}
}

func TestSetActiveProfile_WritesValidProfile(t *testing.T) {
	stateDir := withStateHome(t)
	pf := sampleProfilesFile()
	if err := config.SetActiveProfile(pf, "gemini-hybrid"); err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "active-profile"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	got := strings.TrimRight(string(data), "\n")
	if got != "gemini-hybrid" {
		t.Errorf("state file = %q, want gemini-hybrid", got)
	}
	// Round-trip through ActiveProfile to confirm it parses back correctly.
	name, ok, err := config.ActiveProfile()
	if err != nil || !ok {
		t.Fatalf("ActiveProfile after Set: ok=%v err=%v", ok, err)
	}
	if name != "gemini-hybrid" {
		t.Errorf("ActiveProfile = %q, want gemini-hybrid", name)
	}
}

func TestSetActiveProfile_OverwritesExistingFile(t *testing.T) {
	withStateHome(t)
	pf := sampleProfilesFile()
	if err := config.SetActiveProfile(pf, "anthropic"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := config.SetActiveProfile(pf, "gemini-hybrid"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	name, ok, err := config.ActiveProfile()
	if err != nil || !ok {
		t.Fatalf("ActiveProfile: ok=%v err=%v", ok, err)
	}
	if name != "gemini-hybrid" {
		t.Errorf("ActiveProfile = %q, want gemini-hybrid (overwrite)", name)
	}
}

func TestResolveActiveProfile_FlagWinsOverStateAndDefault(t *testing.T) {
	withStateHome(t)
	pf := sampleProfilesFile()
	if err := config.SetActiveProfile(pf, "gemini-hybrid"); err != nil {
		t.Fatal(err)
	}
	name, source, err := config.ResolveActiveProfile(pf, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if name != "anthropic" {
		t.Errorf("name = %q, want anthropic (flag)", name)
	}
	if source != "flag" {
		t.Errorf("source = %q, want flag", source)
	}
}

func TestResolveActiveProfile_StateFileWinsOverDefault(t *testing.T) {
	withStateHome(t)
	pf := sampleProfilesFile() // Default: anthropic
	if err := config.SetActiveProfile(pf, "gemini-hybrid"); err != nil {
		t.Fatal(err)
	}
	name, source, err := config.ResolveActiveProfile(pf, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "gemini-hybrid" {
		t.Errorf("name = %q, want gemini-hybrid (state file)", name)
	}
	if source != "state-file" {
		t.Errorf("source = %q, want state-file", source)
	}
}

func TestResolveActiveProfile_FallsBackToNixDefault(t *testing.T) {
	withStateHome(t)
	pf := sampleProfilesFile() // Default: anthropic, no state file written
	name, source, err := config.ResolveActiveProfile(pf, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "anthropic" {
		t.Errorf("name = %q, want anthropic (nix default)", name)
	}
	if source != "nix-default" {
		t.Errorf("source = %q, want nix-default", source)
	}
}

func TestResolveActiveProfile_NoneWhenAllAbsent(t *testing.T) {
	withStateHome(t)
	pf := &config.ProfilesFile{Default: "", Profiles: map[string]config.ProfileEntry{}}
	name, source, err := config.ResolveActiveProfile(pf, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" || source != "none" {
		t.Errorf("got (%q,%q), want (\"\",\"none\")", name, source)
	}
}

func TestResolveActiveProfile_NilProfilesFile(t *testing.T) {
	withStateHome(t)
	// With a nil pf, only flag and state file can produce a value.
	name, source, err := config.ResolveActiveProfile(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" || source != "none" {
		t.Errorf("got (%q,%q), want (\"\",\"none\") with nil pf", name, source)
	}
	// Flag still wins even with nil pf.
	name, source, err = config.ResolveActiveProfile(nil, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if name != "anthropic" || source != "flag" {
		t.Errorf("got (%q,%q), want (\"anthropic\",\"flag\")", name, source)
	}
}

func TestRequiredSlotsCoverCoordinatorAndWorker(t *testing.T) {
	// Defensive test: the AC explicitly says "coordinator and worker minimum".
	// If someone changes RequiredSlots later, this test catches the regression.
	want := map[string]bool{"coordinator": true, "worker": true}
	for _, slot := range config.RequiredSlots {
		delete(want, slot)
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for k := range want {
			missing = append(missing, k)
		}
		t.Errorf("RequiredSlots missing required entries: %v", missing)
	}
}
