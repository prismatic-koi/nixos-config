// Command iris is the daemon-mode successor to prism (codename iris, D-2).
//
// D-2 scope: binary entrypoint, path layout, config loading, and DB open.
// No daemon behaviour, no socket, no RPC. See docs/daemon-mode-design.md §10
// for the coexistence strategy and §10.1 for the path table.
//
// Usage:
//
//	iris --version   — print version string and exit 0
//	iris version     — same, as a subcommand
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// irisVersion is the version string for the iris binary. It is set to the
// package version at compile time; in development builds it is "dev".
//
// This intentionally does NOT reuse the prism version string or any
// prism-internal ldflags variable — iris has its own identity.
const irisVersion = "0.1.0-d2"

func main() {
	if err := rootCmd.Execute(); err != nil {
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

var rootCmd = &cobra.Command{
	Use:     "iris",
	Short:   "Iris — daemon-mode successor to prism (codename, D-2+)",
	Version: irisVersion,

	// The default run opens the DB and loads config so that a plain `iris`
	// invocation exercises the startup path. --version is handled by cobra
	// before RunE is called.
	RunE: func(cmd *cobra.Command, args []string) error {
		return startup()
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

// versionCmd provides `iris version` as an explicit subcommand in addition to
// the --version flag (cobra wires --version automatically from rootCmd.Version).
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the iris version and exit",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(irisVersion)
		return nil
	},
}

// startup runs the iris initialisation sequence: resolve paths, load config,
// and open the DB. This is the minimal D-2 startup contract.
//
// startup does NOT require ~/.local/state/iris/ or ~/.config/iris/ to exist
// before it is called — it creates the state directory on demand (via
// db.Open → os.MkdirAll) and treats an absent config file as a non-error.
func startup() error {
	p := iris.ResolvePaths()

	// Load config — absent file returns defaults, not an error.
	_, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		return fmt.Errorf("iris: load config: %w", err)
	}

	// Open (or create) the iris DB.
	db, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris: open db: %w", err)
	}
	defer db.Close()

	return nil
}
