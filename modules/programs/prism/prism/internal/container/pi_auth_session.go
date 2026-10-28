package container

// pi_auth_session.go — the auth.json of each sandboxed pi session, and the
// host-side sync of its OAuth entries (#3295).
//
// pi 1.4 runs the key of an api_key entry in auth.json that starts with "!"
// as a shell command. Every later pi reads the host ~/.pi/agent/auth.json:
// host-mode sessions, coordinators, and a pi that the user starts in a
// terminal. So a bwrap or sandbox-exec session must not write that file.
// Each sandboxed session gets its own auth.json in a host dir:
//
//	<state>/prism/pi-auth/<instance id>/   mode 0700, host only
//	  sync.lock                           the flock of the sync
//	  sync-state.json                     the hashes of the sync, no token
//	  agent/                              mode 0700, the sandbox can write it
//	    auth.json                         the auth file of the session
//
// prism writes agent/auth.json from the host file before pi starts
// (PreparePIAuthSession). It leaves out each entry that holds a string that
// starts with "!". bwrap binds agent/ into the sandbox, and the auth.json of
// the sandbox agent dir is a symlink into it (pi_agent_dir.go). sandbox-exec
// uses agent/ as the pi agent dir, with a symlink to each other entry of
// the host dir (LinkPIAgentDirEntries). The sandbox cannot write the host
// file.
//
// SyncPIAuthSession copies changes between the two files. The sidecar runs
// it on a timer and when the anthropic-oauth extension asks for it under the
// shared refresh lock (internal/sidecar/pi_auth_sync.go):
//
//   - From the session file to the host file: only a type "oauth" entry with
//     a string access, a string refresh, and a finite number expires, and
//     only these fields. The value must be new: no earlier sync of the
//     session wrote it or read it. pi writes back an old snapshot of the file
//     after a refresh, and a value from that snapshot must not undo a newer
//     host value. If the host entry changed too, the host value wins.
//   - From the host file to the session file: the whole host file, less each
//     entry that holds a string that starts with "!".
//
// The session file is hostile. The sync reads it with O_NOFOLLOW and
// O_NONBLOCK, only as a regular file of at most piAuthSessionFileLimit bytes
// that holds one JSON object. Else it changes nothing, and it returns the
// reason. It writes the session file with a temp file and a rename, so it
// never follows a symlink that the session put there.
//
// sync-state.json holds, for each provider, the SHA-256 of the OAuth value
// that the two files agreed on last (current), and of each value that a
// sync wrote or read (seen).

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PIAuthSyncEnvVar is set to "1" in a sandbox that has a per-session
// auth.json. The anthropic-oauth extension then asks the sidecar for a sync
// (POST /pi-auth/sync) around each token refresh, under the shared refresh
// lock (anthropic-oauth/session-sync.ts).
const PIAuthSyncEnvVar = "PRISM_PI_AUTH_SYNC"

// PIAuthSyncPath is the host-API route of the sync. PIAuthSyncEnvVar,
// PIAuthSyncPath, piRefreshLockName, piRefreshLockStale, and the owner file
// of TakePIRefreshLock copy values of the anthropic-oauth extension
// (session-sync.ts, refresh-lock.ts). TestPIAuthSync_ExtensionUsesSameValues
// fails when the two sides differ.
const PIAuthSyncPath = "/pi-auth/sync"

const (
	piAuthSessionsDirName     = "pi-auth"
	piAuthSessionAgentDirName = "agent"
	piAuthFileName            = "auth.json"
	piAuthSyncLockName        = "sync.lock"
	piAuthSyncStateName       = "sync-state.json"

	// piAuthTmpPrefix is the prefix of the temp files that the sync renames
	// onto an auth file.
	piAuthTmpPrefix = ".auth.json.prism-"

	// piAuthSessionFileLimit is the largest session auth file that the sync
	// reads. A real file is a few kB.
	piAuthSessionFileLimit = 1 << 20

	// piAuthSeenLimit is the number of values per provider that the sync
	// state keeps in seen.
	piAuthSeenLimit = 32

	// The limits on what a session can add to the host file, so that a
	// session cannot make the host file so large that every pi start fails
	// (CheckPIAuthFile) or that the sync refuses the file of each other
	// session (piAuthSessionFileLimit). A real token is less than 2 KiB, and
	// a real /login adds one provider.
	piAuthMaxTokenLen     = 16 << 10
	piAuthMaxNewProviders = 2
	piAuthHostWriteLimit  = 256 << 10

	// piAuthStateMaxProviders is the largest number of providers in the
	// sync state that the session names and the host file does not. With
	// the seen limit, it keeps the state file small, whatever names the
	// session writes. piAuthSyncStateLimit is the largest state file.
	piAuthStateMaxProviders = 64
	piAuthSyncStateLimit    = 4 << 20

	// piAuthFileLockStale is the age after which the proper-lockfile lock of
	// the host auth.json is stale. pi uses 10 s and 30 s and touches a held
	// lock at half that time, so a live holder never reaches this age.
	piAuthFileLockStale = 60 * time.Second

	// piRefreshLockName and piRefreshLockStale are REFRESH_LOCK_NAME and
	// LOCK_TIMINGS.staleMs of pi/extensions/anthropic-oauth/refresh-lock.ts.
	piRefreshLockName  = "anthropic-oauth-refresh.lock"
	piRefreshLockStale = 30 * time.Second
)

