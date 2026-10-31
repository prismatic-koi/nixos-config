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
//
// The first record comes from the session start records in prism.db, not
// from the .bare dirs on disk (#3364). A session can plant a .bare before the
// first record, and no check of the files can tell a plant from a .bare that
// prism made.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/sessionname"
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
	// Start is bareRecordStart in a record that startBareRecord began.
	Start string            `json:"start,omitempty"`
	Repos []bareRecordEntry `json:"repos"`
}

// bareRecordStart marks a record that began from the session start records.
// A record with no mark began from the .bare dirs on disk (#3363), so it can
// hold a plant (#3364). readBareRecord treats it as no record.
const bareRecordStart = "session-starts"

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

// ErrBareInSandbox is returned when CloneWorktree or ConvertToBare runs in a
// sandbox.
var ErrBareInSandbox = errors.New("prism makes a bare repo only on the host. A sandbox cannot write the bare repo record, and host git refuses a .bare that is not in the record. Run the command in a shell on the host")

// inSandbox reports whether prism runs in a sandbox. A sandbox cannot read
// or write the prism state dir, so prism does not use the record there. In a
// sandbox, git runs with the rights of the session, so a .bare that the
// session made gives it nothing more.
func inSandbox() bool {
	return sandboxenv.IsInsideSandbox()
}

