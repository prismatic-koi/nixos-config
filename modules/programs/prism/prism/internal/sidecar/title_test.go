package sidecar

// Tests for once-per-session title generation.
//
// The frames are fed to handlePipeFrame directly rather than over a real
// socket: the trigger points live in that switch, and driving them straight
// keeps the tests focused on the guard logic instead of on transport.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// stubTitleGenerator records every call and returns a canned result. It is
// the injection seam for both the success and the failure paths.
type stubTitleGenerator struct {
	mu      sync.Mutex
	calls   []string
	title   string
	err     error
	blockOn chan struct{}
}

func (s *stubTitleGenerator) GenerateTitle(ctx context.Context, sourceText string) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, sourceText)
	block := s.blockOn
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	return s.title, s.err
}

func (s *stubTitleGenerator) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubTitleGenerator) callArgs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// newTitleTestSidecar builds an isolated sidecar for the given role, with
// the given spawn prompt and title generator, and seeds its agent_status row
// the way SpawnSession does.
func newTitleTestSidecar(t *testing.T, role, prompt string, gen TitleGenerator) (*Sidecar, *sidecartest.Bus) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	sessionName := "prism-test@title-" + role

	if err := bus.DB.UpsertStatusSeedRootAgentName(
		sessionName, "prism-test", t.TempDir(), "idle", nil, nil, role, "pi", "bwrap",
	); err != nil {
		t.Fatalf("seed agent_status: %v", err)
	}

	sc := New(Config{
		SessionName:    sessionName,
		Repo:           "prism-test",
		Worktree:       t.TempDir(),
		DB:             bus.DB,
		Clock:          newTestClock(),
		AgentRole:      role,
		HarnessName:    "pi",
		InitialPrompt:  prompt,
		TitleGenerator: gen,
		Harness:        pih.New("", "", ""),
	})
	return sc, bus
}

// feedTurnStart drives one turn_start frame through the real frame handler.
func feedTurnStart(t *testing.T, sc *Sidecar) {
	t.Helper()
	sc.handlePipeFrame([]byte(`{"type":"turn_start"}`))
}

// feedMsgUser drives one msg_user frame — the coordinator's title source
// — through the real frame handler.
func feedMsgUser(t *testing.T, sc *Sidecar, text string) {
	t.Helper()
	frame, err := json.Marshal(map[string]string{
		"type": "msg_user", "messageId": "m1", "text": text,
	})
	if err != nil {
		t.Fatalf("marshal msg_user frame: %v", err)
	}
	sc.handlePipeFrame(frame)
}

// TestTitleGen_WorkerIsTitledFromItsSpawnPrompt covers the AC: a newly
// spawned worker gets a generated title describing its task.
func TestTitleGen_WorkerIsTitledFromItsSpawnPrompt(t *testing.T) {
	gen := &stubTitleGenerator{title: "Generate session titles"}
	sc, bus := newTitleTestSidecar(t, "worker",
		"Please implement GitHub issue #2683 in this repo (nixos-config).\nRead it in full first.", gen)

	feedTurnStart(t, sc)
	sc.WaitNotifies()

	if got := gen.callCount(); got != 1 {
		t.Fatalf("model call count = %d, want 1", got)
	}
	if args := gen.callArgs(); !strings.Contains(args[0], "issue #2683") {
		t.Errorf("the generator was called with %q, want the spawn prompt", args[0])
	}

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Generate session titles" {
		t.Errorf("title = %v, want the generated title", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "generated" {
		t.Errorf("title_source = %v, want \"generated\"", st.TitleSource)
	}
	if st.IssueRef == nil || *st.IssueRef != "#2683" {
		t.Errorf("issue_ref = %v, want %q extracted from the spawn prompt", st.IssueRef, "#2683")
	}
}

// TestTitleGen_CoordinatorIsTitledFromItsFirstUserMessage covers the AC for
// the coordinator half. A coordinator has no spawn prompt, so its source
// text is the first thing the operator typed, delivered as a msg_user frame.
func TestTitleGen_CoordinatorIsTitledFromItsFirstUserMessage(t *testing.T) {
	gen := &stubTitleGenerator{title: "Triage the merge queue"}
	sc, bus := newTitleTestSidecar(t, "coordinator", "", gen)

	// A coordinator's turn_start carries no prompt, so it must not consume
	// the single attempt.
	feedTurnStart(t, sc)
	sc.WaitNotifies()
	if got := gen.callCount(); got != 0 {
		t.Fatalf("model call count after a promptless turn_start = %d, want 0", got)
	}

	feedMsgUser(t, sc, "Can you triage the merge queue and land PLAT-42 please")
	sc.WaitNotifies()

	if got := gen.callCount(); got != 1 {
		t.Fatalf("model call count = %d, want 1", got)
	}

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Triage the merge queue" {
		t.Errorf("title = %v, want the generated title", st.Title)
	}
	if st.IssueRef == nil || *st.IssueRef != "PLAT-42" {
		t.Errorf("issue_ref = %v, want %q", st.IssueRef, "PLAT-42")
	}
}

