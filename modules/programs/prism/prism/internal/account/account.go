// Package account implements the on-disk account store and atomic
// switching logic behind `prism account` (#2283).
//
// Background
// ----------
//
// Pi treats ~/.pi/agent/auth.json as a singleton — there's exactly one
// Anthropic OAuth blob at a time. Switching between Claude OAuth
// subscriptions used to require quitting prism, running `pi /login
// anthropic` to overwrite the file, then `prism restart`. This package
// stores each subscription's "anthropic" blob as a named file under
// ~/.config/prism/accounts/<name>.json and atomically merges the chosen
// blob into the live auth.json on switch. Pi's existing credential cache
// (with the mtime-invalidation tweak in #2283) then picks up the new
// tokens on its next request.
//
// On-disk layout
// --------------
//
//	~/.config/prism/accounts/
//	  <name>.json         the value of auth.json's "anthropic" key
//	                      (e.g. {"type":"oauth","access":"...","refresh":"...","expires":...})
//	  current             plain text: the active account name, one line
//
// Mode bits: directory 0o700, all files 0o600.
//
// Atomicity
// ---------
//
// Every write goes through a tempfile in the same directory followed by
// an os.Rename. Snapshot-before-swap means a SIGTERM after we've written
// the new auth.json tempfile but before the rename leaves the live file
// byte-identical to its pre-invocation contents.
//
// Security
// --------
//
// No function in this package ever returns or logs a token value. The
// error strings are intentionally token-free; callers can print them
// directly.
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// anthropicKey is the top-level key in auth.json that holds the Anthropic
// OAuth blob. `prism account` swaps only this key; sibling keys
// (github-copilot, …) are preserved byte-identical.
const anthropicKey = "anthropic"

// currentFileName is the basename of the pointer file inside the accounts
// directory. Its content is a single line: the active account name.
const currentFileName = "current"

// defaultAccountName is the name used by the first-run migration to
// snapshot the existing auth.json's anthropic blob.
const defaultAccountName = "default"

// Mode bits used throughout. Centralised so the security AC has a single
// place to enforce.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// Paths bundles the resolved on-disk paths this package operates on. All
// fields are absolute paths. AuthJSON honours $PI_AUTH_JSON for tests and
// for the documented manual escape-hatch (PI_AUTH_JSON=… pi).
type Paths struct {
	Dir      string // ~/.config/prism/accounts
	Current  string // ~/.config/prism/accounts/current
	AuthJSON string // ~/.pi/agent/auth.json (or $PI_AUTH_JSON)
}

// ResolvePaths computes the canonical paths from $HOME / $XDG_CONFIG_HOME
// / $PI_AUTH_JSON. Returns an error only when the home directory cannot
// be resolved at all — empty $HOME, $XDG_CONFIG_HOME unset, and
// $PI_AUTH_JSON unset all at once. Callers that intend to write must call
// Init before using these paths.
func ResolvePaths() (Paths, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("account: resolve home dir: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	dir := filepath.Join(configHome, "prism", "accounts")

	authJSON := os.Getenv("PI_AUTH_JSON")
	if authJSON == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("account: resolve home dir: %w", err)
		}
		authJSON = filepath.Join(home, ".pi", "agent", "auth.json")
	}

	return Paths{
		Dir:      dir,
		Current:  filepath.Join(dir, currentFileName),
		AuthJSON: authJSON,
	}, nil
}

// AccountPath returns the absolute path of a named account file. Does not
// verify the file exists.
func (p Paths) AccountPath(name string) string {
	return filepath.Join(p.Dir, name+".json")
}

// validName enforces that `name` is a non-empty single path component
// with no separator characters. Account names are used directly as
// filenames so we must reject anything that could traverse out of the
// accounts dir.
func validName(name string) error {
	if name == "" {
		return errors.New("account name must not be empty")
	}
	if strings.ContainsRune(name, os.PathSeparator) || strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("account name %q must not contain path separators", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("account name %q is reserved", name)
	}
	if name == currentFileName {
		return fmt.Errorf("account name %q collides with the pointer file", name)
	}
	return nil
}

// Init runs the first-run migration: if the accounts directory does not
// exist, create it (mode 0o700), snapshot the existing auth.json's
// anthropic blob (if present) to accounts/default.json (mode 0o600), and
// write accounts/current ← "default". Idempotent — a no-op when the
// accounts directory already exists. Callers must invoke this before any
// other operation in this package.
func Init(p Paths) error {
	if _, err := os.Stat(p.Dir); err == nil {
		// Directory already exists — first run already happened. Do not
		// touch any existing files.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("account: stat %s: %w", p.Dir, err)
	}

	if err := os.MkdirAll(p.Dir, dirMode); err != nil {
		return fmt.Errorf("account: create %s: %w", p.Dir, err)
	}
	// MkdirAll respects umask, which on most hosts is 022 → 0o755. Force
	// the AC-mandated 0o700 explicitly.
	if err := os.Chmod(p.Dir, dirMode); err != nil {
		return fmt.Errorf("account: chmod %s: %w", p.Dir, err)
	}

	// Snapshot the existing auth.json's anthropic blob (if any).
	blob, err := readAnthropicBlob(p.AuthJSON)
	if err != nil {
		// Malformed auth.json or a read error other than ENOENT.
		// Surface but do not abort: the migration should still leave the
		// user with a usable accounts/ directory.
		if !errors.Is(err, errNoAnthropicKey) && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("account: read %s: %w", p.AuthJSON, err)
		}
		// Either auth.json is absent or it has no anthropic key — leave
		// accounts/ empty. The user can run `prism account save` later.
		return nil
	}

	if err := atomicWriteFile(p.AccountPath(defaultAccountName), blob, fileMode); err != nil {
		return fmt.Errorf("account: write default snapshot: %w", err)
	}
	if err := atomicWriteFile(p.Current, []byte(defaultAccountName+"\n"), fileMode); err != nil {
		return fmt.Errorf("account: write current pointer: %w", err)
	}
	return nil
}