// requirePrismBare returns ErrNotPrismBare unless gitDir, with the Lstat
// result info, is in the record. gitDir must be a resolved path.
//
// If the record has an entry for the path of gitDir with another inode, the
// entry is stale, and requirePrismBare removes it. So a session cannot make
// one .bare after another until one gets the recorded inode.
func requirePrismBare(gitDir string, info os.FileInfo) error {
	if allowUnrecordedBare.Load() || inSandbox() {
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
	if filepath.Base(gitDir) != ".bare" {
		return fmt.Errorf("host git: %s: %w", gitDir, ErrNotPrismBare)
	}
	return fmt.Errorf("host git: %s: %w. If you made this bare repo with prism clone or prism convert, and no session ran in a plain clone of this project since then, run \"prism confirm-bare %s\" in a shell on the host",
		gitDir, ErrNotPrismBare, filepath.Dir(gitDir))
}

// SessionRuns reports whether a session with the name runs on the host. It
// returns an error when it cannot tell.
type SessionRuns func(name string) (bool, error)

var sessionRunsSource atomic.Pointer[SessionRuns]

// SetSessionRunsSource sets the SessionRuns that CloneWorktree and
// ConvertToBare use. internal/session sets it in its init, because
// internal/git cannot import internal/session.
func SetSessionRunsSource(runs SessionRuns) {
	sessionRunsSource.Store(&runs)
}

// registeredSessionRuns returns the SessionRuns of SetSessionRunsSource, or
// nil.
func registeredSessionRuns() SessionRuns {
	if runs := sessionRunsSource.Load(); runs != nil {
		return *runs
	}
	return nil
}

// refuseSessionsAtOrAbove returns an error when a session that started in
// root, or in a dir above root, runs. Such a session can write a .bare that
// prism makes or records in root. action starts each error.
//
// A session runs if its start record is open (SessionStart.Open), or if runs
// says so. runs must ask a source that the host controls, such as tmux. A
// session can end its own start record and keep its sandbox: the sidecar
// ends the record when the session sends session_shutdown. If runs is nil or
// cannot tell, refuseSessionsAtOrAbove refuses.
func refuseSessionsAtOrAbove(action string, root projectRoot, runs SessionRuns) error {
	dir, err := bareRecordDir()
	if err != nil {
		return err
	}
	starts, err := readSessionStarts(dir)
	if err != nil {
		return err
	}
	for _, s := range sessionsAtOrAbove(starts, statSessionDirs(starts), root) {
		if s.Open {
			return fmt.Errorf("%s: a session record with no end time names the session %s in %s. A session there can write this root. If the session does not run, end it with \"prism cleanup --yes --session %s\", then run this command again", action, s.Name, s.Worktree, s.Name)
		}
		if runs == nil {
			return fmt.Errorf("%s: no check of the running sessions is set", action)
		}
		running, err := runs(s.Name)
		if err != nil {
			return fmt.Errorf("%s: prism cannot tell if the session %s runs: %v. A session that runs in %s can write this root", action, s.Name, err, s.Worktree)
		}
		if running {
			return fmt.Errorf("%s: the session %s runs, and it started in %s, so it can write this root. Stop the session first", action, s.Name, s.Worktree)
		}
	}
	return nil
}

// refuseSessionsAtOrAboveDir is refuseSessionsAtOrAbove for the dir root,
// with the SessionRuns of SetSessionRunsSource. root must exist.
func refuseSessionsAtOrAboveDir(action, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return refuseSessionsAtOrAbove(action, projectRoot{path: filepath.Clean(abs), resolved: resolved}, registeredSessionRuns())
}

// ErrConfirmInSandbox is returned when ConfirmBare runs in a sandbox.
var ErrConfirmInSandbox = errors.New("prism confirm-bare runs only on the host. A sandbox cannot write the bare repo record. Run the command in a shell on the host")

// ConfirmBare adds the .bare of the project at root to the record, because
// the owner says that prism made it. It is for a .bare that the first record
// did not get (startBareRecord). It returns the git dir that it recorded.
//
// ConfirmBare does the checks of OpenBare, except the record check. It
// refuses a root whose .git is a directory, and a raw bare git dir: prism
// makes neither. It refuses while a session runs in the root or in a dir
// above it, because that session can write the .bare after the confirmation
// (refuseSessionsAtOrAbove).
func ConfirmBare(root string, runs SessionRuns) (string, error) {
	if inSandbox() {
		return "", ErrConfirmInSandbox
	}
	r, err := openBare(root, func(string, os.FileInfo) error { return nil })
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("host git: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("host git: project root: %w", err)
	}
	if r.gitDir != filepath.Join(resolvedRoot, ".bare") {
		return "", fmt.Errorf("host git: confirm %s: the root is a bare git dir. prism does not make a bare git dir as a project root", r.gitDir)
	}
	if g, err := os.Lstat(filepath.Join(resolvedRoot, ".git")); err == nil && g.IsDir() {
		return "", fmt.Errorf("host git: confirm %s: %s/.git is a directory. The root is a plain clone, and prism does not make a .bare next to the .git dir of a plain clone", r.gitDir, resolvedRoot)
	}
	pr := projectRoot{path: filepath.Clean(abs), resolved: resolvedRoot}
	if err := refuseSessionsAtOrAbove("host git: confirm "+r.gitDir, pr, runs); err != nil {
		return "", err
	}
	if err := recordBare(r.gitDir); err != nil {
		return "", err
	}
	return r.gitDir, nil
}

// recordBare adds gitDir, a .bare that prism made, to the record.
func recordBare(gitDir string) error {
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

// loadBareRecord reads the record. If there is no record, or the record has
// no start mark, it starts one (startBareRecord).
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
	rec, err = startBareRecord(dir)
	if err != nil {
		return bareRecordFile{}, err
	}
	if err := writeBareRecord(dir, rec); err != nil {
		return bareRecordFile{}, err
	}
	return rec, nil
}

// SessionStart is the start record of one session in prism.db: the name of
// the session and the dir that prism started it in. prism writes it on the
// host. Open is true when the record has no end time.
type SessionStart struct {
	Name     string
	Worktree string
	Open     bool
}

var sessionStartSource atomic.Pointer[func(dbPath string) ([]SessionStart, error)]

// SetSessionStartSource sets the function that reads the session start
// records from the prism.db at dbPath. internal/db sets it in its init,
// because internal/git cannot import internal/db.
func SetSessionStartSource(read func(dbPath string) ([]SessionStart, error)) {
	sessionStartSource.Store(&read)
}

// readSessionStarts reads the session start records from prism.db in dir,
// the prism state dir.
func readSessionStarts(dir string) ([]SessionStart, error) {
	read := sessionStartSource.Load()
	if read == nil {
		return nil, errors.New("host git: no reader of the session start records is set. internal/db sets it")
	}
	starts, err := (*read)(filepath.Join(dir, "prism.db"))
	if err != nil {
		return nil, fmt.Errorf("host git: read the session start records: %w", err)
	}
	return starts, nil
}

// startBareRecord returns the first record. prism uses it one time, when
// there is no record file with the start mark, so a .bare that prism made
// before the record existed stays usable.
//
// A session that can write a project root can plant a .bare in it, and the
// files of a plant can be the same as the files of a .bare that prism made
// (#3364). So the first record comes from the session start records in
// prism.db. prism writes them on the host, with the dir that prism computed,
// and no sandbox can write them. A .bare in a project root goes in the
// record only if both of these are true:
//
//   - A session started in a worktree of the root. prism made that worktree
//     with the .bare.
//   - No session started in the root or in a dir above it. Such a session
//     can write the root, so it can have planted the .bare. A meta-session
//     runs no agent, so it does not count.
//
// It also skips a root whose .git is a directory. That root is a plain
// clone, and prism does not make a .bare next to the .git dir of a plain
// clone. ConfirmBare adds a .bare that the first record does not get.
func startBareRecord(dir string) (bareRecordFile, error) {
	starts, err := readSessionStarts(dir)
	if err != nil {
		return bareRecordFile{}, err
	}
	infos := statSessionDirs(starts)
	rec := bareRecordFile{Start: bareRecordStart}
	seen := map[string]bool{}
	for _, root := range configuredProjectRoots(config.LoadFresh()) {
		gitDir := filepath.Join(root.resolved, ".bare")
		if seen[gitDir] {
			continue
		}
		seen[gitDir] = true
		info, err := os.Lstat(gitDir)
		if err != nil || !info.IsDir() {
			continue
		}
		if g, err := os.Lstat(filepath.Join(root.resolved, ".git")); err == nil && g.IsDir() {
			continue
		}
		if !worktreeSessionIn(starts, root) || len(sessionsAtOrAbove(starts, infos, root)) > 0 {
			continue
		}
		if ino, ok := inodeOf(info); ok {
			rec.Repos = append(rec.Repos, bareRecordEntry{Path: gitDir, Ino: ino})
		}
	}
	return rec, nil
}

// projectRoot is a project root of config.json: path as config.json gives
// it, and resolved with no symlink.
type projectRoot struct {
	path     string
	resolved string
}

func (r projectRoot) forms() []string {
	if r.path == r.resolved {
		return []string{r.resolved}
	}
	return []string{r.path, r.resolved}
}

// worktreeSessionIn reports whether a session started in a dir below root,
// other than a dir in .bare. The compare is by path only, so a symlink that a
// session made cannot make a dir seem to be below root.
func worktreeSessionIn(starts []SessionStart, root projectRoot) bool {
	for _, s := range starts {
		w := filepath.Clean(s.Worktree)
		if !filepath.IsAbs(w) {
			continue
		}
		for _, form := range root.forms() {
			if first, ok := firstPartBelow(w, form); ok && first != ".bare" {
				return true
			}
		}
	}
	return false
}

// statSessionDirs returns the Stat result of each start dir that exists.
func statSessionDirs(starts []SessionStart) map[string]os.FileInfo {
	infos := map[string]os.FileInfo{}
	for _, s := range starts {
		w := filepath.Clean(s.Worktree)
		if _, done := infos[w]; done || !filepath.IsAbs(w) {
			continue
		}
		if info, err := os.Stat(w); err == nil && info.IsDir() {
			infos[w] = info
		} else {
			infos[w] = nil
		}
	}
	return infos
}

// sessionsAtOrAbove returns each session, other than a meta-session, that
// started in root or in a dir above root. infos is the result of
// statSessionDirs. A session matches by path, or by file identity with root
// or with a dir above it. A match that is too wide refuses a root, and a
// match that is too narrow accepts a plant, so both compares count.
func sessionsAtOrAbove(starts []SessionStart, infos map[string]os.FileInfo, root projectRoot) []SessionStart {
	var above []os.FileInfo
	for p := root.resolved; ; p = filepath.Dir(p) {
		if info, err := os.Stat(p); err == nil {
			above = append(above, info)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	var out []SessionStart
	for _, s := range starts {
		if sessionname.IsMeta(s.Name) {
			continue
		}
		w := filepath.Clean(s.Worktree)
		if !filepath.IsAbs(w) {
			continue
		}
		match := false
		for _, form := range root.forms() {
			if _, below := firstPartBelow(form, w); below || form == w {
				match = true
			}
		}
		if info := infos[w]; info != nil && !match {
			for _, a := range above {
				if os.SameFile(info, a) {
					match = true
					break
				}
			}
		}
		if match {
			out = append(out, s)
		}
	}
	return out
}

// firstPartBelow returns the first part of path below dir. ok is false when
// path is not below dir. Both paths must be absolute and clean.
func firstPartBelow(path, dir string) (first string, ok bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	first, _, _ = strings.Cut(rel, string(filepath.Separator))
	return first, true
}

// configuredProjectRoots returns each project root of cfg that exists: each
// dir in a project location, and each project_specific dir.
func configuredProjectRoots(cfg config.Config) []projectRoot {
	var roots []projectRoot
	add := func(p string) {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return
		}
		if info, err := os.Stat(resolved); err == nil && info.IsDir() {
			roots = append(roots, projectRoot{path: filepath.Clean(p), resolved: resolved})
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
	if rec.Start != bareRecordStart {
		return bareRecordFile{}, fmt.Errorf("host git: %s has no start mark %q: %w", path, bareRecordStart, os.ErrNotExist)
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
