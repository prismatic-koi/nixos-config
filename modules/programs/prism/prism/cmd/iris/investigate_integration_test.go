package main

// investigate_integration_test.go — end-to-end test of `iris investigate`
// against a real iris.ClientSocket.
//
// The test stands up an in-process ClientSocket whose SpawnSession
// callback is a recorder, then invokes runInvestigateAt() with the socket
// path. This proves the wire path used by `iris investigate`:
//
//   CLI                 →  sessions_list (resolve caller worktree + role)
//   CLI                 →  session_spawn frame on iris.sock
//   daemon ClientSocket →  invokes spawnFn(name, worktree, role="investigate", parent=<from>, ...)
//   daemon              →  session_spawned frame back to CLI
//   CLI                 →  prints sessionName to stdout
//
// is wired correctly end-to-end. We do not start a real pi child here —
// the spawnFn returns a fake *iris.Supervisor whose SessionRecord has
// the fields needed to fill out the ack frame. The full investigator
// behaviour (read-only enforcement) is covered by the unit tests on
// internal/iris/bash_permission.go and the tool_dispatcher read-only
// gates.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// TestInvestigate_EndToEndAgainstRealClientSocket exercises the full wire
// path against a real ClientSocket. It asserts:
//
//   - The CLI forwards the caller's worktree (resolved via sessions_list)
//     and the calling session name as the spawn frame's Parent.
//   - The CLI passes role="investigate" and the pre-computed session name
//     (<caller>~investigate-<slug>) to the daemon.
//   - The CLI prints the new session name on stdout.
//   - The spawned session appears in the daemon's active-sessions list
//     with role="investigate" and the expected `~investigate-` suffix.
func TestInvestigate_EndToEndAgainstRealClientSocket(t *testing.T) {
	iso := iristest.NewIsolated(t)

	const callerName = "iris-coordinator@invest-e2e"
	const callerWorktree = "/abs/path/to/caller-worktree"

	// Active-sessions snapshot the daemon hands to clients.  Initially
	// contains just the caller so resolveCallerWorktree succeeds.
	var (
		activeMu       sync.Mutex
		activeSessions = []iris.SessionSnapshot{{
			Name:       callerName,
			InstanceID: uuid.NewString(),
			State:      "active",
			Role:       "coordinator",
			Worktree:   callerWorktree,
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		}}
	)
	getActive := func() []iris.SessionSnapshot {
		activeMu.Lock()
		defer activeMu.Unlock()
		out := make([]iris.SessionSnapshot, len(activeSessions))
		copy(out, activeSessions)
		return out
	}

	// Record what spawnFn sees.
	var (
		recMu       sync.Mutex
		gotName     string
		gotWorktree string
		gotRole     string
		gotParent   string
		spawnCalls  int
	)
	spawnFn := func(_ context.Context, sessionName, worktree, role, parent string, _ map[string]any) (*iris.Supervisor, error) {
		recMu.Lock()
		gotName = sessionName
		gotWorktree = worktree
		gotRole = role
		gotParent = parent
		spawnCalls++
		recMu.Unlock()
		// Add the new session to the active set so the post-spawn
		// `iris sessions list` AC is observable.
		activeMu.Lock()
		activeSessions = append(activeSessions, iris.SessionSnapshot{
			Name:       sessionName,
			InstanceID: uuid.NewString(),
			State:      "active",
			Role:       role,
			Worktree:   worktree,
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		})
		activeMu.Unlock()
		return iris.NewFakeSupervisorForTest(sessionName, worktree, role), nil
	}

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          iso.Paths.Sock,
		Database:          iso.DB,
		GetActiveSessions: getActive,
		SpawnSession:      spawnFn,
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	// Drive the investigate CLI core.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	const prompt = "trace the call chain for SSH auth"
	if err := runInvestigateAt(ctx, iso.Paths.Sock, callerName, "", prompt, &out); err != nil {
		t.Fatalf("runInvestigateAt: %v", err)
	}

	// Asserts on what spawnFn observed.
	recMu.Lock()
	defer recMu.Unlock()
	if spawnCalls != 1 {
		t.Errorf("spawnFn calls = %d, want 1", spawnCalls)
	}
	const wantSlug = "trace-the-call-chain-for-ssh"
	wantName := callerName + "~investigate-" + wantSlug
	if gotName != wantName {
		t.Errorf("spawnFn session name = %q, want %q", gotName, wantName)
	}
	if gotWorktree != callerWorktree {
		t.Errorf("spawnFn worktree = %q, want %q (caller worktree borrowed)", gotWorktree, callerWorktree)
	}
	if gotRole != "investigate" {
		t.Errorf("spawnFn role = %q, want %q", gotRole, "investigate")
	}
	if gotParent != callerName {
		t.Errorf("spawnFn parent = %q, want %q (caller session forwarded for terminal-state notification)", gotParent, callerName)
	}

	// Stdout must contain the new session name (AC: "Returns within ~2
	// seconds with the session name on stdout").
	if !strings.Contains(out.String(), wantName) {
		t.Errorf("stdout = %q, want it to contain %q", out.String(), wantName)
	}

	// AC: "An integration test spawns an investigator, sends a prompt,
	// asserts the session appears in `iris sessions list` with the
	// expected role/name." We check the in-memory snapshot directly
	// (which `iris sessions list` reads via getActive).
	activeMu.Lock()
	defer activeMu.Unlock()
	found := false
	for _, s := range activeSessions {
		if s.Name == wantName {
			found = true
			if s.Role != "investigate" {
				t.Errorf("active session %q role = %q, want %q", s.Name, s.Role, "investigate")
			}
			if !strings.Contains(s.Name, "~investigate-") {
				t.Errorf("active session %q name does not contain ~investigate-", s.Name)
			}
		}
	}
	if !found {
		names := make([]string, 0, len(activeSessions))
		for _, s := range activeSessions {
			names = append(names, s.Name)
		}
		t.Errorf("spawned investigator %q not found in active sessions; got %v", wantName, names)
	}
}

