// Package container manages the podman container lifecycle for prism sidecar.
// This file defines IsolationRegistry — a name→constructor map populated at
// init() time by each isolator file — and the Resolve helper that centralises
// the back-compat resolution logic that is currently scattered across multiple
// call sites.
//
// Phase 1 of A.1 §7: the registry exists and is consultable, but no existing
// caller has been migrated to read from it yet. That migration is the work of
// A.1 Phases 2–4.
package container

import (
	"fmt"
	"sort"
	"sync"

	"github.com/prismatic-koi/prism/internal/config"
)

// ConstructorOpts carries the per-session parameters passed to an isolator
// constructor. Name is the stable session identifier (the container name for
// podman, or the raw session name for the other modes).
type ConstructorOpts struct {
	Name string
}

// Constructor is a factory function that returns an Isolator for a given set
// of construction options. Each isolator file registers one at init() time.
type Constructor func(opts ConstructorOpts) Isolator

// Registration holds everything the registry stores for one isolation mode.
type Registration struct {
	// Name is the canonical mode name (the registry key).
	Name config.IsolationMode

	// Constructor is the factory function that creates an Isolator instance.
	Constructor Constructor

	// Capabilities is a pre-computed snapshot of the mode's feature flags,
	// used for capability queries that do not need a live Isolator instance.
	Capabilities Capabilities
}

// IsolationRegistry maps isolation mode names to their constructors.
// Isolators register at init() time; the registry is consulted at spawn time
// and by capability queries. The singleton is package-level; callers use the
// package-level Register, MustRegister, For, Names, and Resolve functions
// rather than constructing an IsolationRegistry directly.
type IsolationRegistry struct {
	mu            sync.RWMutex
	registrations map[config.IsolationMode]Registration
}

// globalRegistry is the package-level singleton populated by init()
// registrations in each isolator file.
var globalRegistry = &IsolationRegistry{
	registrations: make(map[config.IsolationMode]Registration),
}

// Register adds a constructor for name to the global registry. It returns an
// error if name is already registered. The Capabilities snapshot is obtained
// by calling the constructor with a zero-value ConstructorOpts and invoking
// Capabilities() on the resulting Isolator.
func Register(name config.IsolationMode, c Constructor) error {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()

	if _, exists := globalRegistry.registrations[name]; exists {
		return fmt.Errorf("container: isolation mode %q is already registered", name)
	}

	// Compute a capabilities snapshot from a throw-away instance so callers
	// can query capabilities without constructing a live isolator.
	probe := c(ConstructorOpts{})
	globalRegistry.registrations[name] = Registration{
		Name:         name,
		Constructor:  c,
		Capabilities: probe.Capabilities(),
	}
	return nil
}

// MustRegister is like Register but panics on double-registration. It is the
// intended call site for init() functions — a duplicate registration is always
// a programming error, not a runtime condition.
func MustRegister(name config.IsolationMode, c Constructor) {
	if err := Register(name, c); err != nil {
		panic(err)
	}
}

// For returns a new Isolator for the given mode and construction options. It
// returns a clear error (not a panic) when mode is not registered.
func For(mode config.IsolationMode, opts ConstructorOpts) (Isolator, error) {
	globalRegistry.mu.RLock()
	reg, ok := globalRegistry.registrations[mode]
	globalRegistry.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("container: unknown isolation mode %q; registered modes: %s",
			mode, modesString())
	}
	return reg.Constructor(opts), nil
}

// CapabilitiesFor returns the pre-computed Capabilities snapshot for the given
// isolation mode. This is the intended entry point for callers that only need
// to query capabilities without constructing a live Isolator. It panics if mode
// is not registered — callers that receive a mode from user input should
// validate it with config.IsValidIsolationMode first.
func CapabilitiesFor(mode config.IsolationMode) Capabilities {
	globalRegistry.mu.RLock()
	reg, ok := globalRegistry.registrations[mode]
	globalRegistry.mu.RUnlock()

	if !ok {
		// Unknown mode: return zero Capabilities (all flags false). This
		// produces safe no-op behaviour for callers that do not check the mode
		// in advance, and is consistent with treating an unrecognised mode as
		// "host-like" (no sandbox, no container lifecycle).
		return Capabilities{}
	}
	return reg.Capabilities
}

