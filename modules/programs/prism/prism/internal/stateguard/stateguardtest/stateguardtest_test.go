package stateguardtest

import (
	"os"
	"testing"
)

// TestIsolateSuite_HelperKeepsParentEnvironment shows that a helper process
// of an isolated suite keeps the XDG_STATE_HOME that its parent test chose,
// and makes no directory of its own.
func TestIsolateSuite_HelperKeepsParentEnvironment(t *testing.T) {
	parent := t.TempDir()
	testState := t.TempDir()
	t.Setenv(SuiteRootEnv, parent)
	t.Setenv("XDG_STATE_HOME", testState)

	root, restore := IsolateSuite()
	defer restore()

	if root != parent {
		t.Errorf("root = %q, want the parent dir %q", root, parent)
	}
	if got := os.Getenv("XDG_STATE_HOME"); got != testState {
		t.Errorf("XDG_STATE_HOME = %q, want the value of the parent test %q", got, testState)
	}
}
