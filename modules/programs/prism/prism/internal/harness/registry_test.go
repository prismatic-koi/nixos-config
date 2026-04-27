package harness_test

// registry_test.go — unit tests for the harness registry.
//
// Covers the acceptance-criteria edge cases:
//   - harness.Lookup("unknown") returns (Registration{}, false)
//   - harness.Names() returns ["opencode"] once the opencode init() has run
//   - MustRegister panics on duplicate registration
//   - Register rejects empty Name / Shape / nil Factory

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/harness"
	// Blank import to trigger the opencode init() registration.
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
)

// noopFactory is a minimal Factory that returns nil (sufficient for
// registration tests that don't exercise the constructed harness).
func noopFactory(endpoint string, _ *http.Client, _, _ string) harness.Harness {
	return &harness.FakeHarness{}
}

func TestLookup_Unknown(t *testing.T) {
	reg, ok := harness.Lookup("unknown-harness-that-does-not-exist")
	if ok {
		t.Errorf("Lookup(unknown): expected ok=false, got ok=true; reg=%+v", reg)
	}
	if !reflect.DeepEqual(reg, harness.Registration{}) {
		t.Errorf("Lookup(unknown): expected zero Registration, got %+v", reg)
	}
}

func TestLookup_Opencode(t *testing.T) {
	reg, ok := harness.Lookup("opencode")
	if !ok {
		t.Fatalf("Lookup(opencode): expected ok=true, got ok=false")
	}
	if reg.Name != "opencode" {
		t.Errorf("Lookup(opencode): Name = %q, want %q", reg.Name, "opencode")
	}
	if reg.Shape != harness.TransportHTTPPort {
		t.Errorf("Lookup(opencode): Shape = %q, want %q", reg.Shape, harness.TransportHTTPPort)
	}
	if reg.Factory == nil {
		t.Error("Lookup(opencode): Factory is nil")
	}
	if reg.ContainerFactory == nil {
		t.Error("Lookup(opencode): ContainerFactory is nil")
	}
}

func TestNames_ContainsOpencode(t *testing.T) {
	names := harness.Names()
	found := false
	for _, n := range names {
		if n == "opencode" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Names() = %v, want it to contain %q", names, "opencode")
	}
}

func TestNames_Sorted(t *testing.T) {
	names := harness.Names()
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("Names() = %v is not sorted ascending: %q < %q", names, names[i], names[i-1])
		}
	}
}

func TestShapeOf_Opencode(t *testing.T) {
	shape, ok := harness.ShapeOf("opencode")
	if !ok {
		t.Fatalf("ShapeOf(opencode): expected ok=true, got ok=false")
	}
	if shape != harness.TransportHTTPPort {
		t.Errorf("ShapeOf(opencode) = %q, want %q", shape, harness.TransportHTTPPort)
	}
}

func TestShapeOf_Unknown(t *testing.T) {
	shape, ok := harness.ShapeOf("no-such-harness")
	if ok {
		t.Errorf("ShapeOf(unknown): expected ok=false, got ok=true; shape=%q", shape)
	}
	if shape != "" {
		t.Errorf("ShapeOf(unknown): expected empty shape, got %q", shape)
	}
}

func TestNew_KnownHarness(t *testing.T) {
	h, err := harness.New("opencode", "", nil, "", "")
	if err != nil {
		t.Fatalf("New(opencode): unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("New(opencode): returned nil Harness")
	}
}

func TestNew_UnknownHarness(t *testing.T) {
	h, err := harness.New("no-such-harness", "", nil, "", "")
	if err == nil {
		t.Fatalf("New(unknown): expected error, got nil; h=%v", h)
	}
	if !strings.Contains(err.Error(), "no-such-harness") {
		t.Errorf("New(unknown): error %q does not mention the harness name", err.Error())
	}
}

func TestNewContainer_KnownHarness(t *testing.T) {
	h, err := harness.NewContainer("opencode", "", nil, "", "")
	if err != nil {
		t.Fatalf("NewContainer(opencode): unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("NewContainer(opencode): returned nil Harness")
	}
}

func TestRegister_RejectsDuplicateName(t *testing.T) {
	// Register a temporary harness with a unique name, then try to register
	// the same name again.  We can't use "opencode" (already registered by
	// init()), so we create a uniquely-named one.
	const name = "test-duplicate-harness"

	err := harness.Register(harness.Registration{
		Name:    name,
		Shape:   harness.TransportStdioPipe,
		Factory: noopFactory,
	})
	if err != nil {
		t.Fatalf("first Register(%q): unexpected error: %v", name, err)
	}

	err = harness.Register(harness.Registration{
		Name:    name,
		Shape:   harness.TransportStdioPipe,
		Factory: noopFactory,
	})
	if err == nil {
		t.Fatalf("second Register(%q): expected duplicate error, got nil", name)
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate error = %q, want it to contain 'already registered'", err.Error())
	}
}

func TestRegister_RejectsEmptyName(t *testing.T) {
	err := harness.Register(harness.Registration{
		Name:    "",
		Shape:   harness.TransportHTTPPort,
		Factory: noopFactory,
	})
	if err == nil {
		t.Fatal("Register(empty Name): expected error, got nil")
	}
}

func TestRegister_RejectsEmptyShape(t *testing.T) {
	err := harness.Register(harness.Registration{
		Name:    "test-empty-shape",
		Shape:   "",
		Factory: noopFactory,
	})
	if err == nil {
		t.Fatal("Register(empty Shape): expected error, got nil")
	}
}

func TestRegister_RejectsUnknownShape(t *testing.T) {
	err := harness.Register(harness.Registration{
		Name:    "test-unknown-shape",
		Shape:   harness.TransportShape("grpc-stream"),
		Factory: noopFactory,
	})
	if err == nil {
		t.Fatal("Register(unknown Shape): expected error, got nil")
	}
}

func TestRegister_RejectsNilFactory(t *testing.T) {
	err := harness.Register(harness.Registration{
		Name:    "test-nil-factory",
		Shape:   harness.TransportHTTPPort,
		Factory: nil,
	})
	if err == nil {
		t.Fatal("Register(nil Factory): expected error, got nil")
	}
}

func TestTransportShape_Constants(t *testing.T) {
	// Smoke-test that the three constants exist and have the correct string values.
	tests := []struct {
		shape harness.TransportShape
		want  string
	}{
		{harness.TransportHTTPPort, "http-port"},
		{harness.TransportStdioPipe, "stdio-pipe"},
		{harness.TransportFallbackScreenScrape, "fallback-screen-scrape"},
	}
	for _, tt := range tests {
		if string(tt.shape) != tt.want {
			t.Errorf("TransportShape constant: got %q, want %q", tt.shape, tt.want)
		}
	}
}
