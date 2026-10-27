package db

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// AC (#3266): spawnOutcomeCascadeInstanceIDs gives each incarnation one
// time, so that WriteSpawnOutcomeCascade writes it one time.
func TestSpawnOutcomeCascadeInstanceIDs_3266(t *testing.T) {
	const (
		parent    = "prism-test@cascade-ids"
		child     = parent + "~review-1-review-goal"
		parentIID = "11111111-2222-4333-8444-555555555555"
		childIID  = "22222222-3333-4444-8555-666666666666"
	)
	cases := []struct {
		name             string
		clearParent      bool
		parentInstanceID string
		want             []string
	}{
		{"hook has not run", false, parentIID, []string{childIID, parentIID}},
		{"hook cleared the parent", true, parentIID, []string{childIID, parentIID}},
		{"no parent instance ID", true, "", []string{childIID}},
		{"hook has not run, no parent instance ID", false, "", []string{childIID, parentIID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { d.Close() })
			for name, iid := range map[string]string{parent: parentIID, child: childIID} {
				if err := d.UpsertStatus(name, "prism-test", "/wt", "active", nil, nil); err != nil {
					t.Fatalf("UpsertStatus %s: %v", name, err)
				}
				if err := d.SetInstanceID(name, iid); err != nil {
					t.Fatalf("SetInstanceID %s: %v", name, err)
				}
				if err := d.InsertSession(Session{InstanceID: iid, SessionName: name, Repo: "prism-test", Worktree: "/wt", Harness: "pi", StartedAt: time.Now()}); err != nil {
					t.Fatalf("InsertSession %s: %v", name, err)
				}
			}
			if c.clearParent {
				if err := d.ClearInstanceID(parent); err != nil {
					t.Fatalf("ClearInstanceID: %v", err)
				}
			}

			got, err := d.spawnOutcomeCascadeInstanceIDs(parent, c.parentInstanceID)
			if err != nil {
				t.Fatalf("spawnOutcomeCascadeInstanceIDs: %v", err)
			}
			slices.Sort(got)
			want := slices.Clone(c.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("instance IDs = %v, want %v", got, want)
			}
		})
	}
}
