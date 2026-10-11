package prismcontainer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

func writeAuditLine(t *testing.T, instanceID, line string) string {
	t.Helper()
	p, err := container.PrismContainerAuditLogPath(instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func labelledFor(id, instanceID string) prismcontainertest.Container {
	return prismcontainertest.Container{ID: id, State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: instanceID}}
}

// TestRemoveAuditDir_KeepsDirUntilSweepSucceeds checks the gate for a
// container and for an image: a failed sweep keeps the dir and the
// resource, and a later call with a working podman removes both.
func TestRemoveAuditDir_KeepsDirUntilSweepSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rm, rmi int
	}{
		{"service container", 125, 0},
		{"built image", 0, 125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			writeAuditLine(t, testInstanceID, "{}\n")
			f := &prismcontainertest.Fake{}
			f.Add(labelledFor("svc", testInstanceID))
			f.AddImage(prismcontainertest.Image{ID: "img", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
			f.SetRmCodes(tc.rm, tc.rmi)

			err := prismcontainer.RemoveAuditDir(context.Background(), f, f, testInstanceID)
			if err == nil || !strings.Contains(err.Error(), "keep the prism-container audit dir of incarnation "+testInstanceID) {
				t.Fatalf("RemoveAuditDir after a failed sweep = %v, want the keep error", err)
			}
			if !container.PrismContainerAuditDirExists(testInstanceID) {
				t.Fatalf("the audit dir was removed although the sweep failed")
			}

			f.SetRmCodes(0, 0)
			if err := prismcontainer.RemoveAuditDir(context.Background(), f, f, testInstanceID); err != nil {
				t.Fatalf("RemoveAuditDir with a working podman = %v", err)
			}
			if container.PrismContainerAuditDirExists(testInstanceID) {
				t.Errorf("the audit dir stays after a successful sweep")
			}
			if len(f.Containers()) != 0 || len(f.Images()) != 0 {
				t.Errorf("containers %+v, images %+v remain after the second sweep", f.Containers(), f.Images())
			}
		})
	}
}

func TestRemoveAuditDir_NoDirNoPodman(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &prismcontainertest.Fake{}
	if err := prismcontainer.RemoveAuditDir(context.Background(), f, f, testInstanceID); err != nil {
		t.Fatalf("RemoveAuditDir = %v", err)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("podman calls with no audit dir: %q", calls)
	}
}

// TestAppendAuditLog_MovesLinesKeepsDir checks that the lines move once:
// a second append after a kept dir copies nothing.
func TestAppendAuditLog_MovesLinesKeepsDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const to = "11111111-2222-4333-8444-555555555555"
	writeAuditLine(t, testInstanceID, "line-a\n")
	for i := 0; i < 2; i++ {
		if err := prismcontainer.AppendAuditLog(testInstanceID, to); err != nil {
			t.Fatalf("AppendAuditLog #%d: %v", i, err)
		}
	}
	dst, _ := container.PrismContainerAuditLogPath(to)
	if got, err := os.ReadFile(dst); err != nil || string(got) != "line-a\n" {
		t.Errorf("destination log = %q (%v), want the line once", got, err)
	}
	if !container.PrismContainerAuditDirExists(testInstanceID) {
		t.Errorf("AppendAuditLog removed the source dir")
	}
}
