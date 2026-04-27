package integration_test

// fakestdio_test.go registers a test-only "fake-stdio" harness adapter with
// TransportStdioPipe shape. Registration happens via init() so it is available
// to all tests in this package. Because this file is a _test.go file it is
// compiled only into test binaries, never into the production prism binary.

import (
	"net/http"

	"github.com/prismatic-koi/prism/internal/harness"
)

func init() {
	harness.MustRegister(harness.Registration{
		Name:  "fake-stdio",
		Shape: harness.TransportStdioPipe,
		// Factory returns a FakeHarness stub. For stdio-pipe harnesses, the
		// sidecar uses Config.HarnessBinaryPath directly and does not call
		// Subscribe() on this adapter. The stub satisfies the harness.Harness
		// interface so construction succeeds, but its methods are never invoked
		// in the stdio startup path.
		Factory: func(_ string, _ *http.Client, _, _ string) harness.Harness {
			return &harness.FakeHarness{}
		},
	})
}