// The waits of the locks. The sync runs while it or its caller holds the
// shared refresh lock, so its waits are short. The sync does not wait for
// the lock of the host file at all: see SyncPIAuthSession.
var (
	piAuthSyncLockWait    = 3 * time.Second
	piAuthPrepareLockWait = 20 * time.Second
	piAuthLockPoll        = 20 * time.Millisecond
)

// ErrNoPIAuthSession is the error of SyncPIAuthSession when the session has
// no per-session agent dir: a host-mode session, a session before
// PreparePIAuthSession, or a session after cleanup.
var ErrNoPIAuthSession = errors.New("no per-session pi auth dir")

// ErrPIRefreshLockHeld is the error of TakePIRefreshLock when another
// holder has the lock at the end of the wait.
var ErrPIRefreshLockHeld = errors.New("the shared OAuth refresh lock is held")

var piAuthInstanceIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// piAuthProviderRe is the form of a provider name that the sync copies from
// a session file to the host file.
var piAuthProviderRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// PIAuthSessionRoot returns <state>/prism/pi-auth/<instanceID>, where
// <state> is $XDG_STATE_HOME, else ~/.local/state. The sandbox cannot write
// it.
func PIAuthSessionRoot(instanceID string) (string, error) {
	if !piAuthInstanceIDRe.MatchString(instanceID) {
		return "", fmt.Errorf("pi auth: instance id %q is not a valid dir name", instanceID)
	}
	base := xdgStateBase()
	if base == "" {
		return "", errors.New("pi auth: no state dir")
	}
	return filepath.Join(base, "prism", piAuthSessionsDirName, instanceID), nil
}

// PIAuthSessionAgentDir returns <root>/agent, the dir of the session auth
// file. The sandbox can write it.
func PIAuthSessionAgentDir(instanceID string) (string, error) {
	root, err := PIAuthSessionRoot(instanceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, piAuthSessionAgentDirName), nil
}

// PIHostAgentAuthPath returns ~/.pi/agent/auth.json: the host auth file of
// EnsurePIAgentConfigDir. It returns "" when the home dir is not known.
func PIHostAgentAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", piAuthFileName)
}

// RemovePIAuthSession removes the per-session auth dir of instanceID. It is
// best-effort and idempotent: prism cleanup calls it for each session.
func RemovePIAuthSession(instanceID string) {
	root, err := PIAuthSessionRoot(instanceID)
	if err != nil {
		return
	}
	_ = os.RemoveAll(root)
}

// PreparePIAuthSession makes the per-session auth dir of instanceID and
// writes agent/auth.json from the host file at hostAuthPath, less each entry
// that holds a string that starts with "!". It returns the agent dir.
//
// The agent dir is made again from nothing, so a file that an earlier
// sandbox of the session put there does not stay. A missing or empty host
// file gives an empty object. A host file that the sync cannot read is an
// error: the session then does not start.
func PreparePIAuthSession(instanceID, hostAuthPath string) (string, error) {
	root, err := PIAuthSessionRoot(instanceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", fmt.Errorf("pi auth: create %s: %w", filepath.Dir(root), err)
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("pi auth: create %s: %w", root, err)
	}
	if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("pi auth: %s is not a dir", root)
	}

	unlock, err := lockPIAuthSync(root)
	if err != nil {
		return "", err
	}
	defer unlock()

	agentDir := filepath.Join(root, piAuthSessionAgentDirName)
	if err := os.RemoveAll(agentDir); err != nil {
		return "", fmt.Errorf("pi auth: remove the old %s: %w", agentDir, err)
	}
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		return "", fmt.Errorf("pi auth: create %s: %w", agentDir, err)
	}

	host, err := readPIAuthHostLocked(hostAuthPath)
	if err != nil {
		return "", err
	}
	session := sanitizePIAuth(host.entries)
	data, err := marshalPIAuth(session)
	if err != nil {
		return "", err
	}
	if err := writePIAuthFile(filepath.Join(agentDir, piAuthFileName), data); err != nil {
		return "", fmt.Errorf("pi auth: write the session auth file: %w", err)
	}

	state, _ := readPIAuthSyncState(root)
	state.setCurrent(session)
	if err := writePIAuthSyncState(root, state); err != nil {
		return "", err
	}
	return agentDir, nil
}

