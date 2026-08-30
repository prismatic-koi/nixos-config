package cmd

// Internal regression tests for resetRootCmdFlags itself.
//
// These tests target the helper directly, exercising both the scalar-flag
// reset path and the slice/array-flag reset path. The slice path is the
// subtle one: pflag's StringArray and StringSlice values track an internal
// `changed` bit, and once it is set, Value.Set() *appends* rather than
// *replaces*. Calling Set(DefValue) — where DefValue is "[]" for an empty
// default — therefore appends the literal string "[]" to the slice, which
// is precisely the corruption we want to prevent.

import (
	"testing"
)

// TestResetRootCmdFlags_ScalarFlagRestored verifies the basic scalar-flag
// reset path: a string flag mutated by Execute() is restored to its
// declared default after resetRootCmdFlags runs.
func TestResetRootCmdFlags_ScalarFlagRestored(t *testing.T) {
	// promptCmd's --deliver-as has a non-empty declared default ("steer").
	// This is the exact flag whose bleed produces the flake.
	flag := promptCmd.Flags().Lookup("deliver-as")
	if flag == nil {
		t.Fatal("--deliver-as flag not found on promptCmd")
	}

	// Mutate it as if a previous test had run.
	if err := flag.Value.Set("bogus"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	flag.Changed = true
	if got := flag.Value.String(); got != "bogus" {
		t.Fatalf("pre-reset value = %q, want %q", got, "bogus")
	}

	// Reset and verify.
	resetRootCmdFlags(t)
	if got := flag.Value.String(); got != "steer" {
		t.Errorf("post-reset value = %q, want %q (declared default)", got, "steer")
	}
	if flag.Changed {
		t.Errorf("post-reset Changed = true, want false")
	}
}

// TestResetRootCmdFlags_SliceFlagReplacedNotAppended verifies that for
// pflag.SliceValue flags (StringArray, StringSlice), reset clears the slice
// outright rather than appending to it. This is the bug the review caught:
// without SliceValue.Replace(nil), the slice grows on every reset call,
// silently corrupting the global flag tree.
func TestResetRootCmdFlags_SliceFlagReplacedNotAppended(t *testing.T) {
	// --abtest is a StringArray on spawnCmd with default nil ("[]").
	flag := spawnCmd.Flags().Lookup("abtest")
	if flag == nil {
		t.Fatal("--abtest flag not found on spawnCmd")
	}

	// Mutate it as if a previous test had run with --abtest A --abtest B.
	if err := flag.Value.Set("profileA"); err != nil {
		t.Fatalf("Set profileA: %v", err)
	}
	if err := flag.Value.Set("profileB"); err != nil {
		t.Fatalf("Set profileB: %v", err)
	}
	flag.Changed = true

	// Round-trip the reset twice to prove the slice does not grow on
	// repeated calls — exactly the corruption mode the previous review
	// flagged. If the implementation ever regresses to Set(DefValue) on a
	// slice, the second reset would observe a non-empty slice on entry.
	resetRootCmdFlags(t)
	if got := flag.Value.String(); got != "[]" {
		t.Errorf("after first reset: --abtest = %q, want %q", got, "[]")
	}
	if flag.Changed {
		t.Errorf("after first reset: Changed = true, want false")
	}

	// Second reset on an already-clean slice must remain clean.
	resetRootCmdFlags(t)
	if got := flag.Value.String(); got != "[]" {
		t.Errorf("after second reset: --abtest = %q, want %q (slice grew on idempotent reset)",
			got, "[]")
	}
}
