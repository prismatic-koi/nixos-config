package main

// prompt_integration_test.go — integration tests for `iris prompt` against a
// real iris.ClientSocket (issue #1677).
//
// Each test stands up an in-process ClientSocket whose GetActiveSessions and
// DeliverPrompt hooks are stubs we can observe, then drives runPromptAt
// against the live socket path. This proves the wire path used by
// `iris prompt`:
//
//	CLI                 →  sessions_list  + prompt_deliver  frames on iris.sock
//	daemon ClientSocket →  sessions_snapshot back, then invokes DeliverPrompt
//
// is wired correctly end-to-end. We do not run a real pi child — DeliverPrompt
// is a recorder.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// startPromptTestSocket starts an iris.ClientSocket on a tempdir socket path
// backed by `sessions` for sessions_list responses and `deliver` for
// prompt_deliver dispatch. Returns the socket path. The socket and its
// goroutines are torn down on test cleanup.
func startPromptTestSocket(
	t *testing.T,
	sessions []iris.SessionSnapshot,
	deliver func(ctx context.Context, name, text, deliverAs string, images []string) error,
) string {
	t.Helper()

	// Per-session Unix sockets must fit under the 108-byte sun_path limit.
	// t.TempDir() can blow that under long test names — anchor under a short
	// MkdirTemp prefix (same pattern used by spawn_integration_test.go).
	shortPrefix, err := os.MkdirTemp("", "iris-prompt-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	sockPath := filepath.Join(shortPrefix, "iris.sock")

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return sessions
		},
		DeliverPrompt: deliver,
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.Serve(ctx)

	return sockPath
}