// LinkPIAgentDirEntries puts in agentDir a symlink to each entry of the host
// agent dir hostDir, except auth.json, the lock dirs, and the temp files of
// the sync. sandbox-exec uses agentDir as the pi agent dir: pi reads
// settings, extensions, skills, and sessions through the symlinks, and the
// profile grants a write of the host targets that pi must write. An entry
// that appears on the host later is not linked until the next start.
func LinkPIAgentDirEntries(hostDir, agentDir string) error {
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return fmt.Errorf("pi: read agent dir %s: %w", hostDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == piAuthFileName || strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, piAuthTmpPrefix) {
			continue
		}
		if err := os.Symlink(filepath.Join(hostDir, name), filepath.Join(agentDir, name)); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("pi: link %s: %w", name, err)
		}
	}
	return nil
}

// PIAuthSyncResult says what one sync did. Nothing in it holds a value from
// a file.
type PIAuthSyncResult struct {
	// ToHost lists the providers whose session value the sync wrote to the
	// host file.
	ToHost []string
	// ToSession is true when the sync wrote the session file from the host
	// file.
	ToSession bool
	// Refused holds a reason for each session value, or for the session
	// file, that the sync did not copy to the host file.
	Refused []string
}

// SyncPIAuthSession copies changes between the session auth file of
// instanceID and the host auth file at hostAuthPath. See the header of this
// file for the rules.
//
// The caller must hold the shared OAuth refresh lock (TakePIRefreshLock),
// or run for a caller that holds it, so that the sync does not run between
// a read and a write of the refresh code of the extension.
//
// The sync takes the lock of the host file (auth.json.lock) only when it is
// free, and does not wait for it. A host-mode pi takes that lock first and
// then waits for the refresh lock, around its own refresh. The sync holds
// the refresh lock, so a wait for the host lock stops both for the length
// of the wait. Then the token of the sandbox does not reach the host before
// the host pi refreshes with the same refresh token. With the host lock
// held, the sync reads the file with no lock, refuses a file of zero bytes
// (a write of pi in progress), and writes with a rename after a check that
// the file did not change.
//
// It returns ErrNoPIAuthSession when the session has no agent dir. A session
// file that is a symlink, is not a regular file, is too large, or does not
// parse changes nothing: the result has the reason in Refused, and the
// error is nil.
func SyncPIAuthSession(instanceID, hostAuthPath string) (PIAuthSyncResult, error) {
	var res PIAuthSyncResult
	root, err := PIAuthSessionRoot(instanceID)
	if err != nil {
		return res, err
	}
	agentDir := filepath.Join(root, piAuthSessionAgentDirName)
	if fi, err := os.Lstat(agentDir); err != nil || !fi.IsDir() {
		return res, ErrNoPIAuthSession
	}

	unlock, err := lockPIAuthSync(root)
	if err != nil {
		return res, err
	}
	defer unlock()

	sessionPath := filepath.Join(agentDir, piAuthFileName)
	sess, err := readPIAuthSessionFile(sessionPath)
	if err != nil {
		res.Refused = append(res.Refused, err.Error())
		return res, nil
	}

	hostLocked := false
	if unlockHost, err := takePIAuthFileLock(hostAuthPath, 0); err == nil {
		hostLocked = true
		defer unlockHost()
	}

	host, err := readPIAuthHostFile(hostAuthPath)
	if err != nil {
		return res, err
	}
	if host.empty && !hostLocked {
		return res, fmt.Errorf("pi auth: the host auth file %s is empty while its lock is held; the next sync tries again", hostAuthPath)
	}
	state, stateErr := readPIAuthSyncState(root)
	if stateErr != nil {
		res.Refused = append(res.Refused, stateErr.Error())
	}
	hostBefore := make(map[string]json.RawMessage, len(host.entries))
	for k, v := range host.entries {
		hostBefore[k] = v
	}

	// From the session file to the host file.
	hostChanged := false
	newProviders := 0
	for _, p := range sortedKeys(sess.entries) {
		entry, reason := piAuthOAuthEntry(p, sess.entries[p])
		if reason != "" {
			// An entry that the sync wrote from the host file is not a
			// change of the session. Report each other refusal once.
			rawHash := piAuthRawHash(sess.entries[p])
			same := samePIAuth(map[string]json.RawMessage{p: sess.entries[p]}, map[string]json.RawMessage{p: host.entries[p]})
			if !same && !state.seen(p, rawHash) {
				if reason == piAuthNotOAuth {
					reason = fmt.Sprintf("provider %s: the entry is not an OAuth entry", piAuthName(p))
				}
				res.Refused = append(res.Refused, reason)
				state.addSessionSeen(p, rawHash)
			}
			continue
		}
		h := entry.hash()
		if state.seen(p, h) {
			continue
		}
		state.addSessionSeen(p, h)
		if len(entry.Access) > piAuthMaxTokenLen || len(entry.Refresh) > piAuthMaxTokenLen {
			res.Refused = append(res.Refused, fmt.Sprintf("provider %s: a token is longer than %d bytes", piAuthName(p), piAuthMaxTokenLen))
			continue
		}
		hostRaw, onHost := host.entries[p]
		hostEntry, hostReason := piAuthOAuthEntry(p, hostRaw)
		if onHost && hostReason != "" {
			res.Refused = append(res.Refused, fmt.Sprintf("provider %s: the host entry is not an OAuth entry, so the session value does not replace it", piAuthName(p)))
			continue
		}
		hostHash := ""
		if onHost {
			hostHash = hostEntry.hash()
		}
		if hostHash != state.current(p) {
			res.Refused = append(res.Refused, fmt.Sprintf("provider %s: the host entry changed too, so the host value wins", piAuthName(p)))
			continue
		}
		if !onHost && state.NewProviders+newProviders >= piAuthMaxNewProviders {
			res.Refused = append(res.Refused, fmt.Sprintf("provider %s: the session already added %d providers to the host file", piAuthName(p), piAuthMaxNewProviders))
			continue
		}
		merged, err := entry.overlay(hostRaw)
		if err != nil {
			res.Refused = append(res.Refused, fmt.Sprintf("provider %s: %v", piAuthName(p), err))
			continue
		}
		host.entries[p] = merged
		res.ToHost = append(res.ToHost, p)
		if !onHost {
			newProviders++
		}
		hostChanged = true
	}
	if hostChanged {
		// On an error the sync state is not written, so the next sync tries
		// the values again.
		notCopied := PIAuthSyncResult{Refused: res.Refused}.capped()
		data, err := marshalPIAuth(host.entries)
		if err != nil {
			return notCopied, err
		}
		if len(data) > piAuthHostWriteLimit {
			// The values stay in seen, so the refusal is reported once.
			for _, p := range res.ToHost {
				res.Refused = append(res.Refused, fmt.Sprintf("provider %s: the host file would be larger than %d bytes", piAuthName(p), piAuthHostWriteLimit))
			}
			res.ToHost = nil
			host.entries = hostBefore
		} else {
			if err := host.checkUnchanged(hostAuthPath); err != nil {
				return notCopied, err
			}
			if err := writePIAuthHostFile(hostAuthPath, data); err != nil {
				return notCopied, fmt.Errorf("pi auth: write the host auth file: %w", err)
			}
			state.NewProviders += newProviders
			host.empty = false
		}
	}

	// From the host file to the session file.
	session := sanitizePIAuth(host.entries)
	if !samePIAuth(session, sess.entries) {
		if err := sess.checkUnchanged(sessionPath); err != nil {
			// pi wrote the file after the read. The next sync copies it.
			_ = writePIAuthSyncState(root, state)
			return res.capped(), nil
		}
		data, err := marshalPIAuth(session)
		if err != nil {
			return res.capped(), err
		}
		if err := writePIAuthFile(sessionPath, data); err != nil {
			return res.capped(), fmt.Errorf("pi auth: write the session auth file: %w", err)
		}
		res.ToSession = true
	}
	state.setCurrent(session)
	if err := writePIAuthSyncState(root, state); err != nil {
		return res.capped(), err
	}
	return res.capped(), nil
}

