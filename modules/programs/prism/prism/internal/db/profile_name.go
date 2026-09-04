// Active-profile resolution for the event-write path.
//
// Why this exists
// ---------------
//
// A coordinator session is never spawned (nixos-config@main, home-ops@main,
// obsidian start directly). spawn_inputs records SPAWNS only, so a profile
// read that joins agent_events to spawn_inputs misses every coordinator and
// folds its spend to "default". Coordinators are long-lived and high-volume,
// so they dominate that bucket. "default" is not a tier — it is the fold the
// exporter applies when the profile cannot be resolved.
//
// The resolved profile of a never-spawned session exists only at runtime, so
// there is nothing joinable to fix. This file records the resolved profile
// NAME on every agent_events row AT WRITE TIME, exactly as account_name.go
// records the account name. The exporter then reads the plain column and drops
// the join (exporter/sql.go, CostEventsTailSQL).
//
// Write time, not scrape time
// ---------------------------
//
// These counters accumulate across profile switches. A scrape-time resolution
// attributes earlier spend to whichever profile is active at the scrape. The
// profile must be pinned to the row the moment it is written. This mirrors
// account_name.go's reasoning.
//
// Precedence
// ----------
//
// A spawned session records the profile it was spawned at. SpawnSession
// resolves `--profile` (or the active profile at spawn time) and writes the
// name to spawn_inputs.profile_name; review agents and investigators inherit
// their parent's value on the same row. That row is the session's own tier,
// so it takes precedence: two sessions on one host under different `--profile`
// flags record two different names, whatever the machine-active profile is.
//
// The machine-active resolution (state file, then nix default) applies only
// when the session has no usable spawn_inputs row: a never-spawned
// coordinator, or a row whose profile_name is NULL or empty. When neither
// source resolves, the value is the explicit "unknown" placeholder, never an
// empty string.
//
// The `--profile` FLAG itself is not consulted here: the process writing the
// event holds no spawn flag. The flag reaches this path only through the
// spawn_inputs row.
//
// The db -> config import decision
// --------------------------------
//
// internal/config is a LEAF package: `go list -deps ./internal/config` names
// no other internal package, and internal/db does not appear in its transitive
// imports. So db importing config creates NO cycle. This file imports config
// directly to reuse the exact profiles.json path (config.LoadProfiles) and
// state-file path (config.ActiveProfilePath) that config.ResolveActiveProfile
// uses.
//
// mtime-cached, not stat-and-read per event
// ------------------------------------------
//
// The event-write path is hot. This resolver stats the active-profile state
// file for its mtime and reads the CONTENT only when the mtime changes (or on
// the first resolution), exactly as account_name.go does for accounts/current.
// The nix default is read once from profiles.json and cached for the life of
// the process: a nixos rebuild regenerates profiles.json and restarts the
// prism daemon, so a fresh process re-reads it.
//
// The spawn_inputs lookup is cached per instance_id on the DB handle, so a
// session's events cost one query, not one query each. A spawn_inputs row is
// written once (INSERT OR IGNORE) and never updated, so a row that resolved a
// name is cached for the life of the handle. A miss is cached for
// spawnProfileMissTTL and then re-queried: a coordinator costs one query per
// TTL, and a session whose events reach the writer before its spawn_inputs
// row is committed picks up the row on the next expiry.
package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

// unknownProfile is the profile label recorded when no profile can be
// resolved (no state file and no nix default), and the placeholder the
// exporter folds a NULL profile_name to. It matches the "unknown" placeholder
// used for the repo label and usage.UnknownAccount for accounts.
const unknownProfile = "unknown"

// ProfileResolver resolves the active prism profile name and caches the
// result, re-deriving it only when the active-profile state file's mtime
// changes. It is safe for concurrent use.
//
// A zero value is not usable; construct one with NewProfileResolver.
type ProfileResolver struct {
	mu sync.Mutex

	// statePath yields the active-profile state file path. It is a field so a
	// test can bind the resolver to a temp file without mutating the process
	// environment. Production uses config.ActiveProfilePath.
	statePath func() (string, error)

	// loadDefault yields the nix-configured default profile name. It is a
	// field so a test can inject a default without writing a real
	// profiles.json. Production uses config.LoadProfiles and reads pf.Default.
	loadDefault func() string

	defaultInit bool
	defaultName string

	haveCache  bool
	cacheMtime time.Time
	cacheName  string

	// reads counts CONTENT reads of the state file (not stats). It backs the
	// performance assertion that N events after one switch trigger at most two
	// reads. Guarded by mu.
	reads int
}

// nixDefaultProfile loads profiles.json and returns pf.Default, or "" when the
// file is absent, unreadable, or malformed. A missing default is a non-error:
// the resolver then falls through to the "unknown" placeholder.
func nixDefaultProfile() string {
	pf, err := config.LoadProfiles()
	if err != nil || pf == nil {
		return ""
	}
	return pf.Default
}

// NewProfileResolver builds a resolver that reads the state-file path and the
// nix default from the process environment.
func NewProfileResolver() *ProfileResolver {
	return &ProfileResolver{
		statePath:   config.ActiveProfilePath,
		loadDefault: nixDefaultProfile,
	}
}

// newProfileResolverForTest builds a resolver bound to a fixed state-file path
// and a fixed nix default. Test-only.
func newProfileResolverForTest(statePath, nixDefault string) *ProfileResolver {
	return &ProfileResolver{
		statePath:   func() (string, error) { return statePath, nil },
		loadDefault: func() string { return nixDefault },
	}
}