// TestInvestigate_NameOverrideSucceeds asserts that --name overrides the
// auto-derived slug and the resulting session name uses it verbatim.
func TestInvestigate_NameOverrideSucceeds(t *testing.T) {
	iso := iristest.NewIsolated(t)

	const callerName = "iris-coordinator@invest-name"
	const callerWorktree = "/abs/path/to/caller"

	var (
		activeMu       sync.Mutex
		activeSessions = []iris.SessionSnapshot{{
			Name: callerName, InstanceID: uuid.NewString(),
			State: "active", Role: "coordinator", Worktree: callerWorktree,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}}
	)
	var gotName string
	spawnFn := func(_ context.Context, sessionName, worktree, role, _parent string, _ map[string]any) (*iris.Supervisor, error) {
		gotName = sessionName
		activeMu.Lock()
		activeSessions = append(activeSessions, iris.SessionSnapshot{
			Name: sessionName, InstanceID: uuid.NewString(),
			State: "active", Role: role, Worktree: worktree,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})
		activeMu.Unlock()
		return iris.NewFakeSupervisorForTest(sessionName, worktree, role), nil
	}

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: iso.Paths.Sock,
		Database: iso.DB,
		GetActiveSessions: func() []iris.SessionSnapshot {
			activeMu.Lock()
			defer activeMu.Unlock()
			out := make([]iris.SessionSnapshot, len(activeSessions))
			copy(out, activeSessions)
			return out
		},
		SpawnSession: spawnFn,
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := runInvestigateAt(ctx, iso.Paths.Sock, callerName, "my-analysis", "irrelevant prompt body", &out); err != nil {
		t.Fatalf("runInvestigateAt: %v", err)
	}
	wantName := callerName + "~investigate-my-analysis"
	if gotName != wantName {
		t.Errorf("session name = %q, want %q (--name override)", gotName, wantName)
	}
}

// TestInvestigate_CallerNotRegistered asserts that when $IRIS_SESSION_NAME
// names a session the daemon does not know about, the CLI exits before
// spawning with a clear error.
func TestInvestigate_CallerNotRegistered(t *testing.T) {
	iso := iristest.NewIsolated(t)

	// Empty active-session set: any caller name will fail the registered-
	// session lookup.
	var spawnCalls int
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          iso.Paths.Sock,
		Database:          iso.DB,
		GetActiveSessions: func() []iris.SessionSnapshot { return nil },
		SpawnSession: func(_ context.Context, _name, _w, _r, _p string, _ map[string]any) (*iris.Supervisor, error) {
			spawnCalls++
			return nil, nil
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := runInvestigateAt(ctx, iso.Paths.Sock, "iris-coordinator@nope", "", "prompt body", &out)
	if err == nil {
		t.Fatalf("runInvestigateAt: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not a registered iris session") {
		t.Errorf("error = %v, want substring 'not a registered iris session'", err)
	}
	if spawnCalls != 0 {
		t.Errorf("spawnFn was called %d times; expected 0 (CLI must fail before sending session_spawn)", spawnCalls)
	}
}

// TestInvestigate_DaemonDown asserts that when the daemon socket does not
// exist, the CLI emits the canonical `systemctl --user start iris` hint
// and exits non-zero. The socket path must NOT exist (we don't Listen).
func TestInvestigate_DaemonDown(t *testing.T) {
	iso := iristest.NewIsolated(t)
	// Do not create the socket; runInvestigateAt's dial must fail with
	// the canonical error.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := runInvestigateAt(ctx, iso.Paths.Sock, "iris-coordinator@nope", "", "prompt body", &out)
	if err == nil {
		t.Fatalf("runInvestigateAt: want daemon-down error, got nil")
	}
	if !strings.Contains(err.Error(), "iris daemon not running") {
		t.Errorf("error = %v, want substring 'iris daemon not running'", err)
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("error = %v, want canonical hint 'systemctl --user start iris'", err)
	}
}

// TestInvestigate_RejectsNestedInvestigator asserts that a calling session
// whose role is "investigate" cannot spawn a further investigator. This
// matches the issue's Out-of-scope clause "Investigator that can spawn
// further investigators. No nesting."
func TestInvestigate_RejectsNestedInvestigator(t *testing.T) {
	iso := iristest.NewIsolated(t)
	const callerName = "iris-coordinator@root~investigate-existing"
	const callerWorktree = "/abs/path/to/worktree"
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: iso.Paths.Sock,
		Database: iso.DB,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return []iris.SessionSnapshot{{
				Name: callerName, InstanceID: uuid.NewString(),
				State: "active", Role: "investigate", Worktree: callerWorktree,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			}}
		},
		SpawnSession: func(_ context.Context, _name, _w, _r, _p string, _ map[string]any) (*iris.Supervisor, error) {
			t.Fatalf("spawnFn must not be called for a nested-investigator spawn")
			return nil, nil
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go cs.Serve(srvCtx)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := runInvestigateAt(ctx, iso.Paths.Sock, callerName, "", "any prompt", &out)
	if err == nil {
		t.Fatalf("runInvestigateAt: want nested-investigator error, got nil")
	}
	if !strings.Contains(err.Error(), "investigators may not spawn further investigators") {
		t.Errorf("error = %v, want substring 'investigators may not spawn further investigators'", err)
	}
}
