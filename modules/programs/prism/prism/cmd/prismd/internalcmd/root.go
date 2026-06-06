// Package internalcmd is the cobra command tree for the prismd binary.
//
// Split out from main package so tests can construct the tree without
// invoking os.Exit. The split is symmetric with the existing prism
// CLI's cmd/ package, which is imported by main.go in the parent
// directory.
package internalcmd

import (
	"github.com/spf13/cobra"
)

// NewRoot returns a fresh root command for the prismd binary. Each
// call returns an independent tree — useful for tests that want to
// exercise multiple commands in the same process without flag-state
// bleed.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "prismd",
		Short:         "Prism daemon — long-lived server processes for prism",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(NewMuxCommand())
	return root
}

// Execute is the conventional cobra Execute entry point used by main.
// It constructs a fresh root and runs it. The return value is the
// error from RunE / RunE-equivalent; cobra's usage / argument-parsing
// errors are also returned here because SilenceErrors is set on the
// root.
func Execute() error {
	return NewRoot().Execute()
}
