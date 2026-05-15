package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/prismatic-koi/prism/cmd"
)

// exitCoder is implemented by errors that want to override the default exit
// code of 1. Used by --wait paths (cmd/wait.go) to distinguish timeouts and
// user interrupts from real terminal failures (issue #1500).
type exitCoder interface {
	ExitCode() int
}

// irisParityTripwireExitCode is the exit code prism uses when the iris
// parity gate tripwire env var IRIS_PARITY_TEST_MODE is set. A non-1 code
// makes accidental invocations easy to spot in CI logs.
const irisParityTripwireExitCode = 99

func main() {
	// Iris parity gate tripwire (issue #1641). The iris parity test suite
	// sets IRIS_PARITY_TEST_MODE=1 to prove no prism binary is invoked by
	// any parity test. When the variable is set we exit 99 with a clear
	// message before any prism-specific work runs.
	if os.Getenv("IRIS_PARITY_TEST_MODE") != "" {
		fmt.Fprintln(os.Stderr, "iris parity test mode active: prism binary must not be invoked from iris parity tests (see issue #1641)")
		os.Exit(irisParityTripwireExitCode)
	}
	if err := cmd.Execute(); err != nil {
		var ec exitCoder
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