// List returns the account names available in p.Dir, sorted
// lexicographically. Only files matching *.json are considered — the
// `current` pointer file is ignored. Returns an empty slice if the
// directory is empty.
func List(p Paths) ([]string, error) {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return nil, fmt.Errorf("account: read %s: %w", p.Dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// Current returns the active account name and whether the pointer file
// exists with non-empty contents. Read errors other than ENOENT are
// surfaced. A missing or whitespace-only pointer file is reported as
// (`"", false, nil`) — callers render that as "none".
func Current(p Paths) (string, bool, error) {
	data, err := os.ReadFile(p.Current)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("account: read %s: %w", p.Current, err)
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "", false, nil
	}
	return name, true, nil
}

// Save snapshots the live auth.json's anthropic blob to
// accounts/<name>.json (mode 0o600). Returns an error if auth.json has
// no anthropic key; the destination file is not created in that case.
// Does not modify accounts/current.
func Save(p Paths, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	blob, err := readAnthropicBlob(p.AuthJSON)
	if err != nil {
		if errors.Is(err, errNoAnthropicKey) {
			return fmt.Errorf("account save %s: %s has no \"anthropic\" key — run `/login anthropic` in pi first", name, p.AuthJSON)
		}
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("account save %s: %s does not exist — run `/login anthropic` in pi first", name, p.AuthJSON)
		}
		return fmt.Errorf("account save %s: read %s: %w", name, p.AuthJSON, err)
	}
	if err := atomicWriteFile(p.AccountPath(name), blob, fileMode); err != nil {
		return fmt.Errorf("account save %s: %w", name, err)
	}
	return nil
}

// Use atomically switches the live auth.json's anthropic blob to the
// contents of accounts/<name>.json. The sequence is:
//
//  1. Validate accounts/<name>.json exists and parses as JSON. Abort
//     before touching auth.json if it does not.
//  2. Snapshot the live auth.json's anthropic blob to
//     accounts/<previous>.json, where <previous> is the contents of
//     accounts/current at the moment of invocation. Skip the snapshot
//     when there is no previous account, when <previous> is already the
//     target account (self-switch), or when the live auth.json has no
//     anthropic key.
//  3. Build the new auth.json contents: existing top-level keys
//     preserved byte-identical, the anthropic key replaced with the
//     target blob.
//  4. Tempfile + rename auth.json (mode 0o600). A SIGTERM between steps
//     3 and 4 leaves auth.json byte-identical to its pre-invocation
//     contents.
//  5. Tempfile + rename accounts/current ← <name>.
//
// If step 2 fails, abort before step 3 — we do not want to swap the
// live blob away while the snapshot is missing.
func Use(p Paths, name string) error {
	if err := validName(name); err != nil {
		return err
	}

	targetPath := p.AccountPath(name)
	target, err := os.ReadFile(targetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("account use %s: %s does not exist — run `prism account list` to see available accounts", name, targetPath)
		}
		return fmt.Errorf("account use %s: read %s: %w", name, targetPath, err)
	}
	// Validate that the file parses as JSON before doing anything else.
	// We deliberately do not require any particular shape — pi's writer
	// shape may evolve. The merge step rebuilds the file from this raw
	// blob; an invalid JSON would corrupt auth.json.
	var probe json.RawMessage
	if err := json.Unmarshal(target, &probe); err != nil {
		return fmt.Errorf("account use %s: %s is not valid JSON: %w", name, targetPath, err)
	}

	// Step 2: snapshot the live blob back to <previous>.
	prev, hasPrev, err := Current(p)
	if err != nil {
		return fmt.Errorf("account use %s: %w", name, err)
	}
	if hasPrev && prev != name {
		// Defence in depth: validate `prev` as if it were a user-supplied
		// name. Writing to accounts/current already requires user-level
		// access to the 0o700 accounts dir, so a malicious value here
		// presupposes a compromise, but skipping a snapshot is the safer
		// failure mode than letting a path-traversing name out the door.
		if err := validName(prev); err != nil {
			return fmt.Errorf("account use %s: malformed previous account name in %s: %w", name, p.Current, err)
		}
		liveBlob, blobErr := readAnthropicBlob(p.AuthJSON)
		if blobErr == nil {
			if err := atomicWriteFile(p.AccountPath(prev), liveBlob, fileMode); err != nil {
				return fmt.Errorf("account use %s: snapshot previous account %q: %w", name, prev, err)
			}
		} else if !errors.Is(blobErr, errNoAnthropicKey) && !errors.Is(blobErr, os.ErrNotExist) {
			// A real read/parse error against the live auth.json means we
			// cannot safely proceed: continuing would risk losing the
			// previous refresh token rotation. Abort before touching
			// anything.
			return fmt.Errorf("account use %s: read live %s: %w", name, p.AuthJSON, blobErr)
		}
		// errNoAnthropicKey or ENOENT: nothing to snapshot, proceed.
	}

	// Step 3: build merged auth.json content.
	merged, err := mergeAnthropicBlob(p.AuthJSON, target)
	if err != nil {
		return fmt.Errorf("account use %s: merge %s: %w", name, p.AuthJSON, err)
	}

	// Step 4: atomic rename of auth.json.
	if err := atomicWriteFile(p.AuthJSON, merged, fileMode); err != nil {
		return fmt.Errorf("account use %s: write %s: %w", name, p.AuthJSON, err)
	}

	// Step 5: update the pointer.
	if err := atomicWriteFile(p.Current, []byte(name+"\n"), fileMode); err != nil {
		return fmt.Errorf("account use %s: write %s: %w", name, p.Current, err)
	}
	return nil
}

