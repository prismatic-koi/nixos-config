//go:build darwin

package integration_test

// sandbox_exec_review_diff_darwin_test.go — the section-5l read of the review
// diff dir of generateProfile (#3377).
//
// `prism review` writes a diff above the inline limit to
// $XDG_STATE_HOME/prism/review-diff/<hash of the parent>/. A review agent of
// that parent gets a read of that dir and no write. It cannot read the diff
// dir of another session. Each positive test has a paired negative that
// mutates the profile and gets the opposite result.
//
// The tests put XDG_STATE_HOME under the real home (stateHomeOutsideTmp):
// the profile grants a read of the user $TMPDIR, and a diff dir there makes
// a read test vacuous. The Manager has no BareRoot, so no ancestor allow
// covers the state dir.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/session"
)

const (
	reviewDiffParent      = "prism-test@3377-parent"
	reviewDiffOtherParent = "prism-test@3377-other"
	reviewDiffPayload     = "diff --git a/x b/x\n+review diff payload\n"
)

type reviewDiffFixture struct {
	m         *container.Manager
	dir       string
	file      string
	otherFile string
}

// newReviewDiffFixture writes a diff file for the parent and one for another
// parent with the production writer, and returns the profile Manager of a
// review agent of the parent.
func newReviewDiffFixture(t *testing.T) reviewDiffFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	write := func(parent string) (string, string) {
		dir, err := review.DiffDirForParent(parent)
		if err != nil {
			t.Fatalf("DiffDirForParent(%q): %v", parent, err)
		}
		file := filepath.Join(dir, "pr-7-round-1.diff")
		if err := rundir.WriteFile(file, []byte(reviewDiffPayload)); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		return dir, file
	}
	dir, file := write(reviewDiffParent)
	_, otherFile := write(reviewDiffOtherParent)
	m := container.New(container.Config{
		SessionName:      reviewDiffParent + "~review-1-review-code",
		InstanceID:       "integ-3377-" + session.SessionDirName(t.Name()),
		Worktree:         t.TempDir(),
		WorktreeReadOnly: true,
		ReviewDiffDir:    dir,
		GitUserName:      "test-user",
		GitUserEmail:     "test@example.com",
	})
	return reviewDiffFixture{m: m, dir: dir, file: file, otherFile: otherFile}
}

// reviewDiffAllowBlock is the section-5l clause for dir, with the operations
// ops. withMutatedProbeProfile fails when the text is not in the profile, so
// a copy that drifts from the generator fails the negative tests.
func reviewDiffAllowBlock(dir, ops string) string {
	return "(allow " + ops + "\n  (subpath " + sbplQuoteForTest(dir) + "))\n"
}

const reviewDiffReadOps = "file-read* file-test-existence file-read-metadata"

func requireDiffFileUnchanged(t *testing.T, file string) {
	t.Helper()
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the diff file %s is not a regular file: %v %v", file, info, err)
	}
	if data, _ := os.ReadFile(file); string(data) != reviewDiffPayload {
		t.Errorf("the diff file %s changed: %q", file, data)
	}
}

// TestSandboxExecReviewDiff_ReadUnderProductionProfile: a review agent reads
// the diff file of its parent.
func TestSandboxExecReviewDiff_ReadUnderProductionProfile(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newReviewDiffFixture(t)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)
	expectOp(t, profilePath, probe, true, "the review agent cannot read the diff file of its parent", "--read", fx.file)
}

// TestSandboxExecReviewDiff_ReadDeniedWithoutGrant is the paired negative:
// with the section-5l rule pointed at a missing path, the read fails.
func TestSandboxExecReviewDiff_ReadDeniedWithoutGrant(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newReviewDiffFixture(t)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.Replace(p, reviewDiffAllowBlock(fx.dir, reviewDiffReadOps), reviewDiffAllowBlock(fx.dir+".prism-3377-disabled", reviewDiffReadOps), 1)
	})
	expectOp(t, mutatedPath, probe, false, "the read still works without the section-5l grant, so the positive test proves nothing", "--read", fx.file)
}

// TestSandboxExecReviewDiff_WriteDenied: the review agent cannot change the
// diff file, remove or rename it, or put a file or a symlink in the dir.
func TestSandboxExecReviewDiff_WriteDenied(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newReviewDiffFixture(t)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)

	planted := filepath.Join(fx.dir, "planted")
	expectOp(t, profilePath, probe, false, "the review agent overwrote the diff file", "--create", fx.file)
	expectOp(t, profilePath, probe, false, "the review agent removed the diff file", "--remove", fx.file)
	expectOp(t, profilePath, probe, false, "the review agent renamed the diff file", "--rename", fx.file, fx.file+".2")
	expectOp(t, profilePath, probe, false, "the review agent made a file in the diff dir", "--create", planted)
	expectOp(t, profilePath, probe, false, "the review agent made a symlink in the diff dir", "--symlink", "/etc/passwd", planted+".link")
	requireDiffFileUnchanged(t, fx.file)
	for _, p := range []string{planted, planted + ".link", fx.file + ".2"} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after the denied writes (err=%v)", p, err)
		}
	}
}

// TestSandboxExecReviewDiff_WritableWithWriteGrant is the paired negative:
// with file-write* added to the section-5l rule, the create in the dir works.
func TestSandboxExecReviewDiff_WritableWithWriteGrant(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newReviewDiffFixture(t)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.Replace(p, reviewDiffAllowBlock(fx.dir, reviewDiffReadOps), reviewDiffAllowBlock(fx.dir, "file-read* file-write* file-test-existence file-read-metadata"), 1)
	})
	expectOp(t, mutatedPath, probe, true, "the create still fails with a write grant, so the positive test proves nothing", "--create", filepath.Join(fx.dir, "planted"))
}

// TestSandboxExecReviewDiff_OtherSessionDenied: the review agent cannot read
// the diff file of another parent.
func TestSandboxExecReviewDiff_OtherSessionDenied(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newReviewDiffFixture(t)
	prepared, _ := preparePositiveProfile(t, fx.m)
	profilePath := writeProbePositiveProfile(t, prepared)
	expectOp(t, profilePath, probe, false, "the review agent read the diff file of another session", "--read", fx.otherFile)
}

// TestSandboxExecReviewDiff_OtherSessionReadableWithWideGrant is the paired
// negative: with the section-5l rule on the whole review-diff dir, the read
// of the diff file of the other parent works.
func TestSandboxExecReviewDiff_OtherSessionReadableWithWideGrant(t *testing.T) {
	requireSandboxExec(t)
	probe := buildConnectProbe(t)
	fx := newReviewDiffFixture(t)
	mutatedPath := withMutatedProbeProfile(t, fx.m, func(p string) string {
		return strings.Replace(p, reviewDiffAllowBlock(fx.dir, reviewDiffReadOps), reviewDiffAllowBlock(filepath.Dir(fx.dir), reviewDiffReadOps), 1)
	})
	expectOp(t, mutatedPath, probe, true, "the read of the other diff file fails with a wide grant, so the positive test proves nothing", "--read", fx.otherFile)
}