// capped returns r with at most piAuthMaxRefused reasons.
func (r PIAuthSyncResult) capped() PIAuthSyncResult {
	if n := len(r.Refused); n > piAuthMaxRefused {
		r.Refused = append(r.Refused[:piAuthMaxRefused:piAuthMaxRefused], fmt.Sprintf("%d more session values", n-piAuthMaxRefused))
	}
	return r
}

// PIAuthFileRevision returns a value that changes when the file at path is
// written or replaced. It returns "" for a missing file. The sidecar uses it
// to skip a sync when neither file changed.
func PIAuthFileRevision(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return piAuthRevision(fi)
}

// ── OAuth entries ─────────────────────────────────────────────────────────

// piAuthNotOAuth is the reason for an entry that is not of type "oauth". The
// sync does not copy such an entry.
const piAuthNotOAuth = "not oauth"

// piAuthMaxRefused is the largest number of reasons that one sync returns,
// so that a session cannot fill the sidecar log.
const piAuthMaxRefused = 10

// piAuthName returns the provider name p for a reason: quoted, and cut to
// 64 bytes. The session chooses the name.
func piAuthName(p string) string {
	if len(p) > 64 {
		p = p[:64] + "..."
	}
	return strconv.Quote(p)
}

// piOAuthEntry is an OAuth entry with only the fields that the sync copies.
// The field order is the order of the hash input.
type piOAuthEntry struct {
	Type    string  `json:"type"`
	Access  string  `json:"access"`
	Refresh string  `json:"refresh"`
	Expires float64 `json:"expires"`
}

