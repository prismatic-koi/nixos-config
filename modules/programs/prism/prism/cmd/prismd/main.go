// Command prismd is the prism daemon binary. The first (and currently
// only) subcommand surface is `mux`, the prism-native multiplexer
// server (programme #2147). Future daemons that share the prism Go
// module can live under the same binary as sibling subcommands.
//
// Splitting the daemon out of the `prism` CLI binary is deliberate:
//
//   - The CLI binary is invoked synchronously from interactive shells
//     and from prism spawn / cleanup paths; it must stay fast to start
//     and small to copy. Pulling in a long-lived daemon's wiring
//     (signal handlers, bubbletea, snapshot loops) would weigh it down
//     for no benefit at the CLI use-case.
//
//   - The daemon's entry-point shape (fork/detach vs --foreground for
//     systemd) is naturally distinct from the CLI's "execute and
//     return" shape; mixing them under one cobra root invited bugs.
//
//   - Both binaries are built from the same Go module so they share
//     all the internal packages (internal/mux/*, internal/sidecar,
//     internal/session) without code duplication.
//
// The `prismd` binary is invoked by:
//
//   - The systemd user unit at modules/programs/prism/prismd-mux.nix
//     (Linux) — calls `prismd mux start --foreground`.
//   - The launchd user agent at the same nix file (Darwin) — calls
//     `prismd mux start --foreground`.
//   - A human on the CLI running `prismd mux start` to launch the
//     daemon ad-hoc; this fork-detaches.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/prismatic-koi/prism/cmd/prismd/internalcmd"
)

func main() {
	if err := internalcmd.Execute(); err != nil {
		// Surface the error message — cobra already printed usage on
		// argument-shape errors; we only need to write the message to
		// stderr and pick an exit code.
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(ec.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
