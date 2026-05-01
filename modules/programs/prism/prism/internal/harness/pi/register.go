package pi

// register.go — registers the PI adapter with the harness registry at process
// startup via an init() function.
//
// Import this package with a blank import to activate PI support:
//
//	import _ "github.com/prismatic-koi/prism/internal/harness/pi"
//
// The harness name is "pi", matching the agent_status.harness column value
// written by the sidecar when a PI session is started.

import (
	"net/http"

	"github.com/prismatic-koi/prism/internal/harness"
)

func init() {
	harness.MustRegister(harness.Registration{
		Name:  "pi",
		Shape: harness.TransportSocketPipe,
		Factory: func(endpoint string, _ *http.Client, role, model string) harness.Harness {
			// endpoint is unused for socket-pipe harnesses; the pipe socket
			// path is managed by the sidecar independently of the harness adapter.
			return New(endpoint, role, model)
		},
		// ContainerFactory is nil — PI uses the same socket-pipe transport in both
		// host and container modes; the single Factory is sufficient.
		ArchiveAdapterFactory: func() harness.ArchiveAdapter {
			return NewArchiveAdapter()
		},
	})
}
