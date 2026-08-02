// Package usage persists Claude subscription rate-limit snapshots.
//
// Background
// ----------
//
// Anthropic returns a set of `anthropic-ratelimit-unified-*` headers on every
// successful `/v1/messages` response on the Claude Code OAuth path. The
// vendored `anthropic-oauth` pi extension captures those headers and POSTs
// them to the sidecar host-API endpoint `/usage/snapshot`. This package owns
// the on-disk format and the write path (issue #2538, parent #2537).
//
// On-disk layout
// --------------
//
//	~/.local/state/prism/usage/
//	  <account>.json      one snapshot per account, mode 0600
//	  current.json        a byte-identical copy of the active account's
//	                      snapshot, mode 0600
//
// The directory is created with mode 0700. `current.json` lets a sandboxed
// reader learn both the active account name and its snapshot in one read —
// `~/.config/prism/accounts/` is deliberately not visible inside an agent
// sandbox (see `internal/container/mounts.go`).
//
// The state directory, not the accounts directory, is the home for these
// files: the data is a derived cache, not a credential.
//
// Sandbox visibility
// ------------------
//
// This directory — the LEAF, not any parent — is bound into agent sandboxes
// READ-ONLY (issue #2572): `StandardSandboxMounts` emits an `--ro-bind` for
// bwrap and `generateProfile` emits a read-only `(subpath ...)` grant for
// sandbox-exec. Read-only is the correct level. Every writer goes through
// the sidecar host-API endpoint `POST /usage/snapshot` (issue #2538), so
// nothing inside a sandbox needs write access, and a compromised session
// cannot forge usage figures.
//
// Absent fields
// -------------
//
// Every optional field is a pointer or has `omitempty`. A header that the
// response did not carry is OMITTED from the JSON rather than written as a
// zero value, so a reader can tell "not present" from "zero". The downstream
// readers (issues #2539, #2540, #2541) depend on that distinction.
//
// Security
// --------
//
// No function in this package reads, accepts, or writes a token value. The
// Snapshot type is a closed struct whose fields all derive from the
// allowlisted `anthropic-ratelimit-unified-*` headers documented in #2537.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/account"
)

// UnknownAccount is the account name used when the account store cannot be
// resolved — no accounts directory, an empty `current` pointer, or a
// malformed name in it. Persisting under this name is deliberately preferred
// over failing the write: a snapshot with an unknown owner is still useful,
// and the capture path must never depend on the account store existing.
const UnknownAccount = "unknown"

// CurrentFileName is the basename of the active-account copy. It is a
// reserved name: SanitizeAccountName refuses an account literally called
// "current" so that <account>.json can never collide with it.
const CurrentFileName = "current.json"

// Mode bits. Centralised so the security AC has a single place to enforce.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// maxAccountNameLen bounds the account name so a hostile or corrupt
// `accounts/current` cannot produce an ENAMETOOLONG write path. Account names
// are short by construction ("work", "personal"); 64 is generous.
const maxAccountNameLen = 64

// Window is one rate-limit window (the 5-hour or the 7-day window).
//
// Utilization is the RAW fraction from the header: 0.94 stays 0.94. Callers
// that render a percentage multiply by 100 themselves.
//
// Reset is unix seconds.
type Window struct {
	Status             string   `json:"status,omitempty"`
	Utilization        *float64 `json:"utilization,omitempty"`
	Reset              *int64   `json:"reset,omitempty"`
	SurpassedThreshold *float64 `json:"surpassed_threshold,omitempty"`
}

// Windows groups the per-window snapshots. `five_hour` maps to the
// `anthropic-ratelimit-unified-5h-*` headers, `seven_day` to the `-7d-*` set.
type Windows struct {
	FiveHour *Window `json:"five_hour,omitempty"`
	SevenDay *Window `json:"seven_day,omitempty"`
}

