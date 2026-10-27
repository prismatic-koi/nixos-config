package container

// pi_shared_state.go — the host dirs that pi sessions of every isolation
// mode share: a lock-only dir and the Notion MCP store (#3289, #3290).
//
// The pi agent dir in a bwrap sandbox is a private dir (#3284). A lock dir
// that pi or an extension makes in it is not seen by other sessions,
// and a rename onto a file bind fails with EBUSY. The state below must be
// the same for every session on the host, so it lives in two dedicated
// dirs. bwrap binds each dir read-write, and the sandbox-exec profile
// grants each dir (section 6c):
//
//   - <home>/.local/state/prism/pi-locks/ holds lock dirs only. The
//     anthropic-oauth extension takes anthropic-oauth-refresh.lock in it
//     around each OAuth token refresh.
//   - <home>/.local/state/prism/pi-notion/ holds the Notion MCP store only:
//     notion-mcp-oauth.json, notion-mcp-client.json, the lock dir
//     notion-mcp-oauth.json.lock, and the .notion-mcp.tmp.* files that the
//     extension renames onto the store files.
//
// No pi session loads configuration, code, or prompts from these dirs. pi
// does not know them. The two extensions read only lock dirs and the JSON
// store from them.
//
// Every sandbox can write these dirs, and this code runs on the host. So
// it does not follow a symlink when it writes a lock owner file, and it
// does not block on a read: a planted symlink or FIFO must not let a
// sandbox change a host file or stop prism agent-run.
//
// The paths come from $HOME, not from $XDG_STATE_HOME. All three isolators
// run pi with HOME set to the host home dir. bwrap clears XDG_STATE_HOME,
// and sandbox-exec sets it to <home>/.local/state, so a path from
// XDG_STATE_HOME can differ between the isolators. The extensions compute
// the same paths: anthropic-oauth/refresh-lock.ts and notion/auth.ts.
// TestPISharedStateDirs_ExtensionsUseSamePaths checks that.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	piLocksDirName  = "pi-locks"
	piNotionDirName = "pi-notion"

	// piNotionStoreFileLimit is the largest Notion store file that the move
	// reads. A real file is less than 1 KiB.
	piNotionStoreFileLimit = 64 << 10

	// piNotionLockStale is the age after which a Notion store lock dir is
	// stale. It is DEFAULT_LOCK_STALE_MS of notion/auth.ts.
	piNotionLockStale = 30 * time.Second

	// piNotionLockOwnerLimit is the largest lock owner file that prism reads.
	piNotionLockOwnerLimit = 4 << 10
)

// PISharedStateDirs returns the host paths of the lock dir and of the
// Notion store dir for home. Both are "" when home is "".
func PISharedStateDirs(home string) (lockDir, notionDir string) {
	if home == "" {
		return "", ""
	}
	base := filepath.Join(home, ".local", "state", "prism")
	return filepath.Join(base, piLocksDirName), filepath.Join(base, piNotionDirName)
}

// EnsurePISharedStateDirs makes the two shared dirs on the host with mode
// 0700, moves the Notion store from the host pi agent dir agentDir into its
// dir, and returns the two paths. prism agent-run calls it before a bwrap
// or sandbox-exec pi session starts (populatePIConfig).
//
// It returns "" for a dir that it cannot make. The sandbox then gets no
// bind or grant for that dir. A failure is logged, and the session still
// starts.
func EnsurePISharedStateDirs(agentDir string) (lockDir, notionDir string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		log.Printf("container: pi: no home dir for the shared state dirs: %v", err)
		return "", ""
	}
	lockDir, notionDir = PISharedStateDirs(home)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		log.Printf("container: pi: create lock dir %s: %v", lockDir, err)
		lockDir = ""
	}
	if err := os.MkdirAll(notionDir, 0o700); err != nil {
		log.Printf("container: pi: create Notion store dir %s: %v", notionDir, err)
		return lockDir, ""
	}
	if agentDir != "" {
		migratePINotionStore(agentDir, notionDir)
	}
	return lockDir, notionDir
}

// migratePINotionStore moves the Notion store files from agentDir, their
// old place, to notionDir. For each file:
//
//   - If agentDir has no such file, nothing changes.
//   - If notionDir already has the file, the file in agentDir is removed.
//     The file in notionDir is the one in use. The old file is a copy, or
//     it holds a refresh token that Notion rotated away. A replay of such a
//     token can make Notion revoke the grant.
//   - Else the file is copied to notionDir with mode 0600 and removed from
//     agentDir.
//
// The move holds the old lock dir and the new lock dir of the store, with
// the lock format of notion/auth.ts. A pi that runs the old extension code
// takes the old lock dir around each refresh, so the move cannot copy a
// token that such a pi is about to rotate. If a lock is held, the move does
// not wait: it stops, and the next session start tries again. Until then
// the extension reads the old path, and does not refresh from it.
//
// The move reads only a regular file of at most piNotionStoreFileLimit
// bytes that holds one JSON object. It does not follow a symlink: a sandbox
// with the old profile could write these names, and a symlink there must
// not copy a host file into a dir that every sandbox can read.
func migratePINotionStore(agentDir, notionDir string) {
	present := false
	for _, name := range piNotionStoreFiles {
		if _, err := os.Lstat(filepath.Join(agentDir, name)); err == nil {
			present = true
		}
	}
	if !present {
		return
	}

	oldLock, err := takePINotionLock(filepath.Join(agentDir, piNotionLockDirName))
	if err != nil {
		log.Printf("container: pi: Notion store move deferred: %v", err)
		return
	}
	defer oldLock.release()
	newLock, err := takePINotionLock(filepath.Join(notionDir, piNotionLockDirName))
	if err != nil {
		log.Printf("container: pi: Notion store move deferred: %v", err)
		return
	}
	defer newLock.release()

	for _, name := range piNotionStoreFiles {
		if err := movePINotionStoreFile(filepath.Join(agentDir, name), filepath.Join(notionDir, name)); err != nil {
			log.Printf("container: pi: move Notion store file %s: %v", name, err)
		}
	}
}

