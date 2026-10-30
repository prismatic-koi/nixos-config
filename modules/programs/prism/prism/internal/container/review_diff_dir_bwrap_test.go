//go:build linux

package container

// review_diff_dir_bwrap_test.go — Linux live coverage of the read-only bind
// of the review diff dir (issue #3377). The sibling unit tests assert the
// argv. These tests run a real bwrap with the production emitter
// (bwrapReviewDiffDirBind) and no bind of the host root, so the bind under
// test is the only route to the dir:
//
//   - a review agent reads the diff file of its parent;
//   - it cannot change the file or make a file in the dir;
//   - it cannot read the diff dir of another session;
//   - without the bind, the read fails (the paired negative).
//
// The tests skip where bwrap cannot run (requireUsableBwrapForUsageTest).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reviewDiffPayload = "diff --git a/x b/x\n+review diff payload\n"

// reviewDiffBwrapFixture returns the own diff dir with a diff file, and the
// diff file of another session in a sibling dir.
func reviewDiffBwrapFixture(t *testing.T) (dir, file, otherFile string) {
	t.Helper()
	dir = reviewDiffDirForTest(t, "own")
	file = filepath.Join(dir, "pr-7-round-1.diff")
	otherDir := filepath.Join(filepath.Dir(dir), "other")
	otherFile = filepath.Join(otherDir, "pr-7-round-1.diff")
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{file, otherFile} {
		if err := os.WriteFile(f, []byte(reviewDiffPayload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, file, otherFile
}

func TestBwrapReviewDiffDir_ReadableInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	dir, file, _ := reviewDiffBwrapFixture(t)
	out, err := runInBwrap(t, bwrapBin, bwrapReviewDiffDirBind(dir), "cat "+shQuoteForTest(file))
	if err != nil {
		t.Fatalf("read of the diff file in the sandbox failed: %v — %s", err, out)
	}
	if !strings.Contains(out, "+review diff payload") {
		t.Errorf("diff file content not visible in the sandbox: %q", out)
	}
}

func TestBwrapReviewDiffDir_UnreadableWithoutBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	_, file, _ := reviewDiffBwrapFixture(t)
	out, err := runInBwrap(t, bwrapBin, nil, "cat "+shQuoteForTest(file))
	if err == nil || strings.Contains(out, "+review diff payload") {
		t.Errorf("read of %s worked with no bind, so the positive test proves nothing: %v — %s", file, err, out)
	}
}

func TestBwrapReviewDiffDir_WriteDeniedInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	dir, file, _ := reviewDiffBwrapFixture(t)
	mounts := bwrapReviewDiffDirBind(dir)

	if out, err := runInBwrap(t, bwrapBin, mounts, "echo forged > "+shQuoteForTest(file)); err == nil {
		t.Errorf("the sandbox overwrote the diff file: %s", out)
	}
	planted := filepath.Join(dir, "planted")
	if out, err := runInBwrap(t, bwrapBin, mounts, "ln -s /etc/passwd "+shQuoteForTest(planted)); err == nil {
		t.Errorf("the sandbox made a symlink in the diff dir: %s", out)
	}
	if _, err := os.Lstat(planted); !os.IsNotExist(err) {
		t.Errorf("%s exists on the host (err=%v)", planted, err)
	}
	if data, _ := os.ReadFile(file); string(data) != reviewDiffPayload {
		t.Errorf("the diff file changed: %q", data)
	}
}

// TestBwrapReviewDiffDir_OtherSessionUnreadable: the bind of the own diff
// dir does not expose the diff dir of another session.
func TestBwrapReviewDiffDir_OtherSessionUnreadable(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	dir, _, otherFile := reviewDiffBwrapFixture(t)
	out, err := runInBwrap(t, bwrapBin, bwrapReviewDiffDirBind(dir), "cat "+shQuoteForTest(otherFile))
	if err == nil || strings.Contains(out, "+review diff payload") {
		t.Errorf("the sandbox read the diff file of another session: %v — %s", err, out)
	}
}

// TestBwrapReviewDiffDir_OtherSessionReadableWithWideBind is the paired
// negative: with a bind of the whole review-diff dir, the same read works.
func TestBwrapReviewDiffDir_OtherSessionReadableWithWideBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	dir, _, otherFile := reviewDiffBwrapFixture(t)
	wide := filepath.Dir(dir)
	out, err := runInBwrap(t, bwrapBin, []string{"--ro-bind", wide, wide}, "cat "+shQuoteForTest(otherFile))
	if err != nil || !strings.Contains(out, "+review diff payload") {
		t.Errorf("the read of the other diff file failed with a wide bind, so the positive test proves nothing: %v — %s", err, out)
	}
}
