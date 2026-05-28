package cmd

// prism escalate — hand a question to the coordinator in one step and pause
// the calling worker session in a new "escalated" state.
//
// Surface:
//
//	prism escalate [--to <session>] --prompt "<message>"
//	prism escalate [--to <session>] --prompt -          # read from stdin
//
// Without --to, the command auto-discovers the same-repo coordinator (the
// session whose root_agent_name = "coordinator", or the legacy <repo>@main
// row when the field is NULL). With multiple candidates, --to is required;
// with zero candidates, the worker still transitions to escalated and writes
// a self-marker but no prompt is delivered (a human is expected to pick up
// the worker via tmux).
//
// State machine:
//
//	active ──prism escalate──▶ escalated
//	escalated ──any turn_start──▶ active
//
// While the calling session is in `escalated`, the sidecar suppresses the
// `has finished` notification — the `session.escalated` bus event is the
// notification.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
)

var escalateCmd = &cobra.Command{
	Use:   "escalate",
	Short: "Hand a question to the coordinator and enter the 'escalated' state",
	Long: `Send a prompt to the same-repo coordinator (auto-discovered or specified
via --to) and transition the calling session to the "escalated" state.

While escalated, the sidecar suppresses the "has finished" notification — the
session.escalated bus event is the notification, and the coordinator gets the
escalation prompt as a single targeted signal rather than two redundant ones.

Discovery rules:

  - exactly one same-repo coordinator candidate → auto-discover, send to it.
  - multiple same-repo coordinator candidates   → refuse without --to and
    print the candidate list.
  - no coordinator candidate found              → still transition to
    escalated; record a "no coordinator found, please wait for a human"
    marker in the calling session's own log. The worker stays paused.

Any incoming turn_start (from prism prompt, a human typing into tmux, or any
other source) clears the escalated state and resumes the worker normally.`,
	Args: cobra.NoArgs,
	RunE: runEscalate,
}

func init() {
	addPromptFlags(escalateCmd)
	escalateCmd.Flags().String("to", "", `Explicit coordinator session to receive the escalation. Required when auto-discovery finds multiple coordinator candidates in the same repo. Optional otherwise.`)
	rootCmd.AddCommand(escalateCmd)
}

func runEscalate(cmd *cobra.Command, args []string) error {
	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	explicitTo, _ := cmd.Flags().GetString("to")

	// Container path: when running inside an isolated worker container, the
	// host-side prism CLI and DB are not directly accessible. Proxy to the
	// host sidecar's /escalate endpoint, which shells back to `prism escalate`
	// on the host with PRISM_HOST_API unset.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyEscalate(apiURL, explicitTo, promptText)
	}

	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	// Derive the calling (source) session from CWD.
	cwd, _ := os.Getwd()
	if envSession := os.Getenv("PRISM_SESSION_NAME"); envSession != "" {
		// Tests and the host-API proxy may set PRISM_SESSION_NAME directly.
		return runEscalateForSession(database, envSession, explicitTo, promptText)
	}
	fromSession := deriveSessionNameFromCWD(cwd)
	if fromSession == "" {
		return fmt.Errorf(
			"prism escalate: could not derive calling session from CWD %q\n" +
				"hint: run from inside a prism worktree, or set PRISM_SESSION_NAME",
			cwd,
		)
	}
	return runEscalateForSession(database, fromSession, explicitTo, promptText)
}