// piAuthOAuthEntry returns the OAuth fields of raw, or the reason why raw
// is not an entry that the sync copies to the host file.
func piAuthOAuthEntry(provider string, raw json.RawMessage) (piOAuthEntry, string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return piOAuthEntry{}, fmt.Sprintf("provider %s: the entry is not a JSON object", piAuthName(provider))
	}
	var typ string
	if err := json.Unmarshal(fields["type"], &typ); err != nil || typ != "oauth" {
		return piOAuthEntry{}, piAuthNotOAuth
	}
	if !piAuthProviderRe.MatchString(provider) {
		return piOAuthEntry{}, fmt.Sprintf("the provider name %s is not a name that prism copies", piAuthName(provider))
	}
	e := piOAuthEntry{Type: "oauth"}
	for _, field := range []struct {
		name string
		dst  *string
	}{{"access", &e.Access}, {"refresh", &e.Refresh}} {
		var v any
		s, ok := "", false
		if raw, present := fields[field.name]; present && json.Unmarshal(raw, &v) == nil {
			s, ok = v.(string)
		}
		if !ok {
			return piOAuthEntry{}, fmt.Sprintf("provider %s: %s is not a string", piAuthName(provider), field.name)
		}
		if s == "" || strings.HasPrefix(s, "!") {
			return piOAuthEntry{}, fmt.Sprintf("provider %s: %s is empty or starts with \"!\"", piAuthName(provider), field.name)
		}
		*field.dst = s
	}
	// Decode into any: a json.Number target also takes a JSON string.
	var v any
	dec := json.NewDecoder(bytes.NewReader(fields["expires"]))
	dec.UseNumber()
	num, isNum := json.Number(""), false
	if fields["expires"] != nil && dec.Decode(&v) == nil {
		num, isNum = v.(json.Number)
	}
	if !isNum {
		return piOAuthEntry{}, fmt.Sprintf("provider %s: expires is not a number", piAuthName(provider))
	}
	f, err := strconv.ParseFloat(num.String(), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return piOAuthEntry{}, fmt.Sprintf("provider %s: expires is not a finite number", piAuthName(provider))
	}
	e.Expires = f
	return e, ""
}

