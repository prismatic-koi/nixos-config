package sidecar

// title.go — once-per-session title generation (issue #2683).
//
// What it does
// ------------
//
// On the FIRST turn of an eligible session, take the text that motivated the
// session, ask a cheap model for a one-line title, extract any issue or
// ticket reference from the same text by regex, and write both to
// agent_status. After that the session is titled and this file does nothing
// for the rest of its life.
//
// Where the source text comes from differs by role, which is the only
// asymmetry here:
//
//   - A WORKER is spawned with a prompt, so cfg.InitialPrompt already holds
//     the task description before the agent has said a word. The trigger is
//     the first turn_start.
//   - A COORDINATOR has no spawn prompt. Its motivating text is whatever the
//     operator typed first, which reaches prism as a msg_user frame (#2678).
//     The trigger is that frame.
//
// Whichever arrives first wins; the guard below means the other is a no-op.
//
// Exactly one model call per session
// ----------------------------------
//
// Two guards, and they cover different failure modes:
//
//   - titleGenAttempted (in-memory) bounds the SIDECAR PROCESS to one
//     attempt. A session that runs 200 turns fires 200 turn_start frames and
//     makes one call. This is the guard that matters for cost, and it is set
//     BEFORE the request is issued, so a slow call cannot be double-started
//     by the next turn.
//   - db.SetGeneratedTitle's SQL guard bounds the ROW: it refuses to
//     overwrite a title whose source is 'human'. This is the guard that
//     matters for correctness, and it lives in SQL so a restarted sidecar —
//     a new process with a fresh in-memory flag — cannot talk over an
//     operator's rename either.
//
// Never blocks
// ------------
//
// The call runs on a goroutine tracked by goNotify, with its own timeout, so
// neither a spawn nor a turn ever waits on it. Every failure — no
// credentials, a 401, a 429, a wedged socket, a model that returns nothing —
// lands on the same path: log it, and write the deterministic fallback title
// instead. A missing title is not an error.
//
// The issue reference is INDEPENDENT of the model. It is extracted from the
// source text by regex before the call is made, and it is written whether
// the call succeeded or failed. That is the point of extracting it
// deterministically: its correctness does not depend on anything remote.

import (
	"context"
	"time"

	"github.com/prismatic-koi/prism/internal/titlegen"
)

// TitleGenerator produces a short title describing a session's work.
//
// The interface exists so the sidecar depends on the CAPABILITY, not on
// internal/titlegen's HTTP client. Config.TitleGenerator is nil in every
// test unless a test sets it, so the test suite never makes a network call
// — and a test that wants to exercise the failure path injects a generator
// that returns an error.
type TitleGenerator interface {
	// GenerateTitle returns a title for sourceText. The returned title is
	// expected to be sanitised by the implementation; the caller sanitises
	// again anyway, because this value reaches a column that is rendered
	// verbatim.
	GenerateTitle(ctx context.Context, sourceText string) (string, error)
}

// titleGenTimeout bounds the whole attempt, independent of the generator's
// own client timeout. It is a backstop for an implementation that was
// configured without one.
const titleGenTimeout = 20 * time.Second

// maybeGenerateTitle starts one title-generation attempt for this session,
// if one is warranted. It returns immediately; the work happens on a
// goroutine.
//
// Must be called with s.mu held — it reads and writes titleGenAttempted.
//
// It does nothing at all when:
//   - a title has already been attempted in this process;
//   - the source text is blank (nothing to summarise, nothing to scrape);
//   - the session's role is not eligible. Review agents are the reason this
//     check exists: there are thousands of them, a name like
//     `...~review-1-review-qa` already says what the session is, and titling
//     them would be pure cost. The check is on root_agent_name via an
//     allowlist, so a role added later is excluded until someone opts it in.
func (s *Sidecar) maybeGenerateTitle(sourceText string) {
	if s.titleGenAttempted {
		return
	}
	if titlegen.Sanitise(sourceText) == "" {
		// No usable text yet. Do NOT latch the flag: a later frame in this
		// same session may carry the real prompt, and burning the single
		// attempt on an empty string would leave the session untitled for
		// good.
		return
	}
	role := s.titleRole()
	if !titlegen.Eligible(role) {
		// Latch anyway. Eligibility cannot change within a session, so
		// re-deciding it on all 200 turns is wasted work.
		s.titleGenAttempted = true
		s.logger().Printf("sidecar: title generation skipped (role=%q not eligible)", role)
		return
	}

	// Latch BEFORE launching, so the next turn_start — which can arrive
	// while the request is still in flight — cannot start a second call.
	s.titleGenAttempted = true

	gen := s.cfg.TitleGenerator
	sessionName := s.cfg.SessionName
	dbConn := s.cfg.DB
	source := sourceText

	s.goNotify(func() {
		// Extracted first and independently of the model: the reference is a
		// pure function of the source text, so it is just as correct when
		// the call below fails as when it succeeds.
		var issueRef *string
		if ref := titlegen.ExtractIssueRef(source); ref != "" {
			issueRef = &ref
		}

		// The deterministic title is computed up front and used unless the
		// model beats it. Same derivation as the spawn-time fallback.
		title := titlegen.Sanitise(source)
		titleIsGenerated := false

		if gen != nil {
			ctx, cancel := context.WithTimeout(context.Background(), titleGenTimeout)
			defer cancel()
			generated, err := gen.GenerateTitle(ctx, source)
			switch {
			case err != nil:
				// Never fatal, and never retried. The fallback title is
				// already in hand, and this runs on the first turn of every
				// eligible session — a retry storm here would be paid on
				// every spawn.
				s.logger().Printf("sidecar: title generation failed (falling back to the derived title): %v", err)
			case titlegen.Sanitise(generated) == "":
				s.logger().Printf("sidecar: title generation returned an empty title (falling back to the derived title)")
			default:
				title = titlegen.Sanitise(generated)
				titleIsGenerated = true
			}
		}

		if title == "" {
			// Sanitise already returned "" for the source above, so this is
			// unreachable via maybeGenerateTitle's guard. Kept because
			// writing an empty-string title would break the NULL-vs-''
			// distinction the title column documents.
			return
		}

		written, err := dbConn.SetGeneratedTitle(sessionName, title, issueRef)
		if err != nil {
			s.logger().Printf("sidecar: SetGeneratedTitle failed: %v", err)
			return
		}
		if !written {
			s.logger().Printf("sidecar: title not written (the session has a human-set title, or no status row exists)")
			return
		}
		s.logger().Printf("sidecar: session title set (generated=%t, issue_ref=%t)", titleIsGenerated, issueRef != nil)
	})
}

// titleRole resolves the role used for the eligibility decision.
//
// cfg.AgentRole is authoritative when set — it is passed at spawn and is the
// same value written to root_agent_name. s.rootAgent is the host-mode
// fallback, inferred from the first user message, and matches what
// upsertState would write to root_agent_name for such a session. Using the
// same precedence as upsertState keeps the eligibility decision and the DB
// column in agreement.
func (s *Sidecar) titleRole() string {
	if s.cfg.AgentRole != "" {
		return s.cfg.AgentRole
	}
	return s.rootAgent
}