// Names returns the registered mode names in sorted order. This is the
// authoritative list of modes the current binary knows how to run.
func Names() []config.IsolationMode {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	names := make([]config.IsolationMode, 0, len(globalRegistry.registrations))
	for name := range globalRegistry.registrations {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return names[i] < names[j]
	})
	return names
}

// joinModeNames returns a comma-separated string of the registered mode names.
// The caller must already hold at least a read lock on r.mu.
func joinModeNames(r *IsolationRegistry) string {
	names := make([]string, 0, len(r.registrations))
	for name := range r.registrations {
		names = append(names, string(name))
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// ResolveInput carries all the inputs that the back-compat resolution logic
// needs. Fields marked "deprecated" correspond to the legacy surfaces that
// A.4 will remove; they are preserved verbatim here so that Resolve mirrors
// today's behaviour exactly and A.4 has a single file to edit.
type ResolveInput struct {
	// IsolationFlag is the value of the --isolation CLI flag, empty when the
	// flag was not set (use IsolationFlagChanged to distinguish).
	IsolationFlag string

	// IsolationFlagChanged is true when --isolation was explicitly set by the
	// user (cobra's cmd.Flags().Changed("isolation")).
	IsolationFlagChanged bool

	// HostModeFlag is the value of the deprecated --host-mode CLI flag.
	// Deprecated: use IsolationFlag with "host" instead.
	HostModeFlag bool

	// HostModeFlagChanged is true when --host-mode was explicitly set.
	// Deprecated: A.4 removes this flag.
	HostModeFlagChanged bool

	// DBIsolationMode is the isolation_mode column value from an
	// agent_status DB row. Empty string means the column was not recorded
	// (pre-v10 rows) — fall back to DBHostMode.
	DBIsolationMode string

	// DBHostMode is the host_mode column value from an agent_status DB row.
	// Deprecated: A.4 removes this column after backfilling isolation_mode.
	DBHostMode bool

	// ConfigDefault is the machine-level default from config.json
	// (cfg.DefaultIsolationMode). Always set to a valid mode; the compiled-in
	// fallback is "host".
	ConfigDefault config.IsolationMode
}

// Resolve picks the effective isolation mode from the various sources of truth.
//
// Resolution order:
//  1. --isolation flag (explicit override, validated).
//  2. --host-mode flag (deprecated alias for "host"). Errors if --isolation
//     is also set.
//  3. DB isolation_mode column (from a restored session row).
//  4. DB host_mode column → "host" (back-compat for pre-v10 rows).
//  5. ConfigDefault (cfg.DefaultIsolationMode; compiled-in default: "host").
//
// Cites: cmd/spawn.go:resolveIsolationMode; cmd/switch.go, cmd/pr.go,
//
//	cmd/review.go, cmd/restore.go (ConfigDefault path);
//	internal/db/db.go (Status.EffectiveIsolationMode, DB back-compat path).
func Resolve(input ResolveInput) (config.IsolationMode, error) {
	// Reject simultaneous --isolation and --host-mode.
	if input.IsolationFlagChanged && input.HostModeFlagChanged {
		return "", fmt.Errorf("--isolation and --host-mode cannot be used together; --host-mode is a deprecated alias for --isolation host")
	}

	// 1. --isolation flag.
	if input.IsolationFlagChanged && input.IsolationFlag != "" {
		mode := config.IsolationMode(input.IsolationFlag)
		if !config.IsValidIsolationMode(input.IsolationFlag) {
			return "", fmt.Errorf("unknown isolation mode %q; registered modes: %s",
				input.IsolationFlag, modesString())
		}
		return mode, nil
	}

	// 2. --host-mode (deprecated alias).
	if input.HostModeFlagChanged && input.HostModeFlag {
		return config.IsolationHost, nil
	}

	// 3. DB isolation_mode column (non-empty → use directly).
	if input.DBIsolationMode != "" {
		return config.IsolationMode(input.DBIsolationMode), nil
	}

	// 4. DB host_mode column → "host" (pre-v10 back-compat).
	if input.DBHostMode {
		return config.IsolationHost, nil
	}

	// 5. Machine-level config default (always set; compiled-in default "host").
	if input.ConfigDefault != "" {
		return input.ConfigDefault, nil
	}

	// Final safety net: should not be reached in a correctly configured system.
	return config.IsolationHost, nil
}

// modesString returns a comma-separated string of registered mode names for
// use in error messages. Acquires a read lock on the global registry.
func modesString() string {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()
	return joinModeNames(globalRegistry)
}