// TestPrompt_HappyPath drives the full wire path:
// sessions_list → snapshot returns target session in active state →
// prompt_deliver → DeliverPrompt hook receives the body → CLI prints
// confirmation.
func TestPrompt_HappyPath(t *testing.T) {
	const targetName = "iris-test@active"
	const promptBody = "please proceed with the next step"

	sessions := []iris.SessionSnapshot{
		{
			Name:       targetName,
			InstanceID: "abcd1234-0000-1111-2222-333333333333",
			State:      "active",
			Role:       "worker",
			Worktree:   "/tmp/iris-test/feature",
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}

	var (
		mu          sync.Mutex
		gotName     string
		gotText     string
		gotDeliver  string
		deliverHits int
	)
	deliver := func(_ context.Context, name, text, deliverAs string, _ []string) error {
		mu.Lock()
		defer mu.Unlock()
		gotName = name
		gotText = text
		gotDeliver = deliverAs
		deliverHits++
		return nil
	}

	sockPath := startPromptTestSocket(t, sessions, deliver)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	if err := runPromptAt(ctx, sockPath, targetName, promptBody, nil, &out); err != nil {
		t.Fatalf("runPromptAt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if deliverHits != 1 {
		t.Errorf("DeliverPrompt called %d times, want 1", deliverHits)
	}
	if gotName != targetName {
		t.Errorf("DeliverPrompt name = %q, want %q", gotName, targetName)
	}
	if gotText != promptBody {
		t.Errorf("DeliverPrompt text = %q, want %q", gotText, promptBody)
	}
	// We don't send deliver_as from the CLI yet; the daemon-side handler
	// receives the empty string and substitutes "prompt" inside the
	// supervisor RPC. The hook here sees the raw frame value.
	if gotDeliver != "" {
		t.Errorf("DeliverPrompt deliver_as = %q, want \"\"", gotDeliver)
	}

	stdout := out.String()
	if !strings.Contains(stdout, "prompt delivered to "+targetName) {
		t.Errorf("expected confirmation in stdout; got %q", stdout)
	}
}

// TestPrompt_WaitingStateGuard asserts that a session in waiting state is
// refused: the CLI exits non-zero with the documented message and the
// DeliverPrompt hook is NEVER invoked (no prompt_deliver frame sent).
func TestPrompt_WaitingStateGuard(t *testing.T) {
	const targetName = "iris-test@paused"

	sessions := []iris.SessionSnapshot{
		{
			Name:       targetName,
			InstanceID: "deadbeef-0000-1111-2222-333333333333",
			State:      "waiting",
			Role:       "worker",
			Worktree:   "/tmp/iris-test/paused",
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}

	var deliverHits int
	deliver := func(_ context.Context, _, _, _ string, _ []string) error {
		deliverHits++
		return nil
	}

	sockPath := startPromptTestSocket(t, sessions, deliver)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runPromptAt(ctx, sockPath, targetName, "do the thing", nil, &out)
	if err == nil {
		t.Fatalf("runPromptAt: want error for waiting state, got nil (stdout=%q)", out.String())
	}
	msg := err.Error()
	if !strings.Contains(msg, "waiting for user input") {
		t.Errorf("error missing 'waiting for user input' wording: %q", msg)
	}
	if !strings.Contains(msg, targetName) {
		t.Errorf("error missing session name %q: %q", targetName, msg)
	}
	if deliverHits != 0 {
		t.Errorf("DeliverPrompt invoked %d times despite waiting-state guard; want 0", deliverHits)
	}
	if out.Len() != 0 {
		t.Errorf("expected no stdout on refusal; got %q", out.String())
	}
}

// TestPrompt_NoSuchSession asserts that an unknown session name produces a
// clean "no such session" error referencing the requested name, without
// invoking DeliverPrompt.
func TestPrompt_NoSuchSession(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{
			Name:       "iris-test@other",
			InstanceID: "11111111-0000-1111-2222-333333333333",
			State:      "active",
			Role:       "worker",
			Worktree:   "/tmp/iris-test/other",
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	var deliverHits int
	deliver := func(_ context.Context, _, _, _ string, _ []string) error {
		deliverHits++
		return nil
	}
	sockPath := startPromptTestSocket(t, sessions, deliver)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runPromptAt(ctx, sockPath, "iris-test@does-not-exist", "hi", nil, &out)
	if err == nil {
		t.Fatalf("runPromptAt: want error for unknown session, got nil")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("error missing 'no such session' wording: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "iris-test@does-not-exist") {
		t.Errorf("error missing requested name: %q", err.Error())
	}
	if deliverHits != 0 {
		t.Errorf("DeliverPrompt invoked %d times on unknown-session path; want 0", deliverHits)
	}
}

// TestPrompt_DaemonNotRunning points the CLI at a non-existent socket and
// asserts the documented "daemon not running" wording.
func TestPrompt_DaemonNotRunning(t *testing.T) {
	shortPrefix, err := os.MkdirTemp("", "iris-prompt-no-daemon-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	// Path does not exist — fetchSessionsSnapshot will surface the canonical
	// "daemon not running" error before we ever try to dial for prompt_deliver.
	sockPath := filepath.Join(shortPrefix, "iris.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err = runPromptAt(ctx, sockPath, "iris-test@anything", "hi", nil, &out)
	if err == nil {
		t.Fatalf("runPromptAt: want daemon-not-running error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon not running") {
		t.Errorf("error missing 'daemon not running' wording: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("error missing 'systemctl --user start iris' hint: %q", err.Error())
	}
}

// TestPrompt_EmptyPrompt asserts the CLI refuses an empty prompt body with
// the documented multi-line "supply one of …" error.
func TestPrompt_EmptyPrompt(t *testing.T) {
	// We don't need a daemon for this — the empty-prompt check runs before
	// any wire activity. Use a never-listened-on path.
	shortPrefix, err := os.MkdirTemp("", "iris-prompt-empty-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })
	sockPath := filepath.Join(shortPrefix, "iris.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = runPromptAt(ctx, sockPath, "iris-test@whatever", "", nil, nil)
	if err == nil {
		t.Fatalf("runPromptAt: want error for empty prompt, got nil")
	}
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error missing 'a prompt is required' wording: %q", err.Error())
	}
}

// TestPrompt_DeliverError asserts that an error returned from DeliverPrompt
// is surfaced as the CLI error (the daemon emits an error frame and the
// readPromptAck path translates it). The session is in active state so the
// guard does not refuse.
func TestPrompt_DeliverError(t *testing.T) {
	const targetName = "iris-test@broken"

	sessions := []iris.SessionSnapshot{
		{
			Name:       targetName,
			InstanceID: "22222222-0000-1111-2222-333333333333",
			State:      "active",
			Role:       "worker",
			Worktree:   "/tmp/iris-test/broken",
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	deliver := func(_ context.Context, _, _, _ string, _ []string) error {
		return errSentinelDeliverFailed
	}

	sockPath := startPromptTestSocket(t, sessions, deliver)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runPromptAt(ctx, sockPath, targetName, "hi", nil, &out)
	if err == nil {
		t.Fatalf("runPromptAt: want delivery error, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic-deliver-failure") {
		t.Errorf("error missing daemon-side reason: %q", err.Error())
	}
}

// errSentinelDeliverFailed is a sentinel error used by TestPrompt_DeliverError
// to assert the CLI surfaces the daemon-side failure message verbatim.
var errSentinelDeliverFailed = sentinelErr("synthetic-deliver-failure")

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }
