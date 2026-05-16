package main

// escalate_integration_test.go — integration tests for `iris escalate`
// against a real iris.ClientSocket (issue #1693). Each test stands up an
// in-process ClientSocket whose escalate / resume / deliverPrompt / roleOf
// hooks are stubs we can observe, then drives runEscalateAt against the
// live socket path. This proves the full wire path:
//
//	CLI                 →  escalation_deliver frame on iris.sock
//	daemon ClientSocket →  escalation_delivered ack, calls hooks
//
// We do not run a real pi child — the hooks are recorders.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// escCallRecorder accumulates the hook invocations the test asserts on.
type escCallRecorder struct {
	mu              sync.Mutex
	escalateCalls   []string
	resumeCalls     []string
	deliverCalls    []string
	deliverPrompts  []string
	deliverError    error
}

// startEscalateTestSocket starts an iris.ClientSocket on a tempdir socket
// path backed by `sessions` and the recorder hooks. Returns the socket path
// and the recorder.
func startEscalateTestSocket(t *testing.T, sessions []iris.SessionSnapshot, roles map[string]string) (string, *escCallRecorder) {
	t.Helper()
	shortPrefix, err := os.MkdirTemp("", "iris-esc-cli-")
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
	rec := &escCallRecorder{}

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			out := make([]iris.SessionSnapshot, len(sessions))
			copy(out, sessions)
			return out
		},
		DeliverPrompt: func(_ context.Context, name, text, _ string, _ []string) error {
			rec.mu.Lock()
			rec.deliverCalls = append(rec.deliverCalls, name)
			rec.deliverPrompts = append(rec.deliverPrompts, text)
			err := rec.deliverError
			rec.mu.Unlock()
			return err
		},
		EscalateSession: func(name string) error {
			rec.mu.Lock()
			rec.escalateCalls = append(rec.escalateCalls, name)
			rec.mu.Unlock()
			return nil
		},
		ResumeSession: func(name string) {
			rec.mu.Lock()
			rec.resumeCalls = append(rec.resumeCalls, name)
			rec.mu.Unlock()
		},
		RoleOf: func(name string) string { return roles[name] },
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(cs.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.Serve(ctx)

	return sockPath, rec
}

// TestEscalate_HappyPath drives the full wire path: CLI sends
// escalation_deliver, daemon auto-discovers the lone coordinator, escalate
// + deliver hooks fire, ack arrives, CLI prints confirmation.
func TestEscalate_HappyPath(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
	}
	sockPath, rec := startEscalateTestSocket(t, sessions, roles)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runEscalateAt(ctx, sockPath, "iris-worker@feat", "", "should I merge?", &out)
	if err != nil {
		t.Fatalf("runEscalateAt: %v", err)
	}

	stdout := out.String()
	if !strings.Contains(stdout, "delivered to iris-coord@main") {
		t.Errorf("expected 'delivered to iris-coord@main' in stdout; got %q", stdout)
	}
	if !strings.Contains(stdout, "escalated") {
		t.Errorf("expected confirmation to mention 'escalated' state; got %q", stdout)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.escalateCalls) != 1 || rec.escalateCalls[0] != "iris-worker@feat" {
		t.Errorf("escalateCalls = %+v", rec.escalateCalls)
	}
	if len(rec.deliverCalls) != 1 || rec.deliverCalls[0] != "iris-coord@main" {
		t.Errorf("deliverCalls = %+v", rec.deliverCalls)
	}
	if len(rec.deliverPrompts) != 1 || rec.deliverPrompts[0] != "should I merge?" {
		t.Errorf("deliverPrompts = %+v", rec.deliverPrompts)
	}
}

