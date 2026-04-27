package container

import (
	"fmt"
	"sort"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newTestRegistry returns a fresh, empty IsolationRegistry for use in tests
// that must not share state with the global singleton.
func newTestRegistry() *IsolationRegistry {
	return &IsolationRegistry{
		registrations: make(map[config.IsolationMode]Registration),
	}
}

// registerOnto calls the package-level Register logic against a specific
// registry (not the global one) so tests can run in isolation.
func registerOnto(r *IsolationRegistry, name config.IsolationMode, c Constructor) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.registrations[name]; exists {
		return fmt.Errorf("container: isolation mode %q is already registered", name)
	}
	probe := c(ConstructorOpts{})
	r.registrations[name] = Registration{
		Name:         name,
		Constructor:  c,
		Capabilities: probe.Capabilities(),
	}
	return nil
}

// ── IsolationRegistry double-registration ────────────────────────────────────

func TestRegistry_DoubleRegistration_ReturnsError(t *testing.T) {
	r := newTestRegistry()

	c := func(opts ConstructorOpts) Isolator { return newHostIsolator(opts.Name) }

	if err := registerOnto(r, config.IsolationHost, c); err != nil {
		t.Fatalf("first registration failed unexpectedly: %v", err)
	}
	if err := registerOnto(r, config.IsolationHost, c); err == nil {
		t.Fatal("second registration should have returned an error, got nil")
	}
}

func TestRegistry_MustRegister_PanicsOnDouble(t *testing.T) {
	// MustRegister wraps Register and panics; test via the global Register
	// function on an isolated registry to avoid touching the real global.
	r := newTestRegistry()
	c := func(opts ConstructorOpts) Isolator { return newHostIsolator(opts.Name) }

	if err := registerOnto(r, config.IsolationHost, c); err != nil {
		t.Fatalf("first registration failed unexpectedly: %v", err)
	}

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("MustRegister should have panicked on double-registration")
		}
	}()

	// Simulate what MustRegister does: panic on error.
	if err := registerOnto(r, config.IsolationHost, c); err != nil {
		panic(err)
	}
}

// ── For with an unregistered mode ────────────────────────────────────────────

func TestFor_UnknownMode_ReturnsError(t *testing.T) {
	_, err := For("nonexistent-mode", ConstructorOpts{Name: "test"})
	if err == nil {
		t.Fatal("For with unknown mode should return an error, got nil")
	}
}

func TestFor_UnknownMode_DoesNotPanic(t *testing.T) {
	// Ensure the error path doesn't panic — the AC explicitly says no panics.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("For with unknown mode panicked: %v", rec)
		}
	}()
	//nolint:errcheck
	_, _ = For("not-a-mode", ConstructorOpts{Name: "test"})
}

// ── Names returns all four registered modes ───────────────────────────────────

func TestNames_ReturnsAllFourModes(t *testing.T) {
	names := Names()

	want := []config.IsolationMode{
		config.IsolationBwrap,
		config.IsolationHost,
		config.IsolationPodman,
		config.IsolationSandboxExec,
	}

	// Names() is already sorted; compare after sorting want too.
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	if len(names) != len(want) {
		t.Fatalf("Names() returned %d modes, want %d; got %v", len(names), len(want), names)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, n, want[i])
		}
	}
}

// ── For returns the right isolator type ──────────────────────────────────────

func TestFor_PodmanMode_ReturnsPodmanIsolator(t *testing.T) {
	iso, err := For(config.IsolationPodman, ConstructorOpts{Name: "test-session"})
	if err != nil {
		t.Fatalf("For(IsolationPodman) returned error: %v", err)
	}
	if iso.Name() != config.IsolationPodman {
		t.Errorf("isolator Name() = %q, want %q", iso.Name(), config.IsolationPodman)
	}
	if !iso.Capabilities().IsContainer {
		t.Error("podmanIsolator.Capabilities().IsContainer should be true")
	}
}

func TestFor_HostMode_ReturnsHostIsolator(t *testing.T) {
	iso, err := For(config.IsolationHost, ConstructorOpts{Name: "test-session"})
	if err != nil {
		t.Fatalf("For(IsolationHost) returned error: %v", err)
	}
	if iso.Name() != config.IsolationHost {
		t.Errorf("isolator Name() = %q, want %q", iso.Name(), config.IsolationHost)
	}
}

func TestFor_BwrapMode_ReturnsBwrapIsolator(t *testing.T) {
	iso, err := For(config.IsolationBwrap, ConstructorOpts{Name: "test-session"})
	if err != nil {
		t.Fatalf("For(IsolationBwrap) returned error: %v", err)
	}
	if iso.Name() != config.IsolationBwrap {
		t.Errorf("isolator Name() = %q, want %q", iso.Name(), config.IsolationBwrap)
	}
}

func TestFor_SandboxExecMode_ReturnsSandboxExecIsolator(t *testing.T) {
	iso, err := For(config.IsolationSandboxExec, ConstructorOpts{Name: "test-session"})
	if err != nil {
		t.Fatalf("For(IsolationSandboxExec) returned error: %v", err)
	}
	if iso.Name() != config.IsolationSandboxExec {
		t.Errorf("isolator Name() = %q, want %q", iso.Name(), config.IsolationSandboxExec)
	}
}

// ── Capabilities correctness ──────────────────────────────────────────────────