// TestTitleGen_ExactlyOneModelCallPerSession covers the AC directly: a
// session that runs 200 turns triggers one call, not 200.
func TestTitleGen_ExactlyOneModelCallPerSession(t *testing.T) {
	gen := &stubTitleGenerator{title: "Some title"}
	sc, _ := newTitleTestSidecar(t, "worker", "Fix the login bug in #42", gen)

	for range 200 {
		feedTurnStart(t, sc)
	}
	// Interleave the coordinator trigger too — the two entry points share
	// one budget, not one each.
	for range 10 {
		feedMsgUser(t, sc, "another user message entirely")
	}
	sc.WaitNotifies()

	if got := gen.callCount(); got != 1 {
		t.Fatalf("model call count after 200 turns and 10 user messages = %d, want exactly 1", got)
	}
}

// TestTitleGen_ReviewAgentsAreNeverTitled covers the AC that review agents
// are excluded and never trigger a model call. The discriminator is
// root_agent_name, which is what the assertion reads back.
func TestTitleGen_ReviewAgentsAreNeverTitled(t *testing.T) {
	for _, role := range []string{
		"review-goal", "review-code", "review-security", "review-qa", "review-context",
	} {
		t.Run(role, func(t *testing.T) {
			gen := &stubTitleGenerator{title: "should never be written"}
			sc, bus := newTitleTestSidecar(t, role, "Review PR #2683 for correctness", gen)

			for range 20 {
				feedTurnStart(t, sc)
				feedMsgUser(t, sc, "reviewing PR #2683 now")
			}
			sc.WaitNotifies()

			if got := gen.callCount(); got != 0 {
				t.Errorf("model call count for a %s session = %d, want 0", role, got)
			}

			st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			// The discriminator, asserted explicitly.
			if st.RootAgentName == nil || *st.RootAgentName != role {
				t.Fatalf("root_agent_name = %v, want %q", st.RootAgentName, role)
			}
			if st.Title != nil {
				t.Errorf("title = %q, want NULL — review agents are never titled", *st.Title)
			}
			if st.TitleSource != nil {
				t.Errorf("title_source = %q, want NULL", *st.TitleSource)
			}
			if st.IssueRef != nil {
				t.Errorf("issue_ref = %q, want NULL", *st.IssueRef)
			}
		})
	}
}

// TestTitleGen_FailingClientFallsBackAndDoesNotBlock covers the edge-case
// AC: a failed, slow, or unauthenticated model call must not block the turn,
// and the session falls back to the deterministic derivation.
func TestTitleGen_FailingClientFallsBackAndDoesNotBlock(t *testing.T) {
	gen := &stubTitleGenerator{err: errors.New("Anthropic API returned HTTP 401")}
	sc, bus := newTitleTestSidecar(t, "worker",
		"Please implement GitHub issue #2683 in this repo\nmore detail here", gen)

	feedTurnStart(t, sc)
	// The turn itself must have completed already — handlePipeFrame returns
	// without waiting on the generator. WaitNotifies only settles the
	// background write so the assertions below are deterministic.
	sc.WaitNotifies()

	if got := gen.callCount(); got != 1 {
		t.Fatalf("model call count = %d, want 1", got)
	}

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	// deriveFallbackTitle's derivation: the first non-blank line.
	const wantFallback = "Please implement GitHub issue #2683 in this repo"
	if st.Title == nil || *st.Title != wantFallback {
		t.Errorf("title = %v, want the deterministic fallback %q", st.Title, wantFallback)
	}
	// The issue reference is extracted from the source text, not from the
	// model, so a failed call must not cost it.
	if st.IssueRef == nil || *st.IssueRef != "#2683" {
		t.Errorf("issue_ref = %v, want %q even though the model call failed", st.IssueRef, "#2683")
	}
}

// TestTitleGen_NilGeneratorStillWritesFallbackAndIssueRef covers the host
// with no credentials: cmd/sidecar.go passes a nil generator, the model call
// is skipped entirely, and the session is still titled deterministically.
func TestTitleGen_NilGeneratorStillWritesFallbackAndIssueRef(t *testing.T) {
	sc, bus := newTitleTestSidecar(t, "worker", "Upgrade the ingress controller for PROJ-7", nil)

	feedTurnStart(t, sc)
	sc.WaitNotifies()

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Upgrade the ingress controller for PROJ-7" {
		t.Errorf("title = %v, want the deterministic fallback", st.Title)
	}
	if st.IssueRef == nil || *st.IssueRef != "PROJ-7" {
		t.Errorf("issue_ref = %v, want %q", st.IssueRef, "PROJ-7")
	}
}

// TestTitleGen_NoIssueReferenceLeavesIssueRefNull covers the edge-case AC:
// source text containing no reference leaves issue_ref NULL, never a
// guessed value.
func TestTitleGen_NoIssueReferenceLeavesIssueRefNull(t *testing.T) {
	gen := &stubTitleGenerator{title: "Refactor the login flow"}
	sc, bus := newTitleTestSidecar(t, "worker", "Refactor the login flow and add tests", gen)

	feedTurnStart(t, sc)
	sc.WaitNotifies()

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.IssueRef != nil {
		t.Errorf("issue_ref = %q, want NULL for source text with no reference", *st.IssueRef)
	}
	if st.Title == nil {
		t.Error("title is NULL, want the generated title to be written regardless")
	}
}

// TestTitleGen_ModelIssueNumberIsIgnored is the anti-hallucination
// assertion. The model is given text naming issue #2683 and replies with a
// title that names a DIFFERENT issue. The stored issue_ref must come from
// the source text, never from the reply — a wrong reference silently
// misattributes work.
func TestTitleGen_ModelIssueNumberIsIgnored(t *testing.T) {
	gen := &stubTitleGenerator{title: "Fix issue #9999 in the parser"}
	sc, bus := newTitleTestSidecar(t, "worker", "Please implement GitHub issue #2683", gen)

	feedTurnStart(t, sc)
	sc.WaitNotifies()

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.IssueRef == nil {
		t.Fatal("issue_ref is NULL, want the reference from the source text")
	}
	if *st.IssueRef == "#9999" {
		t.Fatal("issue_ref was taken from the model's reply — it must be extracted from the source text only")
	}
	if *st.IssueRef != "#2683" {
		t.Errorf("issue_ref = %q, want %q from the source text", *st.IssueRef, "#2683")
	}
}

// TestTitleGen_ControlBytesAreStrippedFromUntrustedSources covers the
// security AC. Both the source text and the model reply are untrusted — a
// spawn prompt routinely quotes an issue body, and the model is a remote
// party — and the title column is rendered verbatim in the tmux dashboard.
func TestTitleGen_ControlBytesAreStrippedFromUntrustedSources(t *testing.T) {
	t.Run("from the model reply", func(t *testing.T) {
		gen := &stubTitleGenerator{title: "\x1b]0;pwned\x07Real title\x00 here"}
		sc, bus := newTitleTestSidecar(t, "worker", "a task with issue #1", gen)
		feedTurnStart(t, sc)
		sc.WaitNotifies()

		st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
		if err != nil {
			t.Fatalf("CurrentStatus: %v", err)
		}
		if st.Title == nil {
			t.Fatal("title is NULL")
		}
		assertNoControlBytes(t, *st.Title)
		if !strings.Contains(*st.Title, "Real title here") {
			t.Errorf("title = %q, want the legitimate text retained", *st.Title)
		}
	})

	t.Run("from the spawn prompt on the fallback path", func(t *testing.T) {
		gen := &stubTitleGenerator{err: errors.New("boom")}
		sc, bus := newTitleTestSidecar(t, "worker",
			"Fix \x1b[31mlogin\x1b[0m bug\x07 in #42", gen)
		feedTurnStart(t, sc)
		sc.WaitNotifies()

		st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
		if err != nil {
			t.Fatalf("CurrentStatus: %v", err)
		}
		if st.Title == nil {
			t.Fatal("title is NULL")
		}
		assertNoControlBytes(t, *st.Title)
		if st.IssueRef == nil {
			t.Fatal("issue_ref is NULL")
		}
		assertNoControlBytes(t, *st.IssueRef)
	})

	t.Run("from an untrusted coordinator message", func(t *testing.T) {
		gen := &stubTitleGenerator{err: errors.New("boom")}
		sc, bus := newTitleTestSidecar(t, "coordinator", "", gen)
		feedMsgUser(t, sc, "Review this issue body:\x1b]0;evil\x07 handle PROJ-9")
		sc.WaitNotifies()

		st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
		if err != nil {
			t.Fatalf("CurrentStatus: %v", err)
		}
		if st.Title == nil {
			t.Fatal("title is NULL")
		}
		assertNoControlBytes(t, *st.Title)
	})
}

