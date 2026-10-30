package container

// review_diff_dir.go — the read of the review diff dir by a review agent
// (issue #3377).
//
// `prism review` writes a diff above the inline limit to a file in the diff
// dir of the parent session, $XDG_STATE_HOME/prism/review-diff/<hash>/, and
// the review prompt names the file. Config.ReviewDiffDir holds that dir for a
// review agent. The agent gets a read of that one dir at its host path:
//
//   - bwrap: a --ro-bind of the dir (bwrapReviewDiffDirBind).
//   - sandbox-exec: a read-only subpath grant (section 5l,
//     reviewDiffDirSBPLAllow).
//
// No sandbox gets a write of the dir or of review-diff/, so no sandbox can
// put a symlink where the host writes. No sandbox gets a read of the dir of
// another session.

import "os"

// bwrapReviewDiffDirBind returns the --ro-bind of dir at its host path. It
// returns nil when dir is empty or is not a dir. A symlink is refused: bwrap
// follows a symlink in the bind source. A missing dir means that no diff
// file was written: the diff is in the prompt.
func bwrapReviewDiffDirBind(dir string) []string {
	if dir == "" {
		return nil
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return nil
	}
	return []string{"--ro-bind", dir, dir}
}

// reviewDiffDirSBPLAllow returns the section-5l clause of generateProfile: a
// read of dir. It has no write operation. It returns "" when dir is empty.
func reviewDiffDirSBPLAllow(dir string) string {
	if dir == "" {
		return ""
	}
	return "(allow file-read* file-test-existence file-read-metadata\n" +
		"  (subpath " + quoteSBPL(dir) + "))\n"
}
