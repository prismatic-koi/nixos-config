// Profile resolution for the event-write path.
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
// Two sources, in this order:
//
//  1. The session's OWN spawn. spawn_inputs.profile_name holds the tier the
//     session was spawned at. cmd/spawn.go writes the RESOLVED profile there,
//     not the raw flag, so the row is populated for a spawn that passed no
//     `--profile` too. The write path reads it by the event's instance_id.
//
//     A spawned session is therefore pinned to its spawn-time tier for its
//     whole life: a later `prism profile use` moves the machine-active
//     profile but not this session's attribution. That is the correct
//     reading. The session's slot and routing were resolved once, at spawn,
//     and do not change under a running agent.
//
//  2. The MACHINE-ACTIVE profile, for a session with no usable spawn row.
//     This is config.ResolveActiveProfile's precedence with an empty flag:
//     the state file, then the nix default.
//
// Source 1 is what makes a `--profile` override visible in telemetry: the
// daemon writing the event holds no spawn flag, but the row that recorded the
// flag is on disk and is keyed by the same instance_id the event carries.
// Source 2 is the coordinator case above, and only that case. Applying source
// 2 to a spawned session stamps the machine's tier rather than the tier the
// session ran at, which folds every override into the host's default bucket.
//
// When neither source resolves, the value is the explicit "unknown"
// placeholder, never an empty string.
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
// Cached, not read per event
// --------------------------
//
// The event-write path is hot, so neither source may add a read to every
// event.
//
// Source 2 is mtime-cached. The resolver stats the active-profile state file
// for its mtime and reads the CONTENT only when the mtime changes (or on the
// first resolution), exactly as account_name.go does for accounts/current.
// The nix default is read once from profiles.json and cached for the life of
// the process: a nixos rebuild regenerates profiles.json and restarts the
// prism daemon, so a fresh process re-reads it.
//
// Source 1 is cached per instance_id by sessionProfileCache below. A
// spawn_inputs row is written once and never updated, so a resolved tier is
// good for the life of the process.
package db

import (
	"database/sql"
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

// spawnProfileQuery reads the spawn-time profile of one session incarnation.
const spawnProfileQuery = `SELECT profile_name FROM spawn_inputs WHERE instance_id = ?`

// negativeSpawnProfileTTL bounds how long a "this session has no spawn-time
// profile" answer is trusted before the cache asks the database again.
//
// A miss is not always permanent. internal/session/spawn.go writes the
// spawn-intent event for a new instance_id just BEFORE it inserts that
// session's spawn_inputs row, so the first lookup a spawning process makes
// can miss a row that lands moments later. Without the TTL that process would
// stamp the machine profile on every further event it writes for the session.
const negativeSpawnProfileTTL = time.Minute

// maxSessionProfileEntries caps the number of cached sessions. A sidecar
// writes events for one session, but a process that spawns and monitors other
// sessions writes events for each of them, so the map needs a bound. At the
// cap the whole map is dropped rather than evicted one entry at a time: a
// rebuild costs one query per live session, which an LRU does not improve on
// at this size.
const maxSessionProfileEntries = 1024

// sessionProfileEntry is one cached answer. name is empty when the session has
// no usable spawn-time profile. at is when the answer was read, and bounds an
// empty answer by negativeSpawnProfileTTL.
type sessionProfileEntry struct {
	name string
	at   time.Time
}

// sessionProfileCache caches spawn_inputs.profile_name per instance_id so the
// event-write path issues no per-event query. It is safe for concurrent use
// and its zero value is usable.
type sessionProfileCache struct {
	mu      sync.Mutex
	entries map[string]sessionProfileEntry

	// queries counts database lookups. It backs the performance assertion that
	// N events for one session trigger one query, mirroring
	// ProfileResolver.reads. Guarded by mu.
	queries int
}

// name returns the spawn-time profile for instanceID, or "" when that session
// has no spawn row, its row records no profile, or the read fails. All of
// those mean the same thing to the caller: fall back to the machine-active
// profile.
func (c *sessionProfileCache) name(conn *sql.DB, instanceID string) string {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[instanceID]; ok && (e.name != "" || now.Sub(e.at) < negativeSpawnProfileTTL) {
		return e.name
	}

	c.queries++
	var resolved string
	var profile sql.NullString
	if err := conn.QueryRow(spawnProfileQuery, instanceID).Scan(&profile); err == nil {
		resolved = strings.TrimSpace(profile.String)
	}

	if c.entries == nil || len(c.entries) >= maxSessionProfileEntries {
		c.entries = make(map[string]sessionProfileEntry)
	}
	c.entries[instanceID] = sessionProfileEntry{name: resolved, at: now}
	return resolved
}

// spawnProfileName returns the spawn-time profile of the session that owns
// this event, or "" when the event carries no instance_id to look it up by.
func (d *DB) spawnProfileName(instanceID *string) string {
	if instanceID == nil || *instanceID == "" {
		return ""
	}
	return d.sessionProfiles.name(d.conn, *instanceID)
}

// resolveProfileName resolves the profile name for a write: the session's own
// spawn tier, then the machine-active profile. It is the single call site the
// event write paths share, so every row records the profile by the same rule.
func (d *DB) resolveProfileName(instanceID *string) string {
	if name := d.spawnProfileName(instanceID); name != "" {
		return name
	}
	return d.profileResolverFor().Name()
}
