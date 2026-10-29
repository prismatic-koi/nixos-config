package git

// bare_record.go — the record of each .bare that prism made (#3333).
//
// A sandboxed session can write its own root when its project is a plain
// clone. There it can make <root>/.bare with a config that defines a filter
// driver and an info/attributes that selects it. No command-line config and
// no environment variable turns off that driver, and a planted config can
// define a hook that prism cannot turn off (#3349). So on the host, OpenBare
// runs git only in a .bare that prism made.
//
// prism records the path and the inode of each .bare that it makes in a file
// in the prism state dir. No sandbox can write that dir. With the inode, a
// stale entry does not match: a .bare that a session makes at the path of a
// removed project has another inode.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
)

// ErrNotPrismBare is returned when a bare repo is not in the record of the
// bare repos that prism made.
var ErrNotPrismBare = errors.New("prism did not make this bare repo. A sandboxed session can make a .bare in a project root that it can write, so prism does not run git in it")

const (
	bareRecordName = "bare-repos.json"
	bareRecordLock = "bare-repos.lock"
)

type bareRecordEntry struct {
	Path string `json:"path"`
	Ino  uint64 `json:"ino"`
}

type bareRecordFile struct {
	Repos []bareRecordEntry `json:"repos"`
}

// allowUnrecordedBare makes requirePrismBare accept each bare repo. Only
// tests set it, through AllowUnrecordedBareForTest.
var allowUnrecordedBare atomic.Bool

// AllowUnrecordedBareForTest makes OpenBare accept a bare repo that is not in
// the record, for test fixtures that make .bare with plain git. Call restore
// when the test ends.
func AllowUnrecordedBareForTest() (restore func()) {
	prev := allowUnrecordedBare.Swap(true)
	return func() { allowUnrecordedBare.Store(prev) }
}

// bareRecordDir returns the prism state dir, the same dir as prism.db.
func bareRecordDir() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "prism"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("host git: find the prism state dir: HOME and XDG_STATE_HOME are not set")
	}
	return filepath.Join(home, ".local", "state", "prism"), nil
}

// skipBareRecord reports whether prism runs in a sandbox, where it does not
// use the record. A sandbox cannot read the prism state dir. In a sandbox,
// git runs with the rights of the session, so a .bare that the session made
// gives it nothing more.
func skipBareRecord() bool {
	return sandboxenv.IsInsideSandbox()
}

// requirePrismBare returns ErrNotPrismBare unless gitDir, with the Lstat
// result info, is in the record. gitDir must be a resolved path.
//
// If the record has an entry for the path of gitDir with another inode, the
// entry is stale, and requirePrismBare removes it. So a session cannot make
// one .bare after another until one gets the recorded inode.
func requirePrismBare(gitDir string, info os.FileInfo) error {
	if allowUnrecordedBare.Load() || skipBareRecord() {
		return nil
	}
	ino, ok := inodeOf(info)
	if !ok {
		return fmt.Errorf("host git: %s: read the inode: %w", gitDir, ErrNotPrismBare)
	}
	dir, err := bareRecordDir()
	if err != nil {
		return err
	}
	rec, err := loadBareRecord(dir)
	if err != nil {
		return err
	}
	if rec.matches(info, ino) {
		return nil
	}
	if rec.hasStale(info, ino) {
		if err := withBareRecordLock(dir, func() error {
			cur, err := readBareRecord(dir)
			if err != nil {
				return err
			}
			cur.Repos = cur.without(info, ino)
			return writeBareRecord(dir, cur)
		}); err != nil {
			return fmt.Errorf("host git: %s: %w. prism could not remove the stale entry from the record: %v", gitDir, ErrNotPrismBare, err)
		}
	}
	return fmt.Errorf("host git: %s: %w. If you made this bare repo, stop each session that runs in a plain clone. Then remove %s. The next prism command records each .bare in a project root again",
		gitDir, ErrNotPrismBare, filepath.Join(dir, bareRecordName))
}

