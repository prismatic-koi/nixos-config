package prismcontainer_test

// Tests for the read-only worktree refusal (#3079): `prism container run`
// and `start` refuse --mount rw for a session with a read-only worktree,
// before podman runs.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// readOnlyRefused reports whether res is the read-only worktree refusal of
// caller c.
func readOnlyRefused(c prismcontainer.Caller, code int, msg string) bool {
	return code == prismcontainer.ExitRefused && strings.HasPrefix(msg, "refused: ") &&
		strings.Contains(msg, "read-only") && strings.Contains(msg, c.Worktree)
}

func TestReadOnlyWorktree_MountRWRefusedBeforePodman(t *testing.T) {
	commands := map[string]func(prismcontainer.Caller, *prismcontainertest.Fake) (int, string){
		"run": func(c prismcontainer.Caller, f *prismcontainertest.Fake) (int, string) {
			res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine", Mount: prismcontainer.MountRW})
			return res.ExitCode, res.Message
		},
		"start": func(c prismcontainer.Caller, f *prismcontainertest.Fake) (int, string) {
			res := prismcontainer.Start(context.Background(), deps(f), c, prismcontainer.StartRequest{Image: "alpine", Mount: prismcontainer.MountRW})
			return res.ExitCode, res.Message
		},
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			c := newCaller(t)
			c.WorktreeReadOnly = true
			f := &prismcontainertest.Fake{ImagePresent: true}
			code, msg := command(c, f)
			if !readOnlyRefused(c, code, msg) {
				t.Errorf("result = (%d, %q), want a refusal that names the read-only worktree %s", code, msg, c.Worktree)
			}
			if calls := f.Calls(); len(calls) != 0 {
				t.Errorf("podman ran before the refusal: %q", calls)
			}
			lines := readAudit(t, testInstanceID)
			if len(lines) != 1 || lines[0]["decision"] != prismcontainer.DecisionRefused {
				t.Errorf("audit = %v, want one refused line", lines)
			}
		})
	}
}

// TestReadOnlyWorktree_MountROAllowed: a read-only session can still mount
// its worktree read-only.
func TestReadOnlyWorktree_MountROAllowed(t *testing.T) {
	c := newCaller(t)
	c.WorktreeReadOnly = true
	f := &prismcontainertest.Fake{ImagePresent: true}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine", Mount: prismcontainer.MountRO})
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v, want the run to start", res)
	}
	if vols := optionValue(f.RunCalls()[0], "--volume"); !slices.Equal(vols, []string{c.Worktree + ":/workspace:ro"}) {
		t.Errorf("volumes %q, want [%s:/workspace:ro]", vols, c.Worktree)
	}
}

// TestWorkerWorktree_MountRWAllowed: the refusal applies only to a
// read-only session.
func TestWorkerWorktree_MountRWAllowed(t *testing.T) {
	c := newCaller(t)
	f := &prismcontainertest.Fake{ImagePresent: true}
	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine", Mount: prismcontainer.MountRW})
	if res.ExitCode != 0 {
		t.Fatalf("result = %+v, want the run to start", res)
	}
	if vols := optionValue(f.RunCalls()[0], "--volume"); !slices.Equal(vols, []string{c.Worktree + ":/workspace:rw"}) {
		t.Errorf("volumes %q, want [%s:/workspace:rw]", vols, c.Worktree)
	}
}
