// Package config — active_profile.go
//
// Runtime active-profile state file (#1207).
//
// State file format
// -----------------
//
// Path:    $XDG_STATE_HOME/prism/active-profile
//          (falling back to $HOME/.local/state/prism/active-profile)
// Format:  a single line containing the active profile name, no preamble,
//          no trailing comment. Trailing newline is permitted and stripped
//          on read. Surrounding whitespace is stripped on read.
//
// Resolution order (consumed by `prism spawn` and other future-spawn paths):
//
//   1. Explicit `--profile <name>` flag (highest priority)
//   2. Runtime state file at the path above
//   3. Nix-configured default in profiles.json (`pf.Default`, lowest priority)
//
// Absence of the state file means "fall through to the nix default" — it is
// a non-error condition. Errors from the read path (permission denied,
// unreadable, etc.) ARE surfaced because they signify a real problem with
// the user's state directory.
//
// The file is created (and its parent directory ensured) on first
// `prism profile use`. There is no migration step from the pre-#1207 world
// because the absence-means-default contract preserves the previous
// behaviour exactly.
//
// Validation
// ----------
//
// Writes through SetActiveProfile validate the profile name against
// `pf.Profiles` and require both `coordinator` and `worker` slots to be
// defined. This is the gate from AC: invalid state never reaches the file
// system in the first place. Reads through ActiveProfile do NOT re-validate;
// validation at spawn time is the responsibility of the spawn flow (which
// already calls RequireSlot).

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// activeProfileFileName is the basename of the runtime state file.
const activeProfileFileName = "active-profile"

// RequiredSlots is the minimal set of role slots a profile must define for
// it to be eligible as the active profile. Spawning prism sessions requires
// at least a coordinator (main-branch sessions) and a worker (everything
// else); without those two slots a session cannot be created.
//
// Exposed so tests and other callers can reason about the validation gate.
var RequiredSlots = []string{"coordinator", "worker"}

// ActiveProfilePath returns the absolute path to the runtime active-profile
// state file. It honours $XDG_STATE_HOME and falls back to
// $HOME/.local/state when XDG_STATE_HOME is empty.
//
// Returns "" together with an error if neither $XDG_STATE_HOME nor a home
// directory can be resolved.
func ActiveProfilePath() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("active-profile: resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", activeProfileFileName), nil
}

// ActiveProfile reads the runtime active-profile state file.
//
// Return contract:
//   - (name, true, nil)  — file exists and contains a non-empty name.
//   - ("",   false, nil) — file does not exist OR exists but contains only
//     whitespace. Both cases mean "fall through to the nix default".
//   - ("",   false, err) — the path could not be resolved or the file
//     could not be read for a reason other than not-existing.
//
// The returned name has surrounding whitespace stripped (including a
// trailing newline). It is NOT validated against the profiles file; that
// is the caller's responsibility (spawn-time RequireSlot already covers
// the role-slot edge case).
func ActiveProfile() (string, bool, error) {
	path, err := ActiveProfilePath()
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("active-profile: read %s: %w", path, err)
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "", false, nil
	}
	return name, true, nil
}

// SetActiveProfile validates `name` against `pf` and writes it to the
// runtime active-profile state file.
//
// Validation:
//   - pf must be non-nil (we cannot validate without the profiles file).
//   - name must be present in pf.Profiles. On miss, the error lists every
//     valid profile name to make recovery a single copy-paste.
//   - The chosen profile must define every slot in RequiredSlots. Slot
//     validation runs through RequireSlot so the error shape is consistent
//     with spawn-time validation.
//
// Write semantics:
//   - The parent directory ($XDG_STATE_HOME/prism) is created with 0o755.
//   - The file is written atomically via a tempfile + rename so a crash
//     mid-write cannot corrupt the file. Permissions are 0o644.
//   - The file content is exactly `name + "\n"` (no preamble, single line,
//     trailing newline).
func SetActiveProfile(pf *ProfilesFile, name string) error {
	if pf == nil {
		return fmt.Errorf("active-profile: profiles file not loaded — cannot validate %q", name)
	}
	if _, ok := pf.Profiles[name]; !ok {
		return fmt.Errorf("active-profile: unknown profile %q — available: %s",
			name, strings.Join(AvailableProfileNames(pf), ", "))
	}
	for _, slot := range RequiredSlots {
		if err := RequireSlot(pf, name, slot); err != nil {
			return err
		}
	}

	path, err := ActiveProfilePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("active-profile: create %s: %w", dir, err)
	}

	// Atomic write: tempfile in the same directory + rename.
	tmp, err := os.CreateTemp(dir, activeProfileFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("active-profile: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.WriteString(name + "\n"); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("active-profile: write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("active-profile: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("active-profile: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("active-profile: rename %s → %s: %w", tmpPath, path, err)
	}
	return nil
}

// ResolveActiveProfile returns the effective active profile name applying
// the resolution order documented at the top of this file:
//
//   1. flagValue (when non-empty) — explicit `--profile` flag
//   2. Runtime state file
//   3. pf.Default — nix-configured default
//
// `source` describes which step produced the result and is intended for
// diagnostic logging only ("flag", "state-file", "nix-default", or "none").
//
// pf may be nil; in that case the nix default is unavailable and the
// function returns ("", "none", nil) when neither the flag nor the state
// file is set. State-file read errors that are NOT "file does not exist"
// are surfaced — a corrupt or permission-denied state file is a real
// problem the user needs to see, not a silent fallthrough.
func ResolveActiveProfile(pf *ProfilesFile, flagValue string) (name, source string, err error) {
	if flagValue != "" {
		return flagValue, "flag", nil
	}
	stateName, ok, err := ActiveProfile()
	if err != nil {
		return "", "", err
	}
	if ok {
		return stateName, "state-file", nil
	}
	if pf != nil && pf.Default != "" {
		return pf.Default, "nix-default", nil
	}
	return "", "none", nil
}