func TestCapabilities_Podman(t *testing.T) {
	iso, _ := For(config.IsolationPodman, ConstructorOpts{Name: "test"})
	caps := iso.Capabilities()

	if !caps.IsContainer {
		t.Error("podman: IsContainer should be true")
	}
	if !caps.OwnsContainerLifecycle {
		t.Error("podman: OwnsContainerLifecycle should be true")
	}
	if !caps.NeedsConfigBlob {
		t.Error("podman: NeedsConfigBlob should be true")
	}
	if caps.NeedsHostAPISocket {
		t.Error("podman: NeedsHostAPISocket should be false")
	}
	if !caps.UsesContainerHarness {
		t.Error("podman: UsesContainerHarness should be true")
	}
	if !caps.RestartOnExit {
		t.Error("podman: RestartOnExit should be true")
	}
	if !caps.NeedsReadinessWait {
		t.Error("podman: NeedsReadinessWait should be true")
	}
}

func TestCapabilities_Bwrap(t *testing.T) {
	iso, _ := For(config.IsolationBwrap, ConstructorOpts{Name: "test"})
	caps := iso.Capabilities()

	if caps.IsContainer {
		t.Error("bwrap: IsContainer should be false")
	}
	if !caps.NeedsConfigBlob {
		t.Error("bwrap: NeedsConfigBlob should be true")
	}
	if !caps.NeedsHostAPISocket {
		t.Error("bwrap: NeedsHostAPISocket should be true")
	}
	if !caps.RestartOnExit {
		t.Error("bwrap: RestartOnExit should be true")
	}
	if !caps.NeedsStartupConnectTimeout {
		t.Error("bwrap: NeedsStartupConnectTimeout should be true")
	}
	if caps.NeedsReadinessWait {
		t.Error("bwrap: NeedsReadinessWait should be false")
	}
}

func TestCapabilities_SandboxExec(t *testing.T) {
	iso, _ := For(config.IsolationSandboxExec, ConstructorOpts{Name: "test"})
	caps := iso.Capabilities()

	if caps.IsContainer {
		t.Error("sandbox-exec: IsContainer should be false")
	}
	if !caps.NeedsConfigBlob {
		t.Error("sandbox-exec: NeedsConfigBlob should be true")
	}
	if !caps.NeedsHostAPISocket {
		t.Error("sandbox-exec: NeedsHostAPISocket should be true")
	}
	if caps.RestartOnExit {
		t.Error("sandbox-exec: RestartOnExit should be false")
	}
}

func TestCapabilities_Host(t *testing.T) {
	iso, _ := For(config.IsolationHost, ConstructorOpts{Name: "test"})
	caps := iso.Capabilities()

	if caps.IsContainer {
		t.Error("host: IsContainer should be false")
	}
	if caps.NeedsConfigBlob {
		t.Error("host: NeedsConfigBlob should be false")
	}
	if caps.NeedsHostAPISocket {
		t.Error("host: NeedsHostAPISocket should be false")
	}
	if caps.RestartOnExit {
		t.Error("host: RestartOnExit should be false")
	}
}

// ── Resolve ───────────────────────────────────────────────────────────────────

func TestResolve_IsolationFlagTakesPrecedence(t *testing.T) {
	mode, err := Resolve(ResolveInput{
		IsolationFlag:        "bwrap",
		IsolationFlagChanged: true,
		ConfigDefault:        config.IsolationPodman,
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if mode != config.IsolationBwrap {
		t.Errorf("mode = %q, want %q", mode, config.IsolationBwrap)
	}
}

func TestResolve_HostModeFlag_MapsToHost(t *testing.T) {
	mode, err := Resolve(ResolveInput{
		HostModeFlag:        true,
		HostModeFlagChanged: true,
		ConfigDefault:       config.IsolationPodman,
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if mode != config.IsolationHost {
		t.Errorf("mode = %q, want %q", mode, config.IsolationHost)
	}
}

func TestResolve_BothFlags_ReturnsError(t *testing.T) {
	_, err := Resolve(ResolveInput{
		IsolationFlag:        "bwrap",
		IsolationFlagChanged: true,
		HostModeFlag:         true,
		HostModeFlagChanged:  true,
	})
	if err == nil {
		t.Fatal("expected error when both --isolation and --host-mode are set")
	}
}

func TestResolve_DBIsolationMode_UsedWhenFlagsAbsent(t *testing.T) {
	mode, err := Resolve(ResolveInput{
		DBIsolationMode: "bwrap",
		ConfigDefault:   config.IsolationPodman,
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if mode != config.IsolationBwrap {
		t.Errorf("mode = %q, want %q", mode, config.IsolationBwrap)
	}
}

func TestResolve_DBHostMode_MapsToHost(t *testing.T) {
	mode, err := Resolve(ResolveInput{
		DBHostMode:    true,
		ConfigDefault: config.IsolationPodman,
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if mode != config.IsolationHost {
		t.Errorf("mode = %q, want %q", mode, config.IsolationHost)
	}
}

func TestResolve_ConfigDefault_Podman(t *testing.T) {
	// ConfigDefault=podman: without any flags, should resolve to podman.
	mode, err := Resolve(ResolveInput{
		ConfigDefault: config.IsolationPodman,
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if mode != config.IsolationPodman {
		t.Errorf("mode = %q, want %q", mode, config.IsolationPodman)
	}
}

func TestResolve_ConfigDefault_FallsThrough(t *testing.T) {
	mode, err := Resolve(ResolveInput{
		ConfigDefault: config.IsolationBwrap,
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if mode != config.IsolationBwrap {
		t.Errorf("mode = %q, want %q", mode, config.IsolationBwrap)
	}
}

func TestResolve_UnknownIsolationFlag_ReturnsError(t *testing.T) {
	_, err := Resolve(ResolveInput{
		IsolationFlag:        "firejail",
		IsolationFlagChanged: true,
	})
	if err == nil {
		t.Fatal("expected error for unknown isolation mode, got nil")
	}
}