func (e piOAuthEntry) hash() string {
	data, _ := json.Marshal(e)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// overlay returns the host entry hostRaw with the four fields of e in place
// of its own. A field of the host entry that the sync does not copy, for
// example the enterprise URL of a provider, stays. With no host entry, it
// returns e alone.
func (e piOAuthEntry) overlay(hostRaw json.RawMessage) (json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if hostRaw != nil {
		if err := json.Unmarshal(hostRaw, &out); err != nil {
			return nil, errors.New("the host entry is not a JSON object")
		}
	}
	own, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(own, &fields); err != nil {
		return nil, err
	}
	for k, v := range fields {
		out[k] = v
	}
	return json.Marshal(out)
}

// piAuthRawHash is the hash of an entry that is not a valid OAuth entry, so
// that the sync reports its refusal once.
func piAuthRawHash(raw json.RawMessage) string {
	sum := sha256.Sum256(append([]byte("raw:"), raw...))
	return hex.EncodeToString(sum[:])
}

// sanitizePIAuth returns the entries less each entry that holds a string
// that starts with "!", at any depth. pi runs such a key as a shell command.
func sanitizePIAuth(entries map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(entries))
	for p, raw := range entries {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil || hasCommandValue(v) {
			continue
		}
		out[p] = raw
	}
	return out
}

