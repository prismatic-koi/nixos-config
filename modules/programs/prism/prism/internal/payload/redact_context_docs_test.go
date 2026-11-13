package payload_test

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

// TestContextShapes_LeaveTheUniFiInstructionsUnchanged runs the text that
// tells an agent how to use the UniFi API, and the two files that hold the
// shape rules, through the context shapes (issue #3476). Control 3 redacts the
// result of each tool, read included. If a context shape matches a command
// or a comment in this text, the agent gets broken text, an edit of it
// fails, and the agent can write the broken text back to the file.
//
// The skill and prism.ts are outside the prism subtree, so the nix build
// sandbox does not have them. The test then checks the files of the subtree
// only. The test files are not in the list: they hold session values on
// purpose.
func TestContextShapes_LeaveTheUniFiInstructionsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		path     string
		optional bool
	}{
		{"../../docs/invariants/session-lifecycle.md", false},
		{"redact.go", false},
		{"../../../repo-skills/unifi/SKILL.md", true},
		{extensionSourcePath, true},
	} {
		b, err := os.ReadFile(tc.path)
		if tc.optional && errors.Is(err, fs.ErrNotExist) {
			t.Logf("%s is not present (expected inside the nix build sandbox)", tc.path)
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if got := payload.RedactContextShapesForTest(string(b)); got != string(b) {
			t.Errorf("the context shapes change %s: %s", tc.path, mismatch(got, string(b)))
		}
	}
}