// TestEscalate_ZeroCoordinators covers the no-coordinator branch from the
// CLI side: the daemon acks with delivered=false; the CLI exits 0 and
// prints the human-pick-up message.
func TestEscalate_ZeroCoordinators(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@solo", State: "active", Role: "worker"},
	}
	roles := map[string]string{"iris-worker@solo": "worker"}
	sockPath, rec := startEscalateTestSocket(t, sessions, roles)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runEscalateAt(ctx, sockPath, "iris-worker@solo", "", "lonely", &out)
	if err != nil {
		t.Fatalf("runEscalateAt: %v", err)
	}
	stdout := out.String()
	if !strings.Contains(stdout, "no coordinator found") {
		t.Errorf("expected 'no coordinator found' message; got %q", stdout)
	}
	if !strings.Contains(stdout, "please wait for a human") {
		t.Errorf("expected human pick-up message; got %q", stdout)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.escalateCalls) != 1 {
		t.Errorf("expected exactly one escalate call (transition still happens); got %+v", rec.escalateCalls)
	}
	if len(rec.deliverCalls) != 0 {
		t.Errorf("expected no deliver calls; got %+v", rec.deliverCalls)
	}
}

// TestEscalate_MultipleCoordinators_RequiresTo asserts that with multiple
// coordinators and no --to, the daemon returns an error frame which the
// CLI surfaces as a non-zero exit. The candidates are listed in the error.
func TestEscalate_MultipleCoordinators_RequiresTo(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
		{Name: "iris-coord@alt", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
		"iris-coord@alt":   "coordinator",
	}
	sockPath, rec := startEscalateTestSocket(t, sessions, roles)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runEscalateAt(ctx, sockPath, "iris-worker@feat", "", "which one?", &out)
	if err == nil {
		t.Fatalf("expected error for multiple-candidate auto-discovery, got nil (stdout=%q)", out.String())
	}
	if !strings.Contains(err.Error(), "multiple coordinator candidates") {
		t.Errorf("error missing 'multiple coordinator candidates': %q", err.Error())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.escalateCalls) != 0 {
		t.Errorf("expected zero escalate calls on ambiguous discovery; got %+v", rec.escalateCalls)
	}
}