// samePIAuth reports whether a and b hold the same JSON values.
func samePIAuth(a, b map[string]json.RawMessage) bool {
	ca, errA := canonicalPIAuth(a)
	cb, errB := canonicalPIAuth(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

func canonicalPIAuth(entries map[string]json.RawMessage) ([]byte, error) {
	m := make(map[string]any, len(entries))
	for p, raw := range entries {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		m[p] = v
	}
	return json.Marshal(m)
}

// marshalPIAuth returns the entries as pi writes them: one JSON object with
// an indent of two spaces.
func marshalPIAuth(entries map[string]json.RawMessage) ([]byte, error) {
	if entries == nil {
		entries = map[string]json.RawMessage{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("pi auth: marshal: %w", err)
	}
	return data, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── File reads and writes ─────────────────────────────────────────────────

// piAuthFile is the content of an auth file and the revision that the read
// saw.
type piAuthFile struct {
	entries  map[string]json.RawMessage
	revision string
	// empty is true for a file of zero bytes.
	empty bool
}

// checkUnchanged returns an error when the file at path is not the file
// that the read saw. It follows a symlink at path, as the reads do: the
// read of a session file refuses a symlink.
func (f piAuthFile) checkUnchanged(path string) error {
	if PIAuthFileRevision(path) != f.revision {
		return fmt.Errorf("pi auth: %s changed during the sync", path)
	}
	return nil
}

// readPIAuthSessionFile reads a session auth file. The error says why the
// sync does not use the file, and holds no value from it. A missing file
// gives no entries.
func readPIAuthSessionFile(path string) (piAuthFile, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return piAuthFile{entries: map[string]json.RawMessage{}}, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return piAuthFile{}, fmt.Errorf("session auth file %s is a symlink, so nothing is copied", path)
	}
	if err != nil {
		return piAuthFile{}, fmt.Errorf("session auth file %s cannot be opened (%v), so nothing is copied", path, errors.Unwrap(err))
	}
	defer f.Close()
	return readPIAuthOpenFile(f, path, "session auth file", piAuthSessionFileLimit)
}

// readPIAuthHostFile reads the host auth file, with the same checks. It
// follows a symlink: the sandbox cannot write the host agent dir, so a
// symlink there is one that the user made. A missing file gives no entries.
func readPIAuthHostFile(path string) (piAuthFile, error) {
	if path == "" {
		return piAuthFile{}, errors.New("pi auth: no host auth file path")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return piAuthFile{entries: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return piAuthFile{}, fmt.Errorf("pi auth: open the host auth file %s: %w", path, err)
	}
	defer f.Close()
	file, err := readPIAuthOpenFile(f, path, "host auth file", piAuthFileLimit)
	if err != nil {
		return piAuthFile{}, fmt.Errorf("pi auth: %w", err)
	}
	return file, nil
}

// readPIAuthHostLocked reads the host auth file under its lock. A host pi
// holds the lock while it refreshes a token, so the wait is long. If the
// lock is still held after it, the read runs with no lock: a session must
// not fail to start because of a busy lock. A read that sees a part of a
// write gives an error or an empty file, and the next sync corrects the
// session file.
func readPIAuthHostLocked(path string) (piAuthFile, error) {
	if unlock, err := takePIAuthFileLock(path, piAuthPrepareLockWait); err == nil {
		defer unlock()
	}
	return readPIAuthHostFile(path)
}

func readPIAuthOpenFile(f *os.File, path, what string, limit int64) (piAuthFile, error) {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return piAuthFile{}, fmt.Errorf("%s %s is not a regular file, so nothing is copied", what, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return piAuthFile{}, fmt.Errorf("%s %s cannot be read, so nothing is copied", what, path)
	}
	if int64(len(data)) > limit {
		return piAuthFile{}, fmt.Errorf("%s %s is larger than %d bytes, so nothing is copied", what, path, limit)
	}
	out := piAuthFile{revision: piAuthRevision(fi), empty: len(data) == 0}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(data)) == 0 {
		out.entries = map[string]json.RawMessage{}
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&out.entries); err != nil || out.entries == nil {
		return piAuthFile{}, fmt.Errorf("%s %s is not one JSON object, so nothing is copied", what, path)
	}
	if _, err := dec.Token(); err != io.EOF {
		return piAuthFile{}, fmt.Errorf("%s %s is not one JSON object, so nothing is copied", what, path)
	}
	return out, nil
}

func piAuthRevision(fi fs.FileInfo) string {
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return fmt.Sprintf("%d:%d:%d:%s", ino, fi.Size(), fi.ModTime().UnixNano(), fi.Mode().Type())
}

// writePIAuthHostFile writes the host auth file. When the user made it a
// symlink, it writes the target, so the symlink stays.
func writePIAuthHostFile(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = target
	}
	return writePIAuthFile(path, data)
}

// writePIAuthFile writes data to path with mode 0600: a temp file in the
// same dir, then a rename. The rename replaces a symlink at path and does
// not follow it. A reader sees the old file or the new file, never a part.
func writePIAuthFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), piAuthTmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	werr := tmp.Chmod(0o600)
	if werr == nil {
		_, werr = tmp.Write(data)
	}
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

// ── Sync state ────────────────────────────────────────────────────────────

type piAuthSyncState struct {
	Providers map[string]*piAuthProviderState `json:"providers"`
	// NewProviders is the number of providers that the session added to
	// the host file. The state is kept across the starts of the session.
	NewProviders int `json:"new_providers"`
}

type piAuthProviderState struct {
	Current string   `json:"current"`
	Seen    []string `json:"seen"`
}

func (s *piAuthSyncState) provider(p string) *piAuthProviderState {
	if s.Providers == nil {
		s.Providers = map[string]*piAuthProviderState{}
	}
	ps := s.Providers[p]
	if ps == nil {
		ps = &piAuthProviderState{}
		s.Providers[p] = ps
	}
	return ps
}

func (s *piAuthSyncState) current(p string) string {
	if ps := s.Providers[p]; ps != nil {
		return ps.Current
	}
	return ""
}

func (s *piAuthSyncState) seen(p, h string) bool {
	if ps := s.Providers[p]; ps != nil {
		for _, x := range ps.Seen {
			if x == h {
				return true
			}
		}
	}
	return false
}

// addSessionSeen records h, a value that the session file holds, in seen.
// The session chooses the provider name and the number of names. So a
// provider that the state does not have yet is added only when its name has
// the form of piAuthProviderRe and the state has fewer than
// piAuthStateMaxProviders providers. Else the value is not recorded: the
// sync then refuses it again at the next sync, and reports it again.
func (s *piAuthSyncState) addSessionSeen(p, h string) {
	if _, ok := s.Providers[p]; !ok && (!piAuthProviderRe.MatchString(p) || len(s.Providers) >= piAuthStateMaxProviders) {
		return
	}
	s.addSeen(p, h)
}

func (s *piAuthSyncState) addSeen(p, h string) {
	if h == "" || s.seen(p, h) {
		return
	}
	ps := s.provider(p)
	ps.Seen = append(ps.Seen, h)
	if len(ps.Seen) > piAuthSeenLimit {
		ps.Seen = ps.Seen[len(ps.Seen)-piAuthSeenLimit:]
	}
}

// setCurrent records the OAuth values of entries, the content of the two
// files after a sync, as current and as seen.
func (s *piAuthSyncState) setCurrent(entries map[string]json.RawMessage) {
	for p, ps := range s.Providers {
		if _, ok := entries[p]; !ok {
			ps.Current = ""
		}
	}
	for p, raw := range entries {
		e, reason := piAuthOAuthEntry(p, raw)
		if reason != "" {
			if ps := s.Providers[p]; ps != nil {
				ps.Current = ""
			}
			continue
		}
		h := e.hash()
		s.provider(p).Current = h
		s.addSeen(p, h)
	}
}

// readPIAuthSyncState reads the sync state in root. Only prism writes the
// file: the sandbox cannot write root. A missing file gives an empty state.
// A file that cannot be read fails closed: the state is empty, and it
// counts piAuthMaxNewProviders new providers, so that a lost state does not
// give the session a new allowance. The error says so.
func readPIAuthSyncState(root string) (*piAuthSyncState, error) {
	f, err := os.OpenFile(filepath.Join(root, piAuthSyncStateName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return &piAuthSyncState{}, nil
	}
	closed := &piAuthSyncState{NewProviders: piAuthMaxNewProviders}
	failed := fmt.Errorf("the sync state in %s cannot be read, so the session adds no new provider to the host file", root)
	if err != nil {
		return closed, failed
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, piAuthSyncStateLimit+1))
	st := &piAuthSyncState{}
	if err != nil || len(data) > piAuthSyncStateLimit || json.Unmarshal(data, st) != nil {
		return closed, failed
	}
	return st, nil
}

func writePIAuthSyncState(root string, st *piAuthSyncState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(data) > piAuthSyncStateLimit {
		return fmt.Errorf("pi auth: the sync state is larger than %d bytes", piAuthSyncStateLimit)
	}
	if err := writePIAuthFile(filepath.Join(root, piAuthSyncStateName), data); err != nil {
		return fmt.Errorf("pi auth: write the sync state: %w", err)
	}
	return nil
}

// ── Locks ─────────────────────────────────────────────────────────────────

// newPILockID returns the owner id of a lock dir that prism takes.
func newPILockID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("prism-%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
}

// lockPIAuthSync takes the flock of root/sync.lock. It serialises the syncs
// and the prepare of one session, also between the sidecar and prism
// agent-run. It returns the unlock function.
func lockPIAuthSync(root string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, piAuthSyncLockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pi auth: open the sync lock: %w", err)
	}
	deadline := time.Now().Add(piAuthSyncLockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("pi auth: take the sync lock: %w", err)
		}
		time.Sleep(piAuthLockPoll)
	}
}

