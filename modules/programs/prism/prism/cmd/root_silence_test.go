package cmd

// Regression tests for the SilenceErrors/SilenceUsage settings on rootCmd
// and the FlagErrorFunc that re-surfaces usage guidance on unknown-flag
// errors. Cobra's silence behaviour changes across major releases. These
// tests pin the intended shape so future upgrades cannot silently regress
// the double-print / usage-dump fix.
//
// What these tests pin:
//   - A failing prism CLI command prints its error message exactly once
//     (to stderr) and exits non-zero. Covered by
//     TestRootCmd_RunEFailure_NoUsageDump_NoDoublePrint.
//   - RunE-originated errors do not print the usage/help block. Covered by
//     TestRootCmd_RunEFailure_NoUsageDump_NoDoublePrint.
//   - Unknown-flag errors still surface usage guidance, exit non-zero, and
//     print the error exactly once. Covered by
//     TestRootCmd_UnknownFlag_StillSurfacesUsageGuidance.
//
// The test-only cobra tree mirrors rootCmd's silence configuration but is
// disjoint from the production tree so a stray subcommand init in the
// package cannot mutate assertion state between subtests.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildTestRootTree constructs a cobra root mirroring rootCmd's silence
// configuration (SilenceErrors + SilenceUsage + FlagErrorFunc) plus a
// single RunE subcommand that returns a fixed sentinel error. The root
// has a --token flag so we can trigger a "required flag missing" / unknown
// flag error path that flows through FlagErrorFunc.
func buildTestRootTree(runE func(*cobra.Command, []string) error) *cobra.Command {
	root := &cobra.Command{
		Use:           "prism",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		c.Println(c.UsageString())
		return err
	})

	sub := &cobra.Command{
		Use:  "widget",
		Args: cobra.NoArgs,
		RunE: runE,
	}
	sub.Flags().String("mode", "", "widget mode")

	root.AddCommand(sub)
	return root
}

var errWidgetSentinel = errors.New("widget failed: something went wrong")

// TestRootCmd_RunEFailure_NoUsageDump_NoDoublePrint verifies that
// a RunE that returns an error must NOT trigger cobra's usage-block dump
// and must NOT print the error message via cobra (leaving main.go as the
// sole printer). The rootCmd.Execute() return value is the error that
// main.go prints exactly once.
func TestRootCmd_RunEFailure_NoUsageDump_NoDoublePrint(t *testing.T) {
	root := buildTestRootTree(func(cmd *cobra.Command, args []string) error {
		return errWidgetSentinel
	})
	root.SetArgs([]string{"widget"})

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.Execute()

	// cobra returns the error to Execute()'s caller so main.go can
	// print it once to os.Stderr and exit non-zero.
	if !errors.Is(err, errWidgetSentinel) {
		t.Fatalf("Execute() err = %v, want %v", err, errWidgetSentinel)
	}
	// No usage block dumped. `Usage:` is cobra's own usage-block
	// header; asserting on it directly is more robust than matching the
	// full block text.
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "Usage:") {
		t.Errorf("output contained cobra 'Usage:' block for RunE failure:\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
	// Cobra must NOT print the error message itself. main.go is the sole
	// printer. Cobra's prefix is "Error:" — assert it does not appear
	// anywhere in either stream.
	if strings.Contains(combined, "Error:") {
		t.Errorf("output contained cobra 'Error:' prefix for RunE failure (double-print):\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
	// The error message itself must also NOT appear in cobra's output —
	// with SilenceErrors it goes only to Execute()'s return.
	if strings.Contains(combined, "widget failed") {
		t.Errorf("output contained RunE error text (should be silent):\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
}

// TestRootCmd_UnknownFlag_StillSurfacesUsageGuidance verifies that an
// unknown flag passed to a subcommand still prints the usage block
// (guidance) AND returns the error to Execute() so main.go can print the
// message exactly once. This is the FlagErrorFunc path that keeps
// unknown-flag misuse discoverable while quiet RunE failures stay quiet.
func TestRootCmd_UnknownFlag_StillSurfacesUsageGuidance(t *testing.T) {
	root := buildTestRootTree(func(cmd *cobra.Command, args []string) error {
		t.Fatalf("RunE must not be called when flag parsing fails")
		return nil
	})
	root.SetArgs([]string{"widget", "--bogus-flag=oops"})

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.Execute()

	// Exit non-zero (Execute returns an error).
	if err == nil {
		t.Fatal("Execute() err = nil, want unknown-flag error")
	}
	// Usage guidance surfaces. Our FlagErrorFunc prints the usage
	// block explicitly via c.Println(c.UsageString()), which lands on the
	// command's OutOrStderr sink. Assert `Usage:` appears somewhere in the
	// captured output.
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "Usage:") {
		t.Errorf("output missing 'Usage:' guidance for unknown-flag error:\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
	// The error message must NOT be double-printed by cobra —
	// SilenceErrors keeps main.go as the sole printer.
	if strings.Contains(combined, "Error:") {
		t.Errorf("output contained cobra 'Error:' prefix for unknown-flag error (double-print):\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
	// Sanity: err carries the unknown-flag message so main.go's
	// fmt.Fprintln surfaces it.
	if !strings.Contains(err.Error(), "bogus-flag") {
		t.Errorf("err = %v; want mention of the offending flag name", err)
	}
}

// TestRootCmd_RootFlagsAreSilenceEnabled is a smoke test that the actual
// production rootCmd carries the silence flags. If a future refactor drops
// them the test surfaces the regression immediately.
func TestRootCmd_RootFlagsAreSilenceEnabled(t *testing.T) {
	if !rootCmd.SilenceErrors {
		t.Error("rootCmd.SilenceErrors = false, want true (issue #2362)")
	}
	if !rootCmd.SilenceUsage {
		t.Error("rootCmd.SilenceUsage = false, want true (issue #2362)")
	}
	// FlagErrorFunc must be installed so unknown-flag errors on any
	// subcommand still print the usage block. Cobra's default
	// FlagErrorFunc returns the error unchanged with no side-effects,
	// which strips usage guidance when root's SilenceUsage is
	// true. Compare against the sentinel returned by FlagErrorFunc() when
	// no custom function is set: it wraps a no-op closure. We assert by
	// running an unknown flag through the real root and checking
	// UsageString content appears in the output.
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs([]string{"--definitely-not-a-real-flag"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("rootCmd.Execute() with unknown flag returned nil error")
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "Usage:") && !strings.Contains(combined, "--help") {
		t.Errorf("unknown-flag against real rootCmd surfaced no usage guidance:\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
	if strings.Contains(combined, "Error:") {
		t.Errorf("unknown-flag against real rootCmd double-printed via cobra 'Error:' prefix:\nstdout=%q\nstderr=%q",
			stdout.String(), stderr.String())
	}
}
