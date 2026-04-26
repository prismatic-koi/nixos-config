package harness

// registry.go — global harness registry, TransportShape enum, and
// the Factory / Registration types that adapters use to register.
//
// Design mirrors database/sql.Register: adapter packages call
// harness.MustRegister from their init() functions, and consumers call
// harness.New / harness.NewContainer / harness.Lookup / harness.Names
// without importing the adapter package directly (only a blank import is
// needed to trigger registration).
//
// B.2 proposal: modules/programs/prism/prism/docs/reviews/
// B2-harness-registry-and-transport-shape.md §3–§4.

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// TransportShape declares the wire-level shape a harness uses to talk to
// its agent runtime. It is a registration-time property of each harness
// adapter: code that does not need to know the harness name (container
// manager, sidecar lifecycle, agent-pane command builder) consults the
// shape instead.
//
// The enum is closed: registration with an unknown TransportShape value
// fails at init time. Adding a new value is a deliberate change that
// requires updating every consumer that switches on the value.
type TransportShape string

const (
	// TransportHTTPPort declares that the harness runs a long-lived
	// HTTP server inside its container (or process) and the sidecar
	// dials it on a TCP port. Health checks are HTTP probes against
	// a known endpoint. Event delivery is server-sent events (SSE) or
	// long-polling. Prompts are POSTed; the response status code is the
	// delivery acknowledgement. Examples: opencode (today),
	// Claude Code (planned).
	TransportHTTPPort TransportShape = "http-port"

	// TransportStdioPipe declares that the harness runs as a child
	// process whose stdin/stdout the sidecar (or sidecar-equivalent)
	// owns. Wire format is JSON Lines (or another framed stream).
	// Health is the process being alive and the pipe being open.
	// Prompts are written to stdin fire-and-forget — the OS write
	// succeeds or fails, but there is no transport-level
	// acknowledgement that the harness has processed the prompt.
	// Examples: PI (planned, RFC #606), Codex (likely future).
	TransportStdioPipe TransportShape = "stdio-pipe"

	// TransportFallbackScreenScrape declares that the harness has no
	// structured wire protocol and the sidecar must observe the
	// agent's behaviour by reading its TTY output (typically via a
	// capture pipe attached to the container's PTY). Prompt delivery
	// is by writing to the harness's controlling TTY rather than to
	// any structured channel. Health is "the process is alive".
	// This is the safety-net shape for harnesses that ship before
	// their structured protocol is stable, and for any future harness
	// whose vendor declines to expose a structured API.
	TransportFallbackScreenScrape TransportShape = "fallback-screen-scrape"
)

// knownShapes is the closed set of valid TransportShape values. Registration
// with an unknown value is rejected so that typos and future enum extensions
// are caught at init time rather than silently at runtime.
var knownShapes = map[TransportShape]bool{
	TransportHTTPPort:             true,
	TransportStdioPipe:            true,
	TransportFallbackScreenScrape: true,
}

// Factory constructs a harness.Harness adapter for a single sidecar
// session. The arguments are the cross-harness inputs the registry
// promises to provide to every adapter:
//
//   - endpoint: the transport-specific endpoint hint. For
//     TransportHTTPPort this is the URL the sidecar dials (e.g.
//     "http://localhost:4096"). For TransportStdioPipe this is the
//     path to the harness binary (or a launch spec — TBD by B.3).
//     For TransportFallbackScreenScrape this is the path to the TTY
//     capture pipe. The factory treats it as opaque per its shape.
//   - httpClient: optional. Non-nil when the caller wants to inject
//     a test client; nil means the adapter uses its default. Ignored
//     by stdio and screen-scrape adapters.
//   - agentRole: "worker" | "coordinator" | "" (review subagents
//     pass empty here; their role is set later via env vars).
//   - agentModel: model identifier (e.g. "anthropic/claude-sonnet-4-6").
//     Empty means "let the adapter resolve from its config".
//
// Factory is intentionally narrow: it returns a Harness, not a
// concrete adapter type.
type Factory func(endpoint string, httpClient *http.Client, agentRole, agentModel string) Harness

// Registration is the data an adapter package supplies at registration
// time. The Name is the user-visible identifier (matches the --harness
// flag value and the agent_status.harness column). Shape is the
// declared transport shape; consumers may switch on it without
// knowing the name.
type Registration struct {
	Name  string
	Shape TransportShape
	// Factory constructs a host-mode harness adapter.
	Factory Factory

	// ContainerFactory, when non-nil, is the factory used in container
	// mode. It exists because the opencode adapter today exposes
	// opencode.New / opencode.NewContainerMode as two separate
	// constructors that differ in CreateSession and DeliverInitialPrompt
	// semantics. Other harnesses (stdio especially) probably need a
	// single Factory; ContainerFactory is opt-in.
	//
	// When nil, NewContainer falls back to Factory.
	ContainerFactory Factory
}