// runEscalateForSession is the testable core of runEscalate, parameterised on
// the calling session name so unit tests can drive it without the CWD walk.
func runEscalateForSession(database *db.DB, fromSession, explicitTo, promptText string) error {
	selfStatus, err := database.CurrentStatus(fromSession)
	if err != nil {
		return fmt.Errorf("prism escalate: read self status: %w", err)
	}
	if selfStatus == nil {
		return fmt.Errorf("prism escalate: calling session %q has no agent_status row", fromSession)
	}
	repo := selfStatus.Repo
	if repo == "" {
		return fmt.Errorf("prism escalate: calling session %q has no repo recorded — escalate is only available from worktree-backed sessions", fromSession)
	}

	// Resolve the target.
	target, err := resolveEscalationTarget(database, repo, explicitTo)
	if err != nil {
		// Discovery error: do NOT transition state. Print and exit non-zero.
		return err
	}

	// Build payload metadata.
	branch := ""
	headSHA := ""
	if selfStatus.Worktree != "" {
		if ref, refErr := git.SymbolicRef(selfStatus.Worktree); refErr == nil {
			branch = strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
		}
		if sha, shaErr := git.ShortHash(selfStatus.Worktree); shaErr == nil {
			headSHA = strings.TrimSpace(sha)
		}
	}
	prNumbers := discoverPRNumbers(selfStatus.Worktree)
	verdicts := discoverLastReviewVerdicts(database, fromSession)

	targetName := ""
	var targetInstanceID *string
	if target != nil {
		targetName = target.SessionName
		targetInstanceID = target.InstanceID
	}

	payload := EscalationPayload{
		Source:     fromSession,
		Target:     targetName,
		Prompt:     promptText,
		PRNumbers:  prNumbers,
		Branch:     branch,
		HeadSHA:    headSHA,
		Verdicts:   verdicts,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Transition the calling session to escalated. This must happen even when
	// no coordinator was found (per the discovery rules), so do it before any
	// delivery attempt that might fail.
	if err := database.UpsertStatus(fromSession, selfStatus.Repo, selfStatus.Worktree, string(agent.StateEscalated), nil, nil); err != nil {
		return fmt.Errorf("prism escalate: transition to escalated: %w", err)
	}

	// Echo the escalation context into the calling session's own log so
	// reviewers and humans see it inline via `prism checkin <self>`.
	echoEscalationToSelf(database, fromSession, selfStatus, payload)

	// Write the session.escalated bus event for dashboard / downstream
	// consumers. This row sits alongside any later `session.finished` row,
	// distinct in name and payload.
	writeSessionEscalatedEvent(database, fromSession, selfStatus, payload)

	// Deliver to the coordinator. With zero candidates, target is nil and
	// we skip delivery entirely — the calling session is still escalated
	// and the bus event still fired (with empty target).
	if target == nil {
		fmt.Fprintf(os.Stderr,
			"prism escalate: no coordinator found in repo %q; session %q transitioned to escalated.\n"+
				"please wait for a human to come check on you.\n",
			repo, fromSession,
		)
		return nil
	}

	// Muted guard: when the calling session is muted, suppress the outbound
	// coordinator escalation notification. The state transition and the
	// session.escalated agent_events row are still written above so the
	// dashboard reflects reality; only the bus delivery is skipped. This
	// mirrors the notifyCoordinator suppression in internal/sidecar/notify.go
	// for the finish-notification path — outbound to the coordinator is the
	// suppression boundary, DB writes are unaffected. (#2013)
	if selfStatus.Muted {
		fmt.Fprintf(os.Stderr,
			"prism escalate: session %q is muted; coordinator notification suppressed (state still transitioned to escalated, bus event still written).\n",
			fromSession,
		)
		return nil
	}

	// Deliver via prism prompt machinery. We write the bus_messages row as
	// "delivered" only on success; the sidecar's prompt path already does this
	// for the audit trail, so we delegate by invoking the same code path that
	// `prism prompt` uses.
	if err := deliverEscalationPrompt(database, fromSession, target, promptText); err != nil {
		// Delivery failed but state is already escalated. Surface the error
		// so the worker knows; the bus event was already written so the
		// coordinator may still see the escalation through the bus.
		return fmt.Errorf("prism escalate: deliver prompt to %s: %w", target.SessionName, err)
	}

	fmt.Printf("prism escalate: delivered to %s; session %s now in 'escalated' state\n", target.SessionName, fromSession)
	_ = targetInstanceID // surfaced via bus payload; not used directly here
	return nil
}

// resolveEscalationTarget applies the discovery rules from the issue:
//
//   - explicitTo set: look up that session by name; error if missing.
//   - explicitTo unset and len(candidates) == 1: return that candidate.
//   - explicitTo unset and len(candidates) > 1: error listing candidates.
//   - explicitTo unset and len(candidates) == 0: return (nil, nil) so the
//     caller can transition state without delivery.
func resolveEscalationTarget(database *db.DB, repo, explicitTo string) (*db.Status, error) {
	if explicitTo != "" {
		st, err := database.CurrentStatus(explicitTo)
		if err != nil {
			return nil, fmt.Errorf("prism escalate: look up --to %q: %w", explicitTo, err)
		}
		if st == nil {
			return nil, fmt.Errorf("prism escalate: --to session %q not found in agent_status\nrun `prism sessions list` to see available sessions", explicitTo)
		}
		if st.EndedAt != nil {
			return nil, fmt.Errorf("prism escalate: --to session %q has ended", explicitTo)
		}
		return st, nil
	}

	candidates, err := database.CoordinatorCandidatesForRepo(repo)
	if err != nil {
		return nil, fmt.Errorf("prism escalate: discover coordinator candidates for repo %q: %w", repo, err)
	}
	switch len(candidates) {
	case 0:
		return nil, nil
	case 1:
		c := candidates[0]
		return &c, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "prism escalate: multiple coordinator candidates in repo %q; pass --to to choose one:\n", repo)
		for _, c := range candidates {
			fmt.Fprintf(&b, "  - %s\n", c.SessionName)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
}

// EscalationPayload is the JSON payload carried by both the local-log
// `escalation` event and the bus `session.escalated` event.
type EscalationPayload struct {
	Source     string   `json:"source"`              // calling worker session
	Target     string   `json:"target,omitempty"`    // coordinator session (empty when none)
	Prompt     string   `json:"prompt"`              // user-supplied message body
	PRNumbers  []string `json:"pr_numbers,omitempty"` // discoverable open PRs
	Branch     string   `json:"branch,omitempty"`    // worker branch
	HeadSHA    string   `json:"head_sha,omitempty"`  // short HEAD SHA
	Verdicts   []string `json:"verdicts,omitempty"`  // last review-cycle verdicts
	OccurredAt string   `json:"occurred_at"`         // RFC3339
}

// echoEscalationToSelf writes a self-marker event of type "escalation" into
// the calling session's own event log so `prism checkin <self>` shows the
// escalation context inline. This is in addition to the bus event and is
// distinct from delivery to the coordinator.
func echoEscalationToSelf(database *db.DB, fromSession string, selfStatus *db.Status, payload EscalationPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: marshal self echo payload: %v\n", err)
		return
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: fromSession,
		Repo:        selfStatus.Repo,
		Worktree:    selfStatus.Worktree,
		InstanceID:  selfStatus.InstanceID,
		Type:        "escalation",
		Payload:     string(body),
		CreatedAt:   time.Now(),
	}
	if err := database.WriteEvent(ev); err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: write self echo event: %v\n", err)
	}
}

