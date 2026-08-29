// Active-account resolution for the event-write path.
//
// Why this exists
// ---------------
//
// The active account lives only on disk, in ~/.config/prism/accounts/current.
// To attribute spend to a subscription, this path records the account NAME on
// every agent_events row (and on the spawn_inputs row) AT WRITE TIME.
//
// Write time, not scrape time — the trap
// ---------------------------------------
//
// The spend counters accumulate across account switches. A scrape-time
// resolution attributes earlier spend to whichever account is active at the
// scrape, which is the wrong account. The account must be pinned to the row
// the moment the row is written, which is what DB.resolveAccountName does from
// inside WriteEvent / InsertSpawnInputs. The operator switches accounts when a
// rate-limit window exhausts, so mid-session switches are common. A per-event
// capture is the only shape that stays accurate across them.
//
// mtime-cached, not stat-and-read per event
// ------------------------------------------
//
// The event-write path is hot. A read of accounts/current on every event adds
// a file read to every write. Instead the resolver stats the pointer file for
// its mtime and reads the CONTENT only when the mtime changes (or on the first
// resolution). This mirrors the mtime-invalidation approach pi's credential
// cache uses. A single account switch therefore costs at most one extra
// content read, no matter how many events follow it.
//
// Security
// --------
//
// This path records the account NAME only. It never reads, logs, or stores
// the contents of any accounts/*.json file — those hold OAuth tokens. It stats
// and reads only the plain-text `current` pointer, exactly as
// account.Current does.
package db

import (
	"os"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/account"
	"github.com/prismatic-koi/prism/internal/usage"
)

// AccountResolver resolves the active prism account name and caches the
// result, invalidating the cache only when the accounts/current pointer file's
// mtime changes. It is safe for concurrent use.
//
// A zero value is not usable; construct one with NewAccountResolver.
type AccountResolver struct {
	mu sync.Mutex

	// resolvePaths yields the on-disk account paths. It is a field so a test
	// can bind the resolver to a temp directory without mutating the process
	// environment. Production uses account.ResolvePaths.
	resolvePaths func() (account.Paths, error)

	pathsInit bool
	paths     account.Paths
	pathsErr  error

	haveCache  bool
	cacheMtime time.Time
	cacheName  string

	// reads counts CONTENT reads of the pointer file (not stats). It backs
	// the performance assertion that N events after one switch trigger at
	// most two reads. Guarded by mu.
	reads int
}

// NewAccountResolver builds a resolver that reads the account paths from the
// process environment ($HOME / $XDG_CONFIG_HOME) via account.ResolvePaths.
func NewAccountResolver() *AccountResolver {
	return &AccountResolver{resolvePaths: account.ResolvePaths}
}

// newAccountResolverForPaths builds a resolver bound to fixed paths. Test-only:
// it lets a test point the resolver at a t.TempDir()-rooted accounts directory.
func newAccountResolverForPaths(p account.Paths) *AccountResolver {
	return &AccountResolver{
		resolvePaths: func() (account.Paths, error) { return p, nil },
	}
}

// Name returns the active account name, or usage.UnknownAccount ("unknown")
// when the account store does not exist, the pointer file is absent, empty, or
// whitespace-only, or the recorded name does not pass usage.SanitizeAccountName.
//
// It never fails: an unresolvable account is recorded as "unknown" rather than
// failing or dropping the event write. The result is cached and re-derived
// only when the pointer file's mtime changes.
func (r *AccountResolver) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.pathsInit {
		r.paths, r.pathsErr = r.resolvePaths()
		r.pathsInit = true
	}
	if r.pathsErr != nil {
		// Home directory is unresolvable; there is no store to read. This is
		// permanent for the process, so leave the cache cold and answer
		// "unknown" every time (the stat below would fail anyway).
		return usage.UnknownAccount
	}

	fi, err := os.Stat(r.paths.Current)
	if err != nil {
		// No pointer file (no accounts directory, or no active account). Stat
		// already told us the file is absent, so no content read is needed.
		// Drop any stale cache so that a later `prism account use` — which
		// creates the file — is picked up on the next event.
		r.haveCache = false
		return usage.UnknownAccount
	}

	mtime := fi.ModTime()
	if r.haveCache && mtime.Equal(r.cacheMtime) {
		return r.cacheName
	}

	// Cache cold or the pointer changed since we last read it: read the
	// content once and re-derive the name.
	r.reads++
	name, ok, err := account.Current(r.paths)
	switch {
	case err != nil || !ok:
		// Read failed, or the pointer is empty / whitespace-only.
		r.cacheName = usage.UnknownAccount
	default:
		r.cacheName = usage.SanitizeAccountName(name)
	}
	r.cacheMtime = mtime
	r.haveCache = true
	return r.cacheName
}

var (
	processAccountResolverOnce sync.Once
	processAccountResolver     *AccountResolver
)

// ProcessAccountResolver returns the resolver built from this process's
// environment. It is built once, on first use, and shared by every DB handle
// that does not carry its own — so the mtime cache is process-wide and one
// switch costs one content read across all event writes in the process.
func ProcessAccountResolver() *AccountResolver {
	processAccountResolverOnce.Do(func() {
		processAccountResolver = NewAccountResolver()
	})
	return processAccountResolver
}

// SetAccountResolver overrides the account resolver this DB handle uses at
// write time. Production code does not call this — Open leaves the handle on
// ProcessAccountResolver. It exists so a test can bind a resolver to a temp
// accounts directory. Passing nil restores the process default.
func (d *DB) SetAccountResolver(r *AccountResolver) {
	d.accountResolverMu.Lock()
	defer d.accountResolverMu.Unlock()
	d.accountResolver = r
}

// accountResolverFor returns the resolver this handle must use.
func (d *DB) accountResolverFor() *AccountResolver {
	d.accountResolverMu.RLock()
	r := d.accountResolver
	d.accountResolverMu.RUnlock()
	if r != nil {
		return r
	}
	return ProcessAccountResolver()
}

// resolveAccountName resolves the active account name for a write. It is the
// single call site the event and spawn-input write paths share, so both record
// the account by the same rule.
func (d *DB) resolveAccountName() string {
	return d.accountResolverFor().Name()
}
