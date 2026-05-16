package iris

// supervisor_state_publish_test.go — asserts that the supervisor calls
// PublishState on its EventPublisher (when that publisher implements
// stateNotifier) at every state transition. This is the wire `iris logs
// --follow` relies on for prompt terminal-state detection (issue #1675).

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// statePublisherSpy records every PublishState call. It also implements
// EventPublisher.Publish as a no-op so it can be used wherever the harness
// expects an EventPublisher.
type statePublisherSpy struct {
	mu     sync.Mutex
	states []stateEvent
}

type stateEvent struct {
	Name  string
	State string
}

func (sp *statePublisherSpy) Publish(_ EventPublication) {}

func (sp *statePublisherSpy) PublishState(name, state string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.states = append(sp.states, stateEvent{Name: name, State: state})
}

func (sp *statePublisherSpy) snapshot() []stateEvent {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	out := make([]stateEvent, len(sp.states))
	copy(out, sp.states)
	return out
}

// TestSupervisorSetState_PublishesState constructs a Supervisor without
// invoking Start, then calls setState manually and asserts the spy received
// the matching PublishState invocations. This is a unit-level check; the
// integration path (full daemon + ClientSocket.PublishState) is covered by
// the logs CLI tests.
func TestSupervisorSetState_PublishesState(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	spy := &statePublisherSpy{}

	// Short prefix to stay under the 108-byte sun_path limit (t.TempDir()
	// with a long test name easily overruns).
	runDir, err := os.MkdirTemp("", "iris-sst-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName: "iris-test@state-publish",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		LogDir:      filepath.Join(tmp, "logs"),
		Database:    database,
		Publisher:   spy,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	sup.setState(StateActive)
	sup.setState(StateFinished)

	// Give the publisher a moment in case it ever becomes async.
	time.Sleep(20 * time.Millisecond)

	got := spy.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 state publications, got %d: %+v", len(got), got)
	}
	if got[0] != (stateEvent{Name: "iris-test@state-publish", State: string(StateActive)}) {
		t.Errorf("first: got %+v", got[0])
	}
	if got[1] != (stateEvent{Name: "iris-test@state-publish", State: string(StateFinished)}) {
		t.Errorf("second: got %+v", got[1])
	}
}

// TestSupervisorSetState_PlainEventPublisherIsNotNotified asserts that a
// publisher which only implements Publish (no PublishState) is not invoked
// for *state transitions* via PublishState — the supervisor must type-assert
// defensively so existing test fixtures keep compiling.
//
// Issue #1674 added a session_end event write on terminal transitions; that
// event flows through the regular Publish path (it is a real agent_events
// row, not a state notification) so plain publishers DO receive exactly one
// session_end Publish call per terminal transition. This test pins both
// behaviours: no PublishState invocations on a plain publisher, and exactly
// one Publish call carrying the session_end event.
func TestSupervisorSetState_PlainEventPublisherIsNotNotified(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// PublisherFunc satisfies EventPublisher but NOT stateNotifier.
	var pubMu sync.Mutex
	var publishedTypes []string
	pub := PublisherFunc(func(p EventPublication) {
		pubMu.Lock()
		defer pubMu.Unlock()
		publishedTypes = append(publishedTypes, p.EventType)
	})

	runDir, err := os.MkdirTemp("", "iris-sst-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName: "iris-test@plain-publisher",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		LogDir:      filepath.Join(tmp, "logs"),
		Database:    database,
		Publisher:   pub,
	}
	// Insert a sessions row so the FK on agent_events.instance_id is
	// satisfied when writeSessionEndEvent runs on terminal transitions.
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	sup.setState(StateActive)
	sup.setState(StateFinished)

	pubMu.Lock()
	got := append([]string(nil), publishedTypes...)
	pubMu.Unlock()

	if len(got) != 1 || got[0] != "session_end" {
		t.Errorf("expected exactly one session_end Publish call, got %v", got)
	}
}

// TestSupervisorWritesPerSessionLog asserts that constructing a supervisor
// with a LogDir results in supervisor.logf writing to the per-session file.
// This is the bridge `iris logs <session>` depends on.
func TestSupervisorWritesPerSessionLog(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	logDir := filepath.Join(tmp, "logs")
	runDir, err := os.MkdirTemp("", "iris-sst-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	cfg := SupervisorConfig{
		SessionName: "iris-test@logfile",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		LogDir:      logDir,
		Database:    database,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	sup.logf("hello %s", "world")

	// Sync via Close-and-reopen would be invasive; the underlying logger
	// writes synchronously. Read the file directly.
	logPath := filepath.Join(logDir, "iris-test@logfile.log")
	data, err := readFileWithRetry(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !containsBytes(data, []byte("hello world")) {
		t.Fatalf("log file does not contain 'hello world': %q", string(data))
	}
	if !containsBytes(data, []byte("[iris:iris-test@logfile]")) {
		t.Fatalf("log file does not contain session prefix: %q", string(data))
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// readFileWithRetry retries a few times in case the test process has not yet
// flushed buffered writes to the log file. log.Logger writes are synchronous
// via the underlying *os.File, so this rarely needs to retry — but the
// helper guards against rare scheduler-driven flakes on slow CI.
func readFileWithRetry(path string) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	for i := 0; i < 10; i++ {
		data, err = os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return data, err
}
