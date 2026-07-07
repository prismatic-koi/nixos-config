package session

// Tests for the durable spawn-intent / spawn-failed events written by
// SpawnSession (#2364 B5 + B7). These cover the acceptance criteria:
//
//   - Every spawn attempt that reaches SpawnSession writes a session.spawn_intent
//     event carrying session name and instance_id.
//   - A spawn that fails after the intent event writes a session.spawn_failed
//     event naming the failing step.
//   - When opts.InvokerSession is set, a bus_messages audit row is written on
//     the failure path.
//   - When opts.InvokerSession is empty (bare CLI spawn), no bus message is
//     written and the spawn does not error on that account.
//   - Successful spawns emit no session.spawn_failed event.
//   - Event-write failures are non-fatal (asserted structurally via the
//     best-effort helpers — no stub is provided, but the code paths are
//     wrapped in warn-and-continue idioms; a real DB failure here would still
//     let SpawnSession return the underlying spawn error rather than a
//     telemetry error).

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSpawnSession_WritesSpawnIntentEvent verifies that every successful
// LayoutAgentOnly spawn writes a session.spawn_intent event carrying
// session name, instance_id, invoker, agent role, and layout.
func TestSpawnSession_WritesSpawnIntentEvent(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@main~worker-intent"
	const invoker = "prism-test@main"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       "/worktrees/prism-test",
		AgentRole:      "worker",
		Prompt:         "do the thing",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
		InvokerSession: invoker,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	events, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnIntent})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d", EventSpawnIntent, len(events))
	}
	ev := events[0]
	if ev.SessionName != sessionName {
		t.Errorf("event session_name = %q, want %q", ev.SessionName, sessionName)
	}
	if ev.InstanceID == nil || *ev.InstanceID == "" {
		t.Error("event instance_id is nil/empty; want the host-minted UUID")
	}
	var payload spawnEventPayload
	if err := json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v (raw=%s)", err, ev.Payload)
	}
	if payload.SessionName != sessionName {
		t.Errorf("payload session_name = %q, want %q", payload.SessionName, sessionName)
	}
	if payload.InstanceID == "" {
		t.Error("payload instance_id is empty; want the host-minted UUID")
	}
	if payload.InvokerSession != invoker {
		t.Errorf("payload invoker_session = %q, want %q", payload.InvokerSession, invoker)
	}
	if payload.AgentRole != "worker" {
		t.Errorf("payload agent_role = %q, want %q", payload.AgentRole, "worker")
	}
	if payload.Layout != "agent-only" {
		t.Errorf("payload layout = %q, want %q", payload.Layout, "agent-only")
	}
	if payload.OccurredAt == "" {
		t.Error("payload occurred_at is empty")
	}
	// A successful spawn must not emit a session.spawn_failed event.
	failed, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnFailed})
	if err != nil {
		t.Fatalf("QueryEvents (failed): %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("successful spawn emitted %d %s events; want 0", len(failed), EventSpawnFailed)
	}
}