func assertNoControlBytes(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			t.Errorf("%q carries control byte %#x", s, r)
		}
	}
}

// TestTitleGen_HumanTitleIsNeverOverwritten covers the AC end-to-end
// through the sidecar, not only at the DB layer.
func TestTitleGen_HumanTitleIsNeverOverwritten(t *testing.T) {
	gen := &stubTitleGenerator{title: "A model title"}
	sc, bus := newTitleTestSidecar(t, "worker", "Fix the login bug", gen)

	// The operator renames the session; pi reports it as a harness title.
	if err := bus.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", ptrTo("Operator chose this"), nil,
	); err != nil {
		t.Fatalf("harness rename: %v", err)
	}

	feedTurnStart(t, sc)
	sc.WaitNotifies()

	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Operator chose this" {
		t.Errorf("title = %v, want the operator's title untouched", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "human" {
		t.Errorf("title_source = %v, want \"human\"", st.TitleSource)
	}
}

// TestTitleGen_SlowCallDoesNotBlockTheTurn asserts the non-blocking property
// structurally: with the generator wedged, handlePipeFrame still returns and
// subsequent frames are still processed.
func TestTitleGen_SlowCallDoesNotBlockTheTurn(t *testing.T) {
	block := make(chan struct{})
	gen := &stubTitleGenerator{title: "eventually", blockOn: block}
	sc, bus := newTitleTestSidecar(t, "worker", "Fix the login bug in #42", gen)

	feedTurnStart(t, sc)
	// The generator is still blocked. If the title path were synchronous,
	// the call above would not have returned and we would never get here.
	for range 5 {
		feedTurnStart(t, sc)
	}
	// The state write for the turn landed despite the wedged generator.
	st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus while the generator is blocked: %v", err)
	}
	if st.State != "active" {
		t.Errorf("state = %q while the title call is in flight, want \"active\"", st.State)
	}

	close(block)
	sc.WaitNotifies()

	if got := gen.callCount(); got != 1 {
		t.Errorf("model call count = %d, want 1 (a wedged call must not be re-entered)", got)
	}
}

// TestTitleGen_BlankSourceDoesNotConsumeTheAttempt verifies a promptless
// turn_start leaves the single attempt available for the real source text
// that arrives later. Without this, a coordinator would be untitled for good.
func TestTitleGen_BlankSourceDoesNotConsumeTheAttempt(t *testing.T) {
	gen := &stubTitleGenerator{title: "Real title"}
	sc, _ := newTitleTestSidecar(t, "coordinator", "", gen)

	for range 5 {
		feedTurnStart(t, sc)
		feedMsgUser(t, sc, "   \n\t  ")
	}
	sc.WaitNotifies()
	if got := gen.callCount(); got != 0 {
		t.Fatalf("model call count on blank input = %d, want 0", got)
	}

	feedMsgUser(t, sc, "Now do the real thing for #123")
	sc.WaitNotifies()
	if got := gen.callCount(); got != 1 {
		t.Fatalf("model call count after real text arrived = %d, want 1", got)
	}
}

// TestTitleGen_RejectedReplyFallsBackAndDoesNotBlock covers the rejected-reply
// edge cases: a reply that is not title-shaped must be rejected via
// titlegen.IsRejected, and the caller must fall back to the deterministic
// title exactly as it does for a transport error -- never a retry, never a
// blocked turn.
func TestTitleGen_RejectedReplyFallsBackAndDoesNotBlock(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{
			"the observed failure: a conversational refusal",
			"I need a task description to create a title. Could you share what issue 2458 is about?",
		},
		{"a bare question", "What is this task about?"},
		{"a reply over the title budget", strings.Repeat("very long title ", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gen := &stubTitleGenerator{title: tc.reply}
			sc, bus := newTitleTestSidecar(t, "coordinator", "", gen)

			feedMsgUser(t, sc, "can we get started on issue 2458?")
			sc.WaitNotifies()

			if got := gen.callCount(); got != 1 {
				t.Fatalf("model call count = %d, want exactly 1 (a rejected reply must not be retried)", got)
			}

			st, err := bus.DB.CurrentStatus(sc.cfg.SessionName)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			const wantFallback = "can we get started on issue 2458?"
			if st.Title == nil || *st.Title != wantFallback {
				t.Errorf("title = %v, want the deterministic fallback %q", st.Title, wantFallback)
			}
			if strings.HasSuffix(*st.Title, "?") && *st.Title != wantFallback {
				t.Errorf("title = %q, a rejected reply must never reach the title column", *st.Title)
			}
		})
	}
}

func ptrTo(s string) *string { return &s }
