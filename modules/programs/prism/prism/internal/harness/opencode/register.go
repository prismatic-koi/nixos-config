package opencode

// register.go — registers the opencode adapter with the harness registry at
// process startup via an init() function.
//
// Callers that today import this package directly for opencode.New /
// opencode.NewContainerMode should switch to a blank import:
//
//	import _ "github.com/prismatic-koi/prism/internal/harness/opencode"
//
// The harness.New / harness.NewContainer functions then construct the
// correct adapter via the registry without any direct dependency on this
// package. B.2 proposal §5.15.

import (
	"net/http"

	"github.com/prismatic-koi/prism/internal/harness"
)

func init() {
	harness.MustRegister(harness.Registration{
		Name:  "opencode",
		Shape: harness.TransportHTTPPort,
		Factory: func(endpoint string, c *http.Client, role, model string) harness.Harness {
			return New(endpoint, c, role, model)
		},
		ContainerFactory: func(endpoint string, c *http.Client, role, model string) harness.Harness {
			return NewContainerMode(endpoint, c, role, model)
		},
	})
}
