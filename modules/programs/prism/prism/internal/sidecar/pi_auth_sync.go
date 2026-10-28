package sidecar

// pi_auth_sync.go — the sync of the per-session auth.json of a bwrap or
// sandbox-exec pi session (#3295).
//
// A sandboxed session reads and writes its own auth.json
// (internal/container/pi_auth_session.go). The sidecar of the session runs
// on the host, and copies OAuth entries between that file and the host
// ~/.pi/agent/auth.json:
//
//   - on a timer, so that `prism account use`, a refresh of another session,
//     and a login in the session reach the other file with no restart. The
//     timer sync takes the shared OAuth refresh lock, so it does not run in
//     the middle of a refresh of the anthropic-oauth extension. If the lock
//     is held, it tries again at the next tick.
//   - on POST /pi-auth/sync. The anthropic-oauth extension calls it while it
//     holds the shared refresh lock: before it reads auth.json, to get a
//     token that another session wrote, and after it writes a new token, so
//     that the next holder of the lock gets it. So one refresh serves all
//     sessions. The handler does not take the lock: its caller holds it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

// piAuthSyncRoute is the host-API route of the sync. The extension sends
// its request to the same path (session-sync.ts).
const piAuthSyncRoute = container.PIAuthSyncPath

// piAuthSyncInterval is the period of the timer sync. Tests replace it.
var piAuthSyncInterval = 3 * time.Second

// piAuthSyncMinGap is the smallest time between two syncs of the endpoint,
// so that a session cannot keep the lock of the host auth.json busy. A
// request that has to wait more than piAuthSyncMaxWait gets 429.
var (
	piAuthSyncMinGap  = 100 * time.Millisecond
	piAuthSyncMaxWait = time.Second
)

// errPIAuthNoInstance is the error of a sync before the sidecar knows its
// instance ID.
var errPIAuthNoInstance = errors.New("the sidecar has no instance id yet")

// piAuthSyncer is the state of the sync of one sidecar.
type piAuthSyncer struct {
	mu sync.Mutex
	// lastSession and lastHost are the revisions of the two files before
	// the last sync that completed. The timer sync does nothing while both
	// files keep these revisions.
	lastSession, lastHost string
	// lastEndpoint is the time of the last sync of the endpoint.
	lastEndpoint time.Time
}

// piAuthSyncApplies reports whether this session has a per-session
// auth.json: a pi session in bwrap or sandbox-exec.
func (s *Sidecar) piAuthSyncApplies() bool {
	if s.cfg.HarnessName != "" && s.cfg.HarnessName != "pi" {
		return false
	}
	switch s.cfg.IsolationMode {
	case config.IsolationBwrap, config.IsolationSandboxExec:
		return true
	}
	return false
}

// startPIAuthSync starts the timer sync. It stops when ctx ends.
func (s *Sidecar) startPIAuthSync(ctx context.Context) {
	if !s.piAuthSyncApplies() {
		return
	}
	go func() {
		ticker := time.NewTicker(piAuthSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.syncPIAuth(true)
			}
		}
	}()
}

// syncPIAuth runs one sync of the session auth.json and logs what it did.
// fromTimer selects the timer sync: it takes the shared refresh lock, and
// it does nothing when neither file changed since the last sync. The sync
// of the endpoint runs under the lock of its caller, and always runs.
func (s *Sidecar) syncPIAuth(fromTimer bool) error {
	id := s.instanceID()
	if id == "" {
		return errPIAuthNoInstance
	}
	agentDir, err := container.PIAuthSessionAgentDir(id)
	if err != nil {
		return err
	}
	hostAuth := container.PIHostAgentAuthPath()
	if hostAuth == "" {
		return errors.New("no home dir for the host auth.json")
	}

	s.piAuth.mu.Lock()
	defer s.piAuth.mu.Unlock()

	sessionRev := container.PIAuthFileRevision(filepath.Join(agentDir, "auth.json"))
	hostRev := container.PIAuthFileRevision(hostAuth)
	if fromTimer {
		if sessionRev == s.piAuth.lastSession && hostRev == s.piAuth.lastHost {
			return nil
		}
		if fi, statErr := os.Lstat(agentDir); statErr != nil || !fi.IsDir() {
			return container.ErrNoPIAuthSession
		}
		home, _ := os.UserHomeDir()
		lockDir, _ := container.PISharedStateDirs(home)
		release, lockErr := container.TakePIRefreshLock(lockDir, 0)
		switch {
		case errors.Is(lockErr, container.ErrPIRefreshLockHeld):
			// A refresh runs now. Its holder asks for a sync itself.
			return lockErr
		case lockErr != nil:
			// The extension refreshes with no lock in this case, so the
			// sync runs with no lock too.
			s.logger().Printf("sidecar: pi auth sync: %v; the sync runs with no lock", lockErr)
		default:
			defer release()
		}
	}

	res, err := container.SyncPIAuthSession(id, hostAuth)
	if errors.Is(err, container.ErrNoPIAuthSession) {
		return err
	}
	if len(res.ToHost) > 0 {
		s.logger().Printf("sidecar: pi auth sync: copied the OAuth entry of %v from the session auth.json to the host auth.json", res.ToHost)
	}
	if res.ToSession {
		s.logger().Printf("sidecar: pi auth sync: wrote the session auth.json from the host auth.json")
	}
	for _, reason := range res.Refused {
		s.logger().Printf("sidecar: pi auth sync: not copied to the host: %s", reason)
	}
	if err != nil {
		s.logger().Printf("sidecar: pi auth sync: %v", err)
		return err
	}
	s.piAuth.lastSession, s.piAuth.lastHost = sessionRev, hostRev
	return nil
}

// handlePIAuthSync serves POST /pi-auth/sync. It runs one sync of the
// auth.json of this session, and answers {"ok":true} when it completed.
// The request body is not read. A session can sync only its own file: the
// sidecar serves one session, and the request names no path.
func (s *Sidecar) handlePIAuthSync(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.Method != http.MethodPost {
		reply(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.piAuthSyncApplies() {
		reply(http.StatusNotFound, map[string]string{"error": "this session has no per-session auth.json"})
		return
	}

	s.piAuth.mu.Lock()
	wait := piAuthSyncMinGap - time.Since(s.piAuth.lastEndpoint)
	if wait > piAuthSyncMaxWait {
		s.piAuth.mu.Unlock()
		reply(http.StatusTooManyRequests, map[string]string{"error": "too many sync requests"})
		return
	}
	s.piAuth.lastEndpoint = time.Now().Add(max(wait, 0))
	s.piAuth.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}

	switch err := s.syncPIAuth(false); {
	case errors.Is(err, container.ErrNoPIAuthSession):
		reply(http.StatusNotFound, map[string]string{"error": "this session has no per-session auth.json"})
	case err != nil:
		reply(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	default:
		reply(http.StatusOK, map[string]bool{"ok": true})
	}
}
