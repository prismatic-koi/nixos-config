package cmd

// Test seam for the shared cobra command tree.
//
// The cmd package uses a single global `rootCmd` (and its sub-command tree)
// for both production and tests. Test cases drive the CLI by calling
// rootCmd.SetArgs(...) followed by rootCmd.Execute(). Cobra/pflag stores the
// parsed value of each flag on the *pflag.Flag object itself — once a flag
// has been set, that value persists on the global object until something
// explicitly resets it.
//
// This is the source of a real, reproducible test-bleed flake (#1521):
//
//   - TestRunPrompt_DeliverAs_InvalidValueRejected runs `prompt ... --deliver-as bogus`.
//     After Execute() returns, the persistent --deliver-as flag value on the
//     `prompt` sub-command is "bogus".
//   - On the next iteration of `go test ./cmd/ -count=N` (N>1), cobra is not
//     re-initialised (init() runs once per process). The "bogus" value is
//     still live, so any later TestRunPrompt_* that invokes `prompt` without
//     specifying --deliver-as inherits "bogus" and fails the client-side
//     validation.
//
// The fix is purely structural: snapshot every flag's default value once,
// and reset all `Changed` flags back to their declared default at the start
// of each test that drives the cobra tree via rootCmd.SetArgs / rootCmd.Execute.
// No production code changes — this is a test-only helper.

import (
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rootCmdFlagDefaults captures the declared default value of every flag in
// the rootCmd tree at process start. It is populated lazily (once) on first
// use and never mutated thereafter, so it is safe to read from multiple
// goroutines.
var (
	rootCmdFlagDefaultsOnce sync.Once
	rootCmdFlagDefaults     = map[*pflag.Flag]string{}
)

// captureRootCmdFlagDefaults walks the rootCmd tree and records each flag's
// declared default value (DefValue). This snapshot is the source of truth for
// resetRootCmdFlags — it is taken before any test has a chance to mutate the
// flags via SetArgs/Execute.
func captureRootCmdFlagDefaults() {
	rootCmdFlagDefaultsOnce.Do(func() {
		walkAllFlags(rootCmd, func(f *pflag.Flag) {
			rootCmdFlagDefaults[f] = f.DefValue
		})
	})
}

// walkAllFlags invokes fn on every flag (persistent + local) on c and on
// every flag of every command reachable from c. Flags shared by multiple
// commands are visited multiple times; that is harmless for both capture
// (idempotent) and reset (idempotent).
func walkAllFlags(c *cobra.Command, fn func(*pflag.Flag)) {
	c.Flags().VisitAll(fn)
	c.PersistentFlags().VisitAll(fn)
	for _, sub := range c.Commands() {
		walkAllFlags(sub, fn)
	}
}

// resetRootCmdFlagsTo restores every flag in the rootCmd tree to its captured
// default value and clears the Changed bit. Call this before a test that
// drives rootCmd via SetArgs/Execute, to immunise it against any persistent
// state left behind by a previous test (or a previous iteration under
// `go test -count=N`). t.Cleanup also re-resets after the test so the next
// test starts from a clean slate even if this test fails mid-flight.
//
// Only call this from non-parallel tests — rootCmd is a package-level global.
func resetRootCmdFlags(t *testing.T) {
	t.Helper()
	captureRootCmdFlagDefaults()

	doReset := func() {
		walkAllFlags(rootCmd, func(f *pflag.Flag) {
			def, ok := rootCmdFlagDefaults[f]
			if !ok {
				// A flag that did not exist at capture time. Cobra/pflag
				// guarantees DefValue is the declared default, so falling
				// back to f.DefValue is correct.
				def = f.DefValue
			}
			// Set() is the public API for restoring a flag value. It also
			// records Changed=true, so after the loop we explicitly clear
			// Changed back to false to match the pristine post-init state.
			if err := f.Value.Set(def); err != nil {
				// A default value should always parse — if it does not,
				// the flag was declared with an invalid default and that
				// is a bug in the production code, not in the helper.
				t.Fatalf("resetRootCmdFlags: cannot reset --%s to default %q: %v",
					f.Name, def, err)
			}
			f.Changed = false
		})
		// Clear any leftover args from a prior SetArgs() call.
		rootCmd.SetArgs(nil)
	}

	doReset()
	t.Cleanup(doReset)
}