// TestEscalate_ExplicitTo covers --to from the CLI side.
func TestEscalate_ExplicitTo(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
		{Name: "iris-coord@alt", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
		"iris-coord@alt":   "coordinator",
	}
	sockPath, rec := startEscalateTestSocket(t, sessions, roles)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	if err := runEscalateAt(ctx, sockPath, "iris-worker@feat", "iris-coord@alt", "explicit", &out); err != nil {
		t.Fatalf("runEscalateAt: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deliverCalls) != 1 || rec.deliverCalls[0] != "iris-coord@alt" {
		t.Errorf("deliverCalls = %+v, want exactly [iris-coord@alt]", rec.deliverCalls)
	}
}

// TestEscalate_DaemonNotRunning points the CLI at a non-existent socket
// and asserts the canonical wording.
func TestEscalate_DaemonNotRunning(t *testing.T) {
	shortPrefix, err := os.MkdirTemp("", "iris-esc-no-daemon-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })
	sockPath := filepath.Join(shortPrefix, "iris.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err = runEscalateAt(ctx, sockPath, "iris-worker@x", "", "hi", &out)
	if err == nil {
		t.Fatalf("runEscalateAt: want daemon-not-running error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon not running") {
		t.Errorf("error missing 'daemon not running' wording: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("error missing 'systemctl --user start iris' hint: %q", err.Error())
	}
}

// TestEscalate_EmptyPrompt asserts the multi-line "supply one of" error.
func TestEscalate_EmptyPrompt(t *testing.T) {
	shortPrefix, err := os.MkdirTemp("", "iris-esc-empty-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })
	sockPath := filepath.Join(shortPrefix, "iris.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = runEscalateAt(ctx, sockPath, "iris-worker@x", "", "", nil)
	if err == nil {
		t.Fatalf("expected empty-prompt error, got nil")
	}
	if !strings.Contains(err.Error(), "a prompt is required") {
		t.Errorf("error missing 'a prompt is required': %q", err.Error())
	}
}

// TestEscalate_UnregisteredCaller covers the caller-not-registered branch.
func TestEscalate_UnregisteredCaller(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{"iris-coord@main": "coordinator"}
	sockPath, _ := startEscalateTestSocket(t, sessions, roles)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runEscalateAt(ctx, sockPath, "iris-worker@ghost", "", "hi", &out)
	if err == nil {
		t.Fatalf("expected unregistered-caller error, got nil")
	}
	if !strings.Contains(err.Error(), "not a registered iris session") {
		t.Errorf("error missing canonical wording: %q", err.Error())
	}
}

// TestEscalate_MidSendDrop simulates a daemon that closes the connection
// after accepting the frame but before sending the ack. The CLI must exit
// non-zero with "lost connection" and not hang.
func TestEscalate_MidSendDrop(t *testing.T) {
	shortPrefix, err := os.MkdirTemp("", "iris-esc-drop-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })
	sockPath := filepath.Join(shortPrefix, "iris.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read at most one line, then close abruptly to simulate a
			// daemon that drops the connection mid-protocol.
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err = runEscalateAt(ctx, sockPath, "iris-worker@x", "", "hi", &out)
	if err == nil {
		t.Fatalf("expected lost-connection error, got nil")
	}
	if !strings.Contains(err.Error(), "lost connection") {
		t.Errorf("error missing 'lost connection': %q", err.Error())
	}
}

// TestEscalate_ResumeFlow is the full escalate-then-resume integration
// test required by the AC. It:
//
//  1. Spawns a CLI escalation against the daemon, asserts the escalate
//     hook fired and the deliverPrompt hook delivered the body to the
//     coordinator.
//  2. Sends a follow-up prompt_deliver to the same worker via the daemon
//     socket directly and asserts the resume hook fired.
//
// Together these prove the active→escalated→active round-trip the
// state-machine AC requires.
func TestEscalate_ResumeFlow(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
	}
	sockPath, rec := startEscalateTestSocket(t, sessions, roles)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Step 1: escalate.
	var out bytes.Buffer
	if err := runEscalateAt(ctx, sockPath, "iris-worker@feat", "", "needs guidance", &out); err != nil {
		t.Fatalf("runEscalateAt: %v", err)
	}
	rec.mu.Lock()
	if len(rec.escalateCalls) != 1 {
		t.Fatalf("escalateCalls = %+v, want 1", rec.escalateCalls)
	}
	if len(rec.deliverCalls) != 1 {
		t.Fatalf("deliverCalls = %+v, want 1", rec.deliverCalls)
	}
	rec.mu.Unlock()

	// Step 2: send a prompt_deliver to the worker via the daemon socket.
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	pd := iris.ClientPromptDeliverFrame{
		Type: iris.ClientFramePromptDeliver,
		Name: "iris-worker@feat",
		Text: "carry on",
	}
	data, _ := json.Marshal(pd)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write prompt_deliver: %v", err)
	}

	// Wait for the resume hook to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.resumeCalls)
		rec.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.resumeCalls) != 1 || rec.resumeCalls[0] != "iris-worker@feat" {
		t.Errorf("resumeCalls = %+v, want [iris-worker@feat]", rec.resumeCalls)
	}
	// The follow-up prompt_deliver also produces a deliverPrompt call.
	if len(rec.deliverCalls) != 2 {
		t.Errorf("deliverCalls = %+v, want 2 (escalation + resume prompt)", rec.deliverCalls)
	}
}

// TestEscalate_DeliveryError surfaces the daemon-side delivery failure as a
// non-zero CLI exit.
func TestEscalate_DeliveryError(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
	}
	sockPath, rec := startEscalateTestSocket(t, sessions, roles)
	rec.deliverError = errors.New("synthetic-deliver-failure")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := runEscalateAt(ctx, sockPath, "iris-worker@feat", "", "hi", &out)
	if err == nil {
		t.Fatalf("expected delivery error, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic-deliver-failure") {
		t.Errorf("error missing underlying cause: %q", err.Error())
	}
}

// verifyHelpText covers the AC requirement that `iris escalate --help`
// documents the auto-discovery rule, the state machine, and the input
// conventions. We assert presence of the key phrases rather than exact
// wording so future doc improvements don't require test churn.
func TestEscalate_HelpDocumentsContract(t *testing.T) {
	long := escalateCmd.Long
	wantPhrases := []string{
		"auto-discover",
		"--to",
		"escalated",
		"active",
		"--prompt",
		"stdin",
		"coordinator",
	}
	for _, p := range wantPhrases {
		if !strings.Contains(strings.ToLower(long), strings.ToLower(p)) {
			t.Errorf("escalateCmd.Long missing %q\nLong text:\n%s", p, long)
		}
	}
}