// Remove deletes accounts/<name>.json. Refuses with a clear error if
// <name> is the currently-active account (per accounts/current); the
// file is not deleted in that case. Refuses with a clear error if the
// file does not exist.
func Remove(p Paths, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	cur, hasCur, err := Current(p)
	if err != nil {
		return fmt.Errorf("account rm %s: %w", name, err)
	}
	if hasCur && cur == name {
		return fmt.Errorf("account rm %s: refusing to delete the active account — switch with `prism account use <other>` first", name)
	}
	path := p.AccountPath(name)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("account rm %s: %s does not exist", name, path)
		}
		return fmt.Errorf("account rm %s: %w", name, err)
	}
	return nil
}

// errNoAnthropicKey signals that auth.json exists and parses as JSON but
// has no top-level "anthropic" key. Distinguished from a read/parse
// error so callers can take a different action (Init: skip snapshot;
// Save: error to user; Use: skip the previous-account snapshot step).
var errNoAnthropicKey = errors.New("auth.json has no anthropic key")

// readAnthropicBlob returns the raw JSON bytes of auth.json's
// "anthropic" key. The returned slice is the value (an object), not the
// `{"anthropic":…}` wrapper. Returns errNoAnthropicKey when the key is
// absent, os.ErrNotExist when the file is absent, and a wrapped parse
// error when the file is malformed.
func readAnthropicBlob(authJSONPath string) ([]byte, error) {
	data, err := os.ReadFile(authJSONPath)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errNoAnthropicKey
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse %s: %w", authJSONPath, err)
	}
	raw, ok := top[anthropicKey]
	if !ok {
		return nil, errNoAnthropicKey
	}
	// Re-serialise with indent so the snapshot file reads cleanly. The
	// value itself round-trips byte-equivalently for canonical inputs;
	// we accept that we may normalise whitespace.
	var pretty json.RawMessage
	if err := json.Unmarshal(raw, &pretty); err != nil {
		return nil, fmt.Errorf("parse %s anthropic blob: %w", authJSONPath, err)
	}
	out, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal %s anthropic blob: %w", authJSONPath, err)
	}
	out = append(out, '\n')
	return out, nil
}

// mergeAnthropicBlob reads authJSONPath, replaces its top-level
// "anthropic" key with `blob`, and returns the serialised bytes. Other
// top-level keys are preserved with their JSON bytes intact
// (json.RawMessage round-trip). When authJSONPath does not exist or is
// empty the result is a single-key object {"anthropic": blob}.
func mergeAnthropicBlob(authJSONPath string, blob []byte) ([]byte, error) {
	top := map[string]json.RawMessage{}
	data, err := os.ReadFile(authJSONPath)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, fmt.Errorf("parse %s: %w", authJSONPath, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", authJSONPath, err)
	}
	// json.RawMessage requires a self-contained valid JSON value.
	var rm json.RawMessage = append([]byte(nil), blob...)
	if err := json.Unmarshal(rm, new(json.RawMessage)); err != nil {
		return nil, fmt.Errorf("blob is not valid JSON: %w", err)
	}
	top[anthropicKey] = rm

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal merged auth.json: %w", err)
	}
	out = append(out, '\n')
	return out, nil
}

// atomicWriteFile writes `data` to `path` atomically via a tempfile in
// the same directory followed by os.Rename. The tempfile is created
// with the requested mode bits (an explicit Chmod after creation in
// case the OS umask masked them). Best-effort cleanup on failure.
//
// Crash safety: a SIGTERM between OpenFile and Rename leaves `path`
// untouched — the tempfile is orphaned but the target is byte-identical
// to its pre-invocation contents. This is the property the SIGTERM AC
// requires.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	// O_EXCL guards against an attacker pre-creating the tempfile.
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
