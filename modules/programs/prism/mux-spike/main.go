// mux-spike: feasibility spike for the prism-native multiplexer programme.
//
// See issue #2141 for the artefact contract and
// modules/programs/prism/prism/docs/multiplexer-proposal.md for the wider
// design context. This binary is not shipped to users — it characterises
// charmbracelet/x/vt against a hand-graded TUI corpus and returns a
// proceed/stop verdict, then gets deleted on cleanup.
package main

import (
	"fmt"
	"os"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/cmd"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub, rest := os.Args[1], os.Args[2:]
	switch sub {
	case "run":
		if err := cmd.Run(rest); err != nil {
			fmt.Fprintf(os.Stderr, "mux-spike run: %v\n", err)
			os.Exit(1)
		}
	case "corpus":
		if err := cmd.Corpus(rest); err != nil {
			fmt.Fprintf(os.Stderr, "mux-spike corpus: %v\n", err)
			os.Exit(1)
		}
	case "screenshot":
		if err := cmd.Screenshot(rest); err != nil {
			fmt.Fprintf(os.Stderr, "mux-spike screenshot: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mux-spike: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `mux-spike — x/vt fidelity spike (see issue #2141)

USAGE:
  mux-spike run <cmd> [args...]                 interactive smoke test
  mux-spike corpus [--out DIR] [--manifest F]   non-interactive corpus walk
  mux-spike screenshot --in FILE --cell R,C     per-cell diagnostic
              [--cells N]

Interactive run exits on Ctrl-q.`)
}