// Name returns the active profile name using config.ResolveActiveProfile's
// precedence with an empty flag: the state file if it holds a non-empty name,
// otherwise the nix default, otherwise unknownProfile. It never fails: an
// unresolvable profile is recorded as "unknown" rather than failing or
// dropping the event write. The state-file read result is cached and
// re-derived only when the file's mtime changes.
func (r *ProfileResolver) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.defaultInit {
		r.defaultName = r.loadDefault()
		r.defaultInit = true
	}

	path, err := r.statePath()
	if err != nil {
		// No resolvable state path (no home). The flag does not apply here, so
		// fall straight to the nix default.
		r.haveCache = false
		return r.foldDefault()
	}

	fi, err := os.Stat(path)
	if err != nil {
		// State file absent (no `prism profile use` yet) or unreadable: the
		// active profile is the nix default. Drop any stale cache so a later
		// `prism profile use` — which creates the file — is picked up on the
		// next event.
		r.haveCache = false
		return r.foldDefault()
	}

	mtime := fi.ModTime()
	if r.haveCache && mtime.Equal(r.cacheMtime) {
		return r.cacheName
	}

	// Cache cold or the state file changed since we last read it: read the
	// content once and re-derive the name.
	r.reads++
	data, err := os.ReadFile(path)
	name := ""
	if err == nil {
		name = strings.TrimSpace(string(data))
	}
	if name == "" {
		// Unreadable, empty, or whitespace-only: config.ActiveProfile's
		// empty-means-default contract says fall through to the nix default.
		r.cacheName = r.foldDefault()
	} else {
		r.cacheName = name
	}
	r.cacheMtime = mtime
	r.haveCache = true
	return r.cacheName
}

// foldDefault returns the nix default, or unknownProfile when no default is
// configured. It is the single point where "unknown" is assigned on the write
// path, matching the exporter's NULL fold.
func (r *ProfileResolver) foldDefault() string {
	if r.defaultName != "" {
		return r.defaultName
	}
	return unknownProfile
}

var (
	processProfileResolverOnce sync.Once
	processProfileResolver     *ProfileResolver
)

// ProcessProfileResolver returns the resolver built from this process's
// environment. It is built once, on first use, and shared by every DB handle
// that does not carry its own — so the mtime cache and the loaded nix default
// are process-wide.
func ProcessProfileResolver() *ProfileResolver {
	processProfileResolverOnce.Do(func() {
		processProfileResolver = NewProfileResolver()
	})
	return processProfileResolver
}

// SetProfileResolver overrides the profile resolver this DB handle uses at
// write time. Production code does not call this — Open leaves the handle on
// ProcessProfileResolver. It exists so a test can bind a resolver to a temp
// state file. Passing nil restores the process default.
func (d *DB) SetProfileResolver(r *ProfileResolver) {
	d.profileResolverMu.Lock()
	defer d.profileResolverMu.Unlock()
	d.profileResolver = r
}

// profileResolverFor returns the resolver this handle must use.
func (d *DB) profileResolverFor() *ProfileResolver {
	d.profileResolverMu.RLock()
	r := d.profileResolver
	d.profileResolverMu.RUnlock()
	if r != nil {
		return r
	}
	return ProcessProfileResolver()
}

// spawnProfileMissTTL bounds how long a spawn_inputs miss (no row, or a row
// with no profile_name) is trusted before the next event re-queries it.
const spawnProfileMissTTL = time.Minute

// spawnProfileEntry is one cached spawn_inputs lookup. name is "" on a miss.
// A miss expires at expiresAt; a hit never expires because the row is
// immutable.
type spawnProfileEntry struct {
	name      string
	expiresAt time.Time
}

// spawnProfileFor returns spawn_inputs.profile_name for instanceID, or ""
// when the session has no row, the row has no profile, or the lookup fails.
// The result is cached per the rules in the file header.
func (d *DB) spawnProfileFor(instanceID string) string {
	now := time.Now()

	d.spawnProfileMu.Lock()
	defer d.spawnProfileMu.Unlock()

	if ent, ok := d.spawnProfiles[instanceID]; ok {
		if ent.name != "" || now.Before(ent.expiresAt) {
			return ent.name
		}
	}

	d.spawnProfileQueries++
	var name sql.NullString
	err := d.conn.QueryRow(`SELECT profile_name FROM spawn_inputs WHERE instance_id = ?`, instanceID).Scan(&name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// A failed lookup must not drop the event write; the machine-active
		// fallback applies until the next TTL expiry.
		name = sql.NullString{}
	}

	ent := spawnProfileEntry{name: strings.TrimSpace(name.String)}
	if ent.name == "" {
		ent.expiresAt = now.Add(spawnProfileMissTTL)
	}
	if d.spawnProfiles == nil {
		d.spawnProfiles = make(map[string]spawnProfileEntry)
	}
	d.spawnProfiles[instanceID] = ent
	return ent.name
}

// resolveProfileName resolves the profile name recorded on a write for the
// session identified by instanceID (nil when the event carries none). It is
// the single call site the event write paths share, so every row records the
// profile by the same rule: the session's own spawn_inputs profile first, then
// the machine-active resolution.
func (d *DB) resolveProfileName(instanceID *string) string {
	if instanceID != nil && *instanceID != "" {
		if name := d.spawnProfileFor(*instanceID); name != "" {
			return name
		}
	}
	return d.profileResolverFor().Name()
}
