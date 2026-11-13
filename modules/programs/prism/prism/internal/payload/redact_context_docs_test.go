package payload_test

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

// TestContextShapes_LeaveTheUniFiInstructionsUnchanged runs the text that
// tells an agent how to use the UniFi API through the shape layer (issue
// #3476). Control 3 redacts the result of each tool, read included. If a
// context shape matches a command in this text, the agent gets a broken
// command, and can write the broken text back to the file.
//
// The skill is outside the prism subtree, so the nix build sandbox does not
// have it. The test then checks the doc only. secret-redaction.md is not in
// the list: it shows a private-key block on purpose.
func TestContextShapes_LeaveTheUniFiInstructionsUnchanged(t *testing.T) {
	r := payload.NewShapeOnlyRedactor()
	for _, tc := range []struct {
		path     string
		optional bool
	}{
		{"../../docs/invariants/session-lifecycle.md", false},
		{"../../../repo-skills/unifi/SKILL.md", true},
	} {
		b, err := os.ReadFile(tc.path)
		if tc.optional && errors.Is(err, fs.ErrNotExist) {
			t.Logf("%s is not present (expected inside the nix build sandbox)", tc.path)
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if got := r.Redact(string(b)); got != string(b) {
			t.Errorf("the shape layer changes %s: %s", tc.path, mismatch(got, string(b)))
		}
	}
}