// writeSessionEscalatedEvent writes a bus-shaped event of type
// "session.escalated" into agent_events for the calling session. This is the
// new event type distinct from "session.finished": handlers that subscribe
// only to session.finished receive nothing for an escalation.
func writeSessionEscalatedEvent(database *db.DB, fromSession string, selfStatus *db.Status, payload EscalationPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: marshal bus event payload: %v\n", err)
		return
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: fromSession,
		Repo:        selfStatus.Repo,
		Worktree:    selfStatus.Worktree,
		InstanceID:  selfStatus.InstanceID,
		Type:        "session.escalated",
		Payload:     string(body),
		CreatedAt:   time.Now(),
	}
	if err := database.WriteEvent(ev); err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: write session.escalated event: %v\n", err)
	}
}

// deliverEscalationPrompt routes the prompt to the target coordinator. It
// reuses the same delivery machinery as `prism prompt`: HTTP for HTTP harnesses
// sessions, host-API socket for socket-pipe (PI) sessions.
//
// The audit row in bus_messages is written with delivered_at set on success.
// from_session is the calling worker session.
func deliverEscalationPrompt(database *db.DB, fromSession string, target *db.Status, promptText string) error {
	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  fromSession,
		ToSession:    target.SessionName,
		ToInstanceID: target.InstanceID,
		Repo:         target.Repo,
		Text:         promptText,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}

	// Reuse the existing delivery primitives. The "deliver_as=followUp" mode
	// queues the message until the coordinator's current turn ends, matching
	// the semantics of finish notifications today (the coordinator should not
	// be steered mid-stream by a worker escalation).
	if target.Harness != nil && *target.Harness != "" {
		// Try the harness-aware path. We mirror cmd/prompt.go's runPrompt
		// inline rather than calling it because runPrompt resolves the
		// from_session via CWD walk, which is brittle here (we already know
		// it). The actual transport selection is what we want to share.
		if err := deliverEscalationToTarget(target, promptText); err != nil {
			if writeErr := database.WriteBusMessageFailed(msg); writeErr != nil {
				fmt.Fprintf(os.Stderr, "prism escalate: write failed audit: %v\n", writeErr)
			}
			return err
		}
	} else {
		// Pre-migration row with no harness column. Fall back to HTTP delivery.
		if err := deliverEscalationToTarget(target, promptText); err != nil {
			if writeErr := database.WriteBusMessageFailed(msg); writeErr != nil {
				fmt.Fprintf(os.Stderr, "prism escalate: write failed audit: %v\n", writeErr)
			}
			return err
		}
	}

	if err := database.WriteBusMessageDelivered(msg); err != nil {
		fmt.Fprintf(os.Stderr, "prism escalate: write delivered audit: %v\n", err)
	}
	return nil
}