// takePIAuthFileLock takes the lock that pi takes around each read and
// write of the auth file at path: the proper-lockfile lock dir <path>.lock.
// So a sync does not interleave with a host pi that writes the file. A lock
// dir that is older than piAuthFileLockStale belongs to a dead holder and is
// removed. It returns the unlock function.
func takePIAuthFileLock(path string, wait time.Duration) (func(), error) {
	if path == "" {
		return nil, errors.New("pi auth: no host auth file path")
	}
	lock := path + ".lock"
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(lock, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			// No agent dir yet: there is no host file to protect.
			return func() {}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("pi auth: take the lock %s: %w", lock, err)
		}
		if fi, lerr := os.Lstat(lock); lerr == nil && time.Since(fi.ModTime()) > piAuthFileLockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pi auth: the lock %s is held", lock)
		}
		time.Sleep(piAuthLockPoll)
	}
}

// TakePIRefreshLock takes the shared OAuth refresh lock in lockDir, in the
// format of pi/extensions/anthropic-oauth/refresh-lock.ts: a dir that mkdir
// makes, with an owner file. It waits at most wait. A lock dir that nobody
// touched for piRefreshLockStale belongs to a dead holder and is removed,
// as the extension does. It returns ErrPIRefreshLockHeld when another
// holder has the lock at the end of the wait. The caller must call the
// release function.
func TakePIRefreshLock(lockDir string, wait time.Duration) (func(), error) {
	if lockDir == "" {
		return nil, errors.New("pi auth: no shared lock dir")
	}
	path := filepath.Join(lockDir, piRefreshLockName)
	id := newPILockID()
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			if werr := writePINotionLockOwner(path, id); werr != nil {
				// An owner file that exists right after the mkdir is not
				// ours: another process changed the dir. Leave it.
				if !errors.Is(werr, fs.ErrExist) {
					_ = os.RemoveAll(path)
				}
				return nil, fmt.Errorf("pi auth: write owner of lock %s: %w", path, werr)
			}
			l := &piNotionLock{dir: path, id: id}
			return l.release, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("pi auth: take lock %s: %w", path, err)
		}
		observed := readPINotionLockOwnerID(path)
		if fi, lerr := os.Lstat(path); lerr == nil && time.Since(fi.ModTime()) > piRefreshLockStale &&
			readPINotionLockOwnerID(path) == observed {
			_ = os.RemoveAll(path)
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, ErrPIRefreshLockHeld
		}
		time.Sleep(piAuthLockPoll)
	}
}
