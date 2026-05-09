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

func main() {
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