// deliverEscalationToTarget delivers the escalation to the target session.
// Uses promptdelivery which dispatches based on harness transport shape.
func deliverEscalationToTarget(target *db.Status, promptText string) error {
	return promptdelivery.DeliverToSession(target.SessionName, target, promptText, nil, "", "followUp")
}



// discoverPRNumbers best-effort returns open PRs whose head matches the
// worker's branch via `gh pr list`. Returns nil silently on any error so the
// escalation never fails on metadata gathering.
func discoverPRNumbers(worktree string) []string {
	if worktree == "" {
		return nil
	}
	branch, err := git.SymbolicRef(worktree)
	if err != nil {
		return nil
	}
	branch = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"))
	if branch == "" {
		return nil
	}
	// Use gh pr list scoped to this branch. Best-effort; silent on any error.
	out, err := runCmdInDir(worktree, "gh", "pr", "list", "--head", branch, "--state", "open", "--json", "number", "--jq", ".[].number")
	if err != nil {
		return nil
	}
	var prs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			prs = append(prs, line)
		}
	}
	return prs
}

// discoverLastReviewVerdicts scans the calling session's recent
// `review_complete` audit events (best-effort) for verdict strings. Returns
// nil when no verdicts are recorded, which is the common case for a worker
// that has not run `prism review`.
func discoverLastReviewVerdicts(database *db.DB, fromSession string) []string {
	// The dashboard / monitor records review verdicts on the parent worker via
	// the audit-event path. We scan the most recent ~20 audit events for any
	// verdict-bearing rows. This is intentionally tolerant: a missing or
	// malformed payload is silently dropped.
	events, err := database.QueryAuditEvents(fromSession, 0, "", 20)
	if err != nil {
		return nil
	}
	var out []string
	for _, ev := range events {
		// Look for verdict markers in payloads. We check both "verdict" and
		// "result" fields to tolerate shape evolution.
		var p map[string]any
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			continue
		}
		if v, ok := p["verdict"].(string); ok && v != "" {
			out = append(out, v)
			continue
		}
		if v, ok := p["result"].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// proxyEscalate forwards the escalate request to the host sidecar. The
// sidecar handler shells out to `prism escalate` on the host, where the
// direct DB path runs.
func proxyEscalate(apiURL, explicitTo, promptText string) error {
	body := map[string]any{
		"prompt": promptText,
	}
	if explicitTo != "" {
		body["to"] = explicitTo
	}
	// Pass our session name so the host can identify the calling session
	// without the CWD walk (the host-side process spawned by the sidecar
	// inherits the host CWD, not the container's worker CWD).
	if envSession := os.Getenv("PRISM_SESSION_NAME"); envSession != "" {
		body["from"] = envSession
	}
	return proxyToHostAPI(apiURL, "/escalate", body, nil)
}

// runCmdInDir runs name with args in dir and returns stdout. A non-zero exit
// returns an error that includes the stderr output.
func runCmdInDir(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// session.NameFor lookup helper passthrough — keeps the import live for
// the test path (testers stub PRISM_SESSION_NAME).
var _ = session.NameFor