// TestSpawnSession_WritesSpawnIntentEvent_BareCLISpawn verifies that when
// opts.InvokerSession is empty (bare `prism spawn` outside a session), the
// spawn still succeeds and the durable event is written without an invoker.
func TestSpawnSession_WritesSpawnIntentEvent_BareCLISpawn(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@main~bare-worker"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       "/worktrees/prism-test",
		AgentRole:      "worker",
		Prompt:         "bare spawn",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
		// InvokerSession intentionally left empty.
	}
	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	events, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnIntent})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d", EventSpawnIntent, len(events))
	}
	var payload spawnEventPayload
	if err := json.Unmarshal([]byte(events[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.InvokerSession != "" {
		t.Errorf("payload invoker_session = %q, want empty for bare CLI spawn", payload.InvokerSession)
	}
}

// TestSpawnSession_LayoutFailure_WritesSpawnFailedEvent verifies that when
// the tmux/layout step fails, SpawnSession writes a durable
// session.spawn_failed event naming step "layout" and carrying the failing
// error text. The session.spawn_intent event must still be present (it was
// written before the failure).
func TestSpawnSession_LayoutFailure_WritesSpawnFailedEvent(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	failTmuxBin(t, "duplicate session (injected by test)")
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@main~layout-fail-events"
	const invoker = "prism-test@main"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       "/worktrees/prism-test",
		AgentRole:      "review-code",
		Prompt:         "review this PR",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
		InvokerSession: invoker,
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession with failing tmux: got nil error, want error")
	}

	// The intent event was written before the failure.
	intent, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnIntent})
	if err != nil {
		t.Fatalf("QueryEvents (intent): %v", err)
	}
	if len(intent) != 1 {
		t.Errorf("want exactly 1 %s event, got %d", EventSpawnIntent, len(intent))
	}

	// The failure event must be written and name step="layout".
	failed, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnFailed})
	if err != nil {
		t.Fatalf("QueryEvents (failed): %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d", EventSpawnFailed, len(failed))
	}
	var payload spawnEventPayload
	if err := json.Unmarshal([]byte(failed[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal failed payload: %v", err)
	}
	if payload.FailingStep != "layout" {
		t.Errorf("payload failing_step = %q, want %q", payload.FailingStep, "layout")
	}
	if payload.Error == "" {
		t.Error("payload error is empty; want the failing tmux error text")
	}
	if payload.InvokerSession != invoker {
		t.Errorf("payload invoker_session = %q, want %q", payload.InvokerSession, invoker)
	}
	if payload.SessionName != sessionName {
		t.Errorf("payload session_name = %q, want %q", payload.SessionName, sessionName)
	}
}

// TestSpawnSession_LayoutFailure_WritesBusMessage verifies that when a spawn
// fails AND opts.InvokerSession is non-empty, a bus_messages row is written
// naming the invoker as to_session and describing the failure.
func TestSpawnSession_LayoutFailure_WritesBusMessage(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	failTmuxBin(t, "duplicate session (injected by test)")
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@main~layout-fail-bus"
	const invoker = "prism-test@main"
	// Seed the invoker's agent_status so CurrentStatus returns a row with
	// an instance_id — writeSpawnFailedEvent uses it to populate
	// to_instance_id on the bus row.
	invokerInstance := "invoker-instance-uuid"
	if err := d.UpsertStatusSeedRootAgentName(invoker, "prism-test", "/worktrees/prism-test", "idle", nil, nil, "coordinator", "pi", "host"); err != nil {
		t.Fatalf("seed invoker status: %v", err)
	}
	if err := d.SetInstanceID(invoker, invokerInstance); err != nil {
		t.Fatalf("SetInstanceID invoker: %v", err)
	}
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       "/worktrees/prism-test",
		AgentRole:      "review-code",
		Prompt:         "review this PR",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
		InvokerSession: invoker,
	}
	if err := SpawnSession(d, opts); err == nil {
		t.Fatal("SpawnSession: want error, got nil")
	}

	// Assert a bus_messages row exists with the correct fields. Use raw SQL
	// because bus.go exposes no direct query for "row by from_session".
	var toSession, fromSession, text string
	var toInstance *string
	row := d.QueryRow(
		"SELECT from_session, to_session, to_instance_id, text FROM bus_messages WHERE from_session = ? AND to_session = ?",
		sessionName, invoker,
	)
	if err := row.Scan(&fromSession, &toSession, &toInstance, &text); err != nil {
		t.Fatalf("query bus_messages: %v", err)
	}
	if toSession != invoker {
		t.Errorf("to_session = %q, want %q", toSession, invoker)
	}
	if fromSession != sessionName {
		t.Errorf("from_session = %q, want %q", fromSession, sessionName)
	}
	if toInstance == nil || *toInstance != invokerInstance {
		got := "<nil>"
		if toInstance != nil {
			got = *toInstance
		}
		t.Errorf("to_instance_id = %q, want %q", got, invokerInstance)
	}
	if !strings.Contains(text, sessionName) || !strings.Contains(text, "layout") {
		t.Errorf("bus text = %q; want to mention session name %q and step %q", text, sessionName, "layout")
	}
}

// TestSpawnSession_LayoutFailure_NoInvoker_NoBusMessage verifies the
// bare-CLI-spawn edge case: when opts.InvokerSession is empty and the layout
// fails, the durable session.spawn_failed event is still written but NO
// bus_messages row is created and SpawnSession does not error on that account.
func TestSpawnSession_LayoutFailure_NoInvoker_NoBusMessage(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	failTmuxBin(t, "duplicate session (injected by test)")
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@main~bare-fail"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       "/worktrees/prism-test",
		AgentRole:      "worker",
		Prompt:         "bare spawn that will fail",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
		// InvokerSession intentionally left empty.
	}
	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession: want error, got nil")
	}
	// The error should be the underlying layout error, not a telemetry error.
	if !strings.Contains(err.Error(), "duplicate session") && !strings.Contains(err.Error(), "prism cleanup") {
		t.Errorf("error %q does not contain the expected layout-failure text", err.Error())
	}

	// Durable failure event written.
	failed, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnFailed})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d", EventSpawnFailed, len(failed))
	}
	var payload spawnEventPayload
	if err := json.Unmarshal([]byte(failed[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.InvokerSession != "" {
		t.Errorf("payload invoker_session = %q, want empty for bare CLI spawn", payload.InvokerSession)
	}

	// No bus_messages row addressed FROM this session.
	var count int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE from_session = ?", sessionName,
	).Scan(&count); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if count != 0 {
		t.Errorf("bare CLI spawn failure wrote %d bus_messages row(s); want 0", count)
	}
}

// TestSpawnSession_SuccessfulSpawn_NoFailedEvent verifies the AC edge case
// that successful spawns emit no session.spawn_failed event, even when the
// invoker is set. (The intent-event assertion is in
// TestSpawnSession_WritesSpawnIntentEvent above; this test is the negative
// twin.)
func TestSpawnSession_SuccessfulSpawn_NoFailedEvent(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@main~ok"
	const invoker = "prism-test@main"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       "/worktrees/prism-test",
		AgentRole:      "worker",
		Prompt:         "do the thing",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
		InvokerSession: invoker,
	}
	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	failed, err := d.QueryEvents(sessionName, 0, nil, nil, []string{EventSpawnFailed})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("successful spawn emitted %d %s events; want 0", len(failed), EventSpawnFailed)
	}
	var count int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE from_session = ?", sessionName,
	).Scan(&count); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if count != 0 {
		t.Errorf("successful spawn wrote %d bus_messages row(s); want 0", count)
	}
}