// recordBare adds gitDir, a .bare that prism made, to the record.
func recordBare(gitDir string) error {
	if skipBareRecord() {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return fmt.Errorf("host git: record %s: %w", gitDir, err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return fmt.Errorf("host git: record %s: %w", gitDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host git: record %s: not a directory", resolved)
	}
	ino, ok := inodeOf(info)
	if !ok {
		return fmt.Errorf("host git: record %s: read the inode", resolved)
	}
	dir, err := bareRecordDir()
	if err != nil {
		return err
	}
	return withBareRecordLock(dir, func() error {
		rec, err := readOrStartBareRecord(dir)
		if err != nil {
			return err
		}
		repos := make([]bareRecordEntry, 0, len(rec.Repos)+1)
		for _, e := range rec.Repos {
			if e.Path != resolved {
				repos = append(repos, e)
			}
		}
		rec.Repos = append(repos, bareRecordEntry{Path: resolved, Ino: ino})
		return writeBareRecord(dir, rec)
	})
}

// forgetBare removes gitDir from the record. Call it before prism removes a
// .bare that it made: a new dir can get the inode of the removed one.
func forgetBare(gitDir string) error {
	if skipBareRecord() {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return fmt.Errorf("host git: forget %s: %w", gitDir, err)
	}
	dir, err := bareRecordDir()
	if err != nil {
		return err
	}
	return withBareRecordLock(dir, func() error {
		rec, err := readBareRecord(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		repos := make([]bareRecordEntry, 0, len(rec.Repos))
		for _, e := range rec.Repos {
			if e.Path != resolved {
				repos = append(repos, e)
			}
		}
		rec.Repos = repos
		return writeBareRecord(dir, rec)
	})
}

// loadBareRecord reads the record. If there is no record, it starts one
// (startBareRecord).
func loadBareRecord(dir string) (bareRecordFile, error) {
	rec, err := readBareRecord(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return rec, err
	}
	err = withBareRecordLock(dir, func() error {
		var lockedErr error
		rec, lockedErr = readOrStartBareRecord(dir)
		return lockedErr
	})
	return rec, err
}

// readOrStartBareRecord reads the record, or starts it when there is none.
// The caller holds the lock.
func readOrStartBareRecord(dir string) (bareRecordFile, error) {
	rec, err := readBareRecord(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return rec, err
	}
	rec = startBareRecord()
	if err := writeBareRecord(dir, rec); err != nil {
		return bareRecordFile{}, err
	}
	return rec, nil
}

// startBareRecord returns a record of each .bare in a project root of
// config.json. prism uses it one time, when there is no record file, so a
// .bare that prism made before the record existed stays usable.
//
// It skips a root whose .git is a directory. That root is a plain clone, and
// prism does not make a .bare next to the .git dir of a plain clone.
func startBareRecord() bareRecordFile {
	var rec bareRecordFile
	seen := map[string]bool{}
	for _, root := range configuredProjectRoots(config.LoadFresh()) {
		gitDir := filepath.Join(root, ".bare")
		if seen[gitDir] {
			continue
		}
		seen[gitDir] = true
		info, err := os.Lstat(gitDir)
		if err != nil || !info.IsDir() {
			continue
		}
		if g, err := os.Lstat(filepath.Join(root, ".git")); err == nil && g.IsDir() {
			continue
		}
		if ino, ok := inodeOf(info); ok {
			rec.Repos = append(rec.Repos, bareRecordEntry{Path: gitDir, Ino: ino})
		}
	}
	return rec
}

// configuredProjectRoots returns each project root of cfg that exists, as a
// resolved path: each dir in a project location, and each project_specific
// dir.
func configuredProjectRoots(cfg config.Config) []string {
	var roots []string
	add := func(p string) {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return
		}
		if info, err := os.Stat(resolved); err == nil && info.IsDir() {
			roots = append(roots, resolved)
		}
	}
	for _, loc := range cfg.ProjectLocations {
		dir := config.ExpandHomePath(loc)
		if !filepath.IsAbs(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			add(filepath.Join(dir, e.Name()))
		}
	}
	for _, spec := range cfg.ProjectSpecific {
		if p := config.ExpandHomePath(spec); filepath.IsAbs(p) {
			add(p)
		}
	}
	return roots
}

func readBareRecord(dir string) (bareRecordFile, error) {
	path := filepath.Join(dir, bareRecordName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bareRecordFile{}, err
		}
		return bareRecordFile{}, fmt.Errorf("host git: read %s: %w", path, err)
	}
	var rec bareRecordFile
	if err := json.Unmarshal(data, &rec); err != nil {
		return bareRecordFile{}, fmt.Errorf("host git: parse %s: %w", path, err)
	}
	return rec, nil
}

// writeBareRecord replaces the record file in one rename, so a reader that
// holds no lock reads the old file or the new file.
func writeBareRecord(dir string, rec bareRecordFile) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, bareRecordName+".*")
	if err != nil {
		return fmt.Errorf("host git: write the bare repo record: %w", err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("host git: write the bare repo record: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, bareRecordName)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("host git: write the bare repo record: %w", err)
	}
	return nil
}

// withBareRecordLock runs fn with an exclusive lock on the record. Each
// change to the record reads it again in fn, so two prism processes do not
// lose an entry.
func withBareRecordLock(dir string, fn func() error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("host git: make %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, bareRecordLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("host git: open the bare repo record lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("host git: lock the bare repo record: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// matches reports whether an entry names the dir of info, with the inode
// ino. The compare is by file identity, so a path with another letter case
// on a case-insensitive volume matches.
func (rec bareRecordFile) matches(info os.FileInfo, ino uint64) bool {
	for _, e := range rec.Repos {
		if e.Ino == ino && sameDirAs(e.Path, info) {
			return true
		}
	}
	return false
}

// hasStale reports whether an entry names the dir of info with an inode
// other than ino.
func (rec bareRecordFile) hasStale(info os.FileInfo, ino uint64) bool {
	for _, e := range rec.Repos {
		if e.Ino != ino && sameDirAs(e.Path, info) {
			return true
		}
	}
	return false
}

// without returns the entries of rec, less each stale entry for the dir of
// info.
func (rec bareRecordFile) without(info os.FileInfo, ino uint64) []bareRecordEntry {
	out := make([]bareRecordEntry, 0, len(rec.Repos))
	for _, e := range rec.Repos {
		if e.Ino != ino && sameDirAs(e.Path, info) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sameDirAs reports whether path names the dir of info now. It does not
// follow a symlink at path.
func sameDirAs(path string, info os.FileInfo) bool {
	cur, err := os.Lstat(path)
	return err == nil && os.SameFile(cur, info)
}

func inodeOf(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Ino), true
}