// globalRegistry is the package-level singleton. All fields are
// protected by mu after process start; init() functions run
// sequentially so no lock is needed during registration itself, but
// callers after init may run concurrently.
var (
	mu           sync.RWMutex
	registrations = map[string]Registration{}
)

// Register adds a Registration to the global registry. It returns an
// error for: empty Name, empty/unknown Shape, nil Factory, or
// duplicate Name. MustRegister panics on the same conditions and is
// the form intended for init().
//
//	// in internal/harness/opencode/register.go
//	func init() {
//	    harness.MustRegister(harness.Registration{
//	        Name:             "opencode",
//	        Shape:            harness.TransportHTTPPort,
//	        Factory:          ...,
//	        ContainerFactory: ...,
//	    })
//	}
func Register(reg Registration) error {
	if reg.Name == "" {
		return fmt.Errorf("harness.Register: Name must not be empty")
	}
	if reg.Shape == "" {
		return fmt.Errorf("harness.Register: Shape must not be empty for harness %q", reg.Name)
	}
	if !knownShapes[reg.Shape] {
		return fmt.Errorf("harness.Register: unknown TransportShape %q for harness %q; valid values: http-port, stdio-pipe, fallback-screen-scrape", reg.Shape, reg.Name)
	}
	if reg.Factory == nil {
		return fmt.Errorf("harness.Register: Factory must not be nil for harness %q", reg.Name)
	}

	mu.Lock()
	defer mu.Unlock()
	if _, exists := registrations[reg.Name]; exists {
		return fmt.Errorf("harness.Register: harness %q is already registered", reg.Name)
	}
	registrations[reg.Name] = reg
	return nil
}

// MustRegister calls Register and panics if it returns an error.
// Intended for use in adapter package init() functions.
func MustRegister(reg Registration) {
	if err := Register(reg); err != nil {
		panic(err)
	}
}

// Lookup returns the Registration for a given harness name, or
// (Registration{}, false) if no harness with that name is registered.
// Callers that want a usable harness use New / NewContainer instead;
// Lookup is for the spawn allow-list and for tests.
func Lookup(name string) (Registration, bool) {
	mu.RLock()
	defer mu.RUnlock()
	reg, ok := registrations[name]
	return reg, ok
}

// Names returns the registered harness names in deterministic order
// (sorted ascending). Used by:
//   - the --harness flag's validation error message ("valid: opencode, pi"),
//   - the host-API /spawn handler's allow-list,
//   - dashboard / stats display.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registrations))
	for name := range registrations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// New constructs a host-mode harness adapter for the named harness.
// Returns an error if the name is not registered. The endpoint /
// httpClient / agentRole / agentModel arguments are forwarded to the
// registered Factory verbatim.
//
// Replaces the hard-coded opencode.New(...) calls at cmd/sidecar.go:296
// and the construction-only-for-side-effects opencode.New("", nil, "", "")
// calls in cmd/spawn.go, cmd/agent_run.go, cmd/restore.go, cmd/switch.go,
// cmd/pr.go, cmd/review.go.
func New(name, endpoint string, httpClient *http.Client, agentRole, agentModel string) (Harness, error) {
	reg, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("harness %q is not registered; valid harnesses: %s", name, joinNames())
	}
	return reg.Factory(endpoint, httpClient, agentRole, agentModel), nil
}

// NewContainer constructs a container-mode harness adapter for the
// named harness. If the registration has a ContainerFactory it is used;
// otherwise Factory is used. Returns an error if the name is not
// registered.
//
// Replaces the hard-coded opencode.NewContainerMode(...) call at
// cmd/sidecar.go:294.
func NewContainer(name, endpoint string, httpClient *http.Client, agentRole, agentModel string) (Harness, error) {
	reg, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("harness %q is not registered; valid harnesses: %s", name, joinNames())
	}
	f := reg.ContainerFactory
	if f == nil {
		f = reg.Factory
	}
	return f(endpoint, httpClient, agentRole, agentModel), nil
}

// ShapeOf returns the declared TransportShape for the named harness,
// or ("", false) if not registered. Convenience over Lookup for sites
// that only need the shape (e.g. session.StartSidecarWithOpts deciding
// whether to allocate a port).
func ShapeOf(name string) (TransportShape, bool) {
	reg, ok := Lookup(name)
	if !ok {
		return "", false
	}
	return reg.Shape, true
}

// joinNames returns the sorted registered names as a comma-separated
// string, for use in error messages.
func joinNames() string {
	names := Names()
	result := ""
	for i, n := range names {
		if i > 0 {
			result += ", "
		}
		result += n
	}
	return result
}