// Fallback mirrors `anthropic-ratelimit-unified-fallback` and
// `-fallback-percentage`. Percentage is a raw fraction.
type Fallback struct {
	Status     string   `json:"status,omitempty"`
	Percentage *float64 `json:"percentage,omitempty"`
}

// Overage mirrors `anthropic-ratelimit-unified-overage-status` and
// `-overage-disabled-reason`.
type Overage struct {
	Status         string `json:"status,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// Snapshot is the persisted per-account rate-limit snapshot. The JSON shape is
// the contract documented in issue #2537 and read by issues #2539, #2540, and
// #2541 — do not rename or retype a field without updating all three.
//
// CapturedAt and Account are set host-side by the sidecar at write time. They
// are the only two fields not derived from a response header, and neither is
// accepted from the caller: the endpoint's request schema has no field for
// either, so a caller cannot spoof the account name.
type Snapshot struct {
	CapturedAt          string    `json:"captured_at"`
	Account             string    `json:"account"`
	UnifiedStatus       string    `json:"unified_status,omitempty"`
	RepresentativeClaim string    `json:"representative_claim,omitempty"`
	UnifiedReset        *int64    `json:"unified_reset,omitempty"`
	Windows             *Windows  `json:"windows,omitempty"`
	Fallback            *Fallback `json:"fallback,omitempty"`
	Overage             *Overage  `json:"overage,omitempty"`
}

// DirForHome returns the usage directory for a caller that has ALREADY
// resolved the host home directory itself, honouring $XDG_STATE_HOME first
// and falling back to <home>/.local/state/prism/usage. It returns "" when
// $XDG_STATE_HOME is empty and home is empty — callers must treat that as
// "skip".
//
// This is the single source of truth for the resolution order. Three readers
// depend on it agreeing byte for byte:
//
//   - DefaultDir below (the prism CLI and the sidecar write path);
//   - usageSnapshotPath() in pi/extensions/prism.ts (the bottom-bar reader,
//     which does $XDG_STATE_HOME-else-os.homedir()/.local/state);
//   - StandardSandboxMounts / generateProfile in internal/container, which
//     must grant the sandbox exactly the directory those two agree on
//     (issue #2572).
//
// The container callers cannot use DefaultDir: they resolve the host home
// once at the top of the mount walk and pass it down, and their unit tests
// drive that value from a fixture rather than from os.UserHomeDir(). Taking
// home as a parameter keeps the XDG lookup in one place regardless.
func DirForHome(home string) string {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "prism", "usage")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "prism", "usage")
}

// DefaultDir returns the canonical usage directory for the current user,
// honouring $XDG_STATE_HOME first and falling back to
// $HOME/.local/state/prism/usage. Returns ("", error) when neither is
// discoverable.
//
// Reading $XDG_STATE_HOME first matters for the nix-build sandbox where
// HOME=/homeless-shelter is intentionally unwritable; tests override
// XDG_STATE_HOME to a writable t.TempDir() to avoid that path entirely.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if dir := DirForHome(home); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("usage: cannot resolve state dir — neither XDG_STATE_HOME nor HOME is set")
}

// Store is the on-disk snapshot directory. The zero value is invalid; use
// NewStore.
type Store struct {
	// Dir is the absolute path of the usage directory. Tests pass a
	// t.TempDir(); production callers resolve it from DefaultDir.
	Dir string
}

// NewStore constructs a Store rooted at dir. The directory is not created
// until the first Write.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

// SanitizeAccountName returns a name that is safe to use as a single path
// component inside the usage directory, or UnknownAccount when the input
// cannot be used.
//
// Defence in depth. `internal/account` already rejects these shapes when a
// user creates or switches an account, so a bad value here presupposes a
// hand-edited or corrupt `accounts/current`. Falling back to UnknownAccount
// is the safer failure mode than letting a path-traversing name reach a file
// write.
//
// Rejected: the empty string, any name with a path separator, "." and "..",
// a leading ".", the reserved name "current" (it would collide with
// current.json), any name with a control character, and any name longer than
// 64 bytes.
func SanitizeAccountName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxAccountNameLen {
		return UnknownAccount
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') ||
		strings.ContainsRune(name, os.PathSeparator) {
		return UnknownAccount
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return UnknownAccount
	}
	// "current" would produce current.json, colliding with the active-account
	// copy this package writes.
	if name == "current" {
		return UnknownAccount
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return UnknownAccount
		}
	}
	return name
}

// CurrentAccountName resolves the active account name from
// ~/.config/prism/accounts/current, returning UnknownAccount when the store
// does not exist, the pointer file is absent or empty, the path cannot be
// resolved, or the recorded name does not pass SanitizeAccountName.
//
// This is a READ-ONLY resolution: it never creates the accounts directory and
// never runs the first-run migration, so it is safe to call from the sidecar
// on a host that has never used `prism account`.
//
// The resolution happens at write time, not at session-spawn time, so a
// snapshot captured after the user switches accounts is attributed to the new
// account (issue #2537, reason 2).
func CurrentAccountName() string {
	paths, err := account.ResolvePaths()
	if err != nil {
		return UnknownAccount
	}
	name, ok, err := account.Current(paths)
	if err != nil || !ok {
		return UnknownAccount
	}
	return SanitizeAccountName(name)
}

// Write persists snap to <Dir>/<account>.json and <Dir>/current.json.
//
// snap.Account is passed through SanitizeAccountName and the sanitised value
// is written into the persisted object, so the `account` field and the
// filename always agree.
//
// Both files are written atomically — a tempfile in the same directory
// followed by a rename — so a concurrent reader never observes a partial
// object, and a crash mid-write leaves the previous snapshot intact. The
// per-account file is written first: if the process dies between the two
// renames, current.json is stale rather than pointing at an account whose own
// file was never written.
//
// The directory is created with mode 0700 and the files with mode 0600.
func (s *Store) Write(snap Snapshot) error {
	if s.Dir == "" {
		return fmt.Errorf("usage: Store.Dir is empty")
	}

	snap.Account = SanitizeAccountName(snap.Account)

	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return fmt.Errorf("usage: create %s: %w", s.Dir, err)
	}
	// MkdirAll does nothing at all when the directory already exists, so a
	// usage/ created earlier with a looser mode would keep it. Chmod
	// unconditionally so the 0700 requirement holds on both paths.
	// TestWrite_TightensPreExistingLooseDirMode covers the already-exists
	// case.
	if err := os.Chmod(s.Dir, dirMode); err != nil {
		return fmt.Errorf("usage: chmod %s: %w", s.Dir, err)
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("usage: marshal snapshot: %w", err)
	}
	data = append(data, '\n')

	accountPath := filepath.Join(s.Dir, snap.Account+".json")
	if err := atomicWriteFile(accountPath, data, fileMode); err != nil {
		return fmt.Errorf("usage: write %s: %w", accountPath, err)
	}
	currentPath := filepath.Join(s.Dir, CurrentFileName)
	if err := atomicWriteFile(currentPath, data, fileMode); err != nil {
		return fmt.Errorf("usage: write %s: %w", currentPath, err)
	}
	return nil
}

// FormatCapturedAt formats t as the `captured_at` field value: RFC3339 in UTC
// with second resolution (e.g. "2026-08-02T23:43:28Z").
func FormatCapturedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// atomicWriteFile writes data to path via a tempfile in the same directory
// followed by a rename. Mirrors internal/account's helper of the same name —
// the two are deliberately separate because neither package should depend on
// the other for a five-syscall primitive.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	// CreateTemp uses O_EXCL, which guards against an attacker pre-creating
	// the tempfile.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	// CreateTemp already opens at 0600, so this is belt and braces: it states
	// the required mode at the call site rather than inheriting it from a
	// stdlib detail that a future Go release could change.
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s → %s: %w", tmpPath, path, err)
	}
	return nil
}