func movePINotionStoreFile(from, to string) error {
	fi, err := os.Lstat(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file, so it stays in place", from)
	}
	if _, err := os.Lstat(to); err == nil {
		return os.Remove(from)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	data, err := readPINotionStoreFile(from)
	if err != nil {
		return err
	}
	if err := writePINotionStoreFile(to, data); err != nil {
		return err
	}
	return os.Remove(from)
}

// readPINotionStoreFile reads path without a follow of a symlink, and
// returns its content if it is one JSON object.
func readPINotionStoreFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, piNotionStoreFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > piNotionStoreFileLimit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, piNotionStoreFileLimit)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s is not one JSON object", path)
	}
	return data, nil
}

// writePINotionStoreFile writes data to path with mode 0600: a temp file
// in the same dir, then a rename, as the extension does.
func writePINotionStoreFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), piNotionTmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmpName, path)
	}
	if werr != nil {
		_ = os.Remove(tmpName)
	}
	return werr
}

// piNotionLock is a lock dir of the Notion store, in the format of
// acquireTokenLock in notion/auth.ts: a dir made with mkdir, with a file
// "owner" that holds {"id", "pid", "ts"}.
type piNotionLock struct {
	dir string
	id  string
}

type piNotionLockOwner struct {
	ID  string `json:"id"`
	PID int    `json:"pid"`
	TS  int64  `json:"ts"`
}

// piNotionLockOwnerRead is the owner file as readLockOwner in
// notion/auth.ts accepts it: id must be a string and ts a number.
type piNotionLockOwnerRead struct {
	ID *string  `json:"id"`
	TS *float64 `json:"ts"`
}

// takePINotionLock takes the lock dir at dir and does not wait. It removes
// a stale lock dir first, with the rule of notion/auth.ts: the age comes
// from the ts of the owner file, or from the mtime of the dir when the
// owner file cannot be read.
func takePINotionLock(dir string) (*piNotionLock, error) {
	var b [6]byte
	_, _ = rand.Read(b[:])
	id := fmt.Sprintf("prism-%d-%s", os.Getpid(), hex.EncodeToString(b[:]))

	for attempt := 0; attempt < 2; attempt++ {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			if werr := writePINotionLockOwner(dir, id); werr != nil {
				// An owner file that exists right after the mkdir is not
				// ours: another process changed the dir. Leave it.
				if !errors.Is(werr, fs.ErrExist) {
					_ = os.RemoveAll(dir)
				}
				return nil, fmt.Errorf("write owner of lock %s: %w", dir, werr)
			}
			return &piNotionLock{dir: dir, id: id}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("take lock %s: %w", dir, err)
		}
		ownerID, age, ok := inspectPINotionLock(dir)
		if !ok {
			continue
		}
		if age <= piNotionLockStale {
			return nil, fmt.Errorf("lock %s is held", dir)
		}
		if readPINotionLockOwnerID(dir) == ownerID {
			_ = os.RemoveAll(dir)
		}
	}
	return nil, fmt.Errorf("lock %s is held", dir)
}

// writePINotionLockOwner creates the owner file of the lock dir with
// O_EXCL and O_NOFOLLOW. A sandbox can swap in a dir with an "owner"
// symlink after the mkdir. The open then fails with EEXIST, and no existing
// file changes.
func writePINotionLockOwner(dir, id string) error {
	owner, _ := json.Marshal(piNotionLockOwner{ID: id, PID: os.Getpid(), TS: time.Now().UnixMilli()})
	f, err := os.OpenFile(filepath.Join(dir, "owner"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(owner)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func (l *piNotionLock) release() {
	if readPINotionLockOwnerID(l.dir) == l.id {
		_ = os.RemoveAll(l.dir)
	}
}

// inspectPINotionLock returns the owner id ("" when the owner file cannot
// be read) and the age of the lock dir. ok is false when the dir is gone.
func inspectPINotionLock(dir string) (ownerID string, age time.Duration, ok bool) {
	if id, ts, err := readPINotionLockOwner(dir); err == nil {
		return id, time.Since(time.UnixMilli(ts)), true
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", 0, false
	}
	return "", time.Since(fi.ModTime()), true
}

func readPINotionLockOwnerID(dir string) string {
	id, _, err := readPINotionLockOwner(dir)
	if err != nil {
		return ""
	}
	return id
}

// readPINotionLockOwner reads the owner file of the lock dir. It opens the
// file with O_NOFOLLOW and O_NONBLOCK and reads only a regular file, so a
// symlink or a FIFO at that name gives an error and does not block.
func readPINotionLockOwner(dir string) (id string, tsMillis int64, err error) {
	f, err := os.OpenFile(filepath.Join(dir, "owner"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if fi, serr := f.Stat(); serr != nil || !fi.Mode().IsRegular() {
		return "", 0, errors.New("owner is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, piNotionLockOwnerLimit))
	if err != nil {
		return "", 0, err
	}
	var owner piNotionLockOwnerRead
	if err := json.Unmarshal(data, &owner); err != nil {
		return "", 0, err
	}
	if owner.ID == nil || owner.TS == nil {
		return "", 0, errors.New("owner has no id or no ts")
	}
	return *owner.ID, int64(*owner.TS), nil
}
