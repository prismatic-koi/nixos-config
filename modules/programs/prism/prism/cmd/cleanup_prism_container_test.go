package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	"github.com/prismatic-koi/prism/internal/review"
)

func writeFakePrismContainerAuditLog(t *testing.T, instanceID, session string) string {
	t.Helper()
	p, err := container.PrismContainerAuditLogPath(instanceID)
	if err != nil {
		t.Fatalf("PrismContainerAuditLogPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir audit dir: %v", err)
	}
	content := `{"time":"2026-10-10T00:00:00Z","session":"` + session + `","instance_id":"` + instanceID + `","command":"run","image":"docker.io/library/alpine","decision":"allowed","exit_code":0}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write audit log: %v", err)
	}
	return content
}

func labelled(id, instanceID, state string) prismcontainertest.Container {
	return prismcontainertest.Container{ID: id, State: state, Labels: map[string]string{prismcontainer.LabelInstanceID: instanceID}}
}

// TestHeadlessCleanup_PrismContainer_SweepsEveryIncarnation checks that
// cleanup removes the labelled containers of the current incarnation and of
// an earlier one, leaves another session's container, archives the audit
// log, and then removes the audit dir.
func TestHeadlessCleanup_PrismContainer_SweepsEveryIncarnation(t *testing.T) {
	const earlierIID = "22222222-3333-4444-8555-666666666666"
	const otherIID = "99999999-3333-4444-8555-666666666666"
	f := setupArchiveOrderFixture(t, "prism-container-sweep", "")
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: earlierIID, SessionName: f.session, Repo: archiveOrderRepo,
		Worktree: f.worktree, Harness: "pi", StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession earlier: %v", err)
	}
	d.Close()
	const childIID = "33333333-3333-4444-8555-666666666666"
	child := f.session + "~review-1-review-qa"
	d, err = db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(child, archiveOrderRepo, f.worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus child: %v", err)
	}
	if err := d.SetInstanceID(child, childIID); err != nil {
		t.Fatalf("SetInstanceID child: %v", err)
	}
	d.Close()
	currentContent := writeFakePrismContainerAuditLog(t, archiveOrderIID, "current")
	earlierContent := writeFakePrismContainerAuditLog(t, earlierIID, "earlier")
	childContent := writeFakePrismContainerAuditLog(t, childIID, "child")

	fake := &prismcontainertest.Fake{}
	fake.Add(labelled("current", archiveOrderIID, "running"))
	fake.Add(labelled("earlier", earlierIID, "exited"))
	fake.Add(labelled("child", childIID, "running"))
	fake.Add(labelled("other", otherIID, "running"))
	installPrismContainerFake(t, fake)
	t.Cleanup(review.SetChildPrismContainerRunnerForTest(fake))

	if err := headlessCleanup(f.session, "prism-container-sweep", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	var left []string
	for _, c := range fake.Containers() {
		left = append(left, c.ID)
	}
	if !slices.Equal(left, []string{"other"}) {
		t.Errorf("containers left = %v, want [other]", left)
	}

	archiveDir := assertTranscriptArchived(t, f)
	got, err := os.ReadFile(filepath.Join(archiveDir, "prism-container-audit.log"))
	if err != nil {
		t.Fatalf("archive has no prism-container-audit.log: %v", err)
	}
	for name, line := range map[string]string{"current": currentContent, "earlier": earlierContent, "review child": childContent} {
		if !strings.Contains(string(got), line) {
			t.Errorf("archived prism-container-audit.log lacks the %s incarnation line %q:\n%s", name, line, got)
		}
	}
	for _, id := range []string{archiveOrderIID, earlierIID, childIID} {
		if container.PrismContainerAuditDirExists(id) {
			t.Errorf("prism-container audit dir of %s still exists after cleanup", id)
		}
	}
}

// TestHeadlessCleanup_PrismContainer_UnusedIssuesNoPodman checks that a
// session with no prism-container audit dir causes no podman command.
func TestHeadlessCleanup_PrismContainer_UnusedIssuesNoPodman(t *testing.T) {
	f := setupArchiveOrderFixture(t, "prism-container-unused", "")
	fake := &prismcontainertest.Fake{}
	installPrismContainerFake(t, fake)

	if err := headlessCleanup(f.session, "prism-container-unused", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("podman called for a session that never used prism container: %q", calls)
	}
}
