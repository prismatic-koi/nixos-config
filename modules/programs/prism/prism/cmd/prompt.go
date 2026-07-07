package cmd

// prism prompt — send a follow-up prompt to a running or finished agent session.
//
// Usage:
//
//	prism prompt <session> --prompt <text>
//	prism prompt <session> --prompt - < /tmp/prompt.txt
//	prism prompt <session> --prompt-file /tmp/prompt.txt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
)

// httpClient is the HTTP client used for prompt delivery. Package-level so
// tests can replace it with an httptest.Server-backed client.
var httpClient = &http.Client{Timeout: 10 * time.Second}

var promptCmd = &cobra.Command{
	Use:   "prompt <session>",
	Short: "Send a follow-up prompt to a running agent session",
	Long: `Send a follow-up message to the agent running in the named tmux
session. The session must already exist and have an agent window.

The prompt is delivered directly via POST /session/:id/prompt_async for
instant delivery. A record is also written to bus_messages (with delivered_at
set) for audit. If HTTP delivery fails, an error is returned.

The --deliver-as flag controls the delivery mode for socket-pipe (PI) sessions:

  steer     — inject mid-turn so the agent sees the correction immediately
              (default — use for coordinator mid-flight redirections)
  followUp  — queue as the next user turn after the current turn completes
  nextTurn  — alias for followUp; the sidecar's own default when no mode is set

For HTTP sessions the flag is accepted but has no effect — the harness
uses prompt_async and does not support delivery modes.`,
	Args: cobra.ExactArgs(1),
	RunE: runPrompt,
}

func init() {
	addPromptFlags(promptCmd)
	promptCmd.Flags().String("deliver-as", "steer", `Delivery mode for socket-pipe sessions: steer (mid-turn), followUp, or nextTurn.`)
	promptCmd.Flags().Bool("json", false, `Emit a single JSON object to stdout instead of the human-readable success line. Shape: {"delivered_to":"<session>","delivery_id":"<uuid>","buffered":<bool>,"replayed":<bool>,"transport":"socket-pipe"|"http"}. When buffered=true the delivery was accepted but the PI extension was disconnected — the sidecar will replay it on the next handshake.`)
	rootCmd.AddCommand(promptCmd)
}

// validDeliverAsModes lists the accepted values for --deliver-as.
var validDeliverAsModes = []string{"steer", "followUp", "nextTurn"}

// waitingStateError returns the standard "session is waiting for user input"
// error used by every callsite that would otherwise deliver a prompt to a
// paused session. Shared by `prism prompt` and by `prism spawn --branch main`
// on the reuse-with-prompt path (#2352) so the operator sees the same shape
// of message regardless of entry point.
func waitingStateError(sessionName string) error {
	return fmt.Errorf(
		"session %q is waiting for user input\n\n"+
			"The agent has paused and is expecting a direct response from the user.\n"+
			"Please switch to that session and respond there, or escalate to the user\n"+
			"so they can address it directly.\n\n"+
			"  prism checkin %s   — inspect the current state\n"+
			"  (C-f or C-w)       — switch to the session in tmux",
		sessionName, sessionName,
	)
}

func runPrompt(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	promptText, err := requirePromptInput(cmd)
	if err != nil {
		return err
	}

	// Validate --deliver-as client-side before any network call.
	deliverAs, _ := cmd.Flags().GetString("deliver-as")
	valid := false
	for _, m := range validDeliverAsModes {
		if deliverAs == m {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("--deliver-as must be one of: %s (got: %q)",
			strings.Join(validDeliverAsModes, ", "), deliverAs)
	}
	jsonOut, _ := cmd.Flags().GetBool("json")

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyPromptWithOutcome(apiURL, sessionName, promptText, deliverAs, jsonOut)
	}

	// Open DB.
	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	// Check target session status.
	status, err := database.CurrentStatus(sessionName)
	if err != nil {
		return fmt.Errorf("check session status: %w", err)
	}

	if status == nil {
		sessionNames, _ := activeSessionNamesForError(database, 10)
		if len(sessionNames) == 0 {
			return fmt.Errorf(
				"session %q not found — no active sessions in DB (run `prism sessions list` to verify)",
				sessionName,
			)
		}
		return fmt.Errorf(
			"session must be one of: %s (got: %q)",
			strings.Join(sessionNames, ", "), sessionName,
		)
	}

	if status.EndedAt != nil {
		return fmt.Errorf(
			"session %q has ended — escalate to user to restart if needed",
			sessionName,
		)
	}

	if status.State == "waiting" {
		return waitingStateError(sessionName)
	}

	// Derive from_session from the current process CWD using the .bare walk.
	fromSession := ""
	if cwd, err := os.Getwd(); err == nil {
		fromSession = deriveSessionNameFromCWD(cwd)
	}

	// Derive repo from the target session's agent_status, fallback to from_session.
	repo := status.Repo
	if repo == "" && fromSession != "" {
		// best-effort: extract repo component from from_session (format: "repo@branch")
		for i, c := range fromSession {
			if c == '@' {
				repo = fromSession[:i]
				break
			}
		}
	}

	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  fromSession,
		ToSession:    sessionName,
		ToInstanceID: status.InstanceID,
		Repo:         repo,
		Text:         promptText,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}

	// Dispatch based on harness transport shape:
	// socket-pipe (pi) → host-API socket; other/unknown → HTTP fallback.
	buildBody := func(text string, s *db.Status) map[string]any {
		return buildPromptBody(text, s)
	}
	outcome, err := promptdelivery.DeliverToSessionEx(sessionName, status, promptText, buildBody, "", deliverAs)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := database.WriteBusMessageDelivered(msg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write audit bus message: %v\n", err)
	}
	// Determine transport for human-readable output and JSON envelope.
	transport := "http"
	if status.Harness != nil && *status.Harness != "" {
		if shape, ok := harness.ShapeOf(*status.Harness); ok && shape == harness.TransportSocketPipe {
			transport = "socket-pipe"
		}
	}
	return emitPromptOutcome(cmd.OutOrStdout(), jsonOut, sessionName, transport, outcome)
}

// emitPromptOutcome renders the result of a `prism prompt` invocation. In
// human mode it prints a single line that names the buffered outcome when
// the sidecar responded {"buffered": true} (issue #2359 Gap B) so the
// caller can distinguish "on the wire" from "parked awaiting reconnect".
// In --json mode it emits a single JSON object with delivered_to,
// delivery_id, buffered, replayed, and transport fields.
//
// Exit code stays 0 on buffered=true: the delivery is contractually
// promised, and the sidecar's durable buffer (also #2359 Gap B) means it
// survives sidecar restart. The distinguishing signal is in the message
// content, not the exit code.
func emitPromptOutcome(stdout io.Writer, jsonOut bool, sessionName, transport string, outcome promptdelivery.DeliveryOutcome) error {
	if jsonOut {
		obj := map[string]any{
			"delivered_to": sessionName,
			"delivery_id":  outcome.DeliveryID,
			"buffered":     outcome.Buffered,
			"replayed":     outcome.Replayed,
			"transport":    transport,
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal prompt outcome: %w", err)
		}
		fmt.Fprintln(stdout, string(b))
		return nil
	}
	switch {
	case outcome.Buffered:
		fmt.Fprintf(stdout,
			"prompt accepted for %s but PI extension is disconnected — buffered for replay on next handshake (delivery_id=%s)\n",
			sessionName, outcome.DeliveryID)
	case outcome.Replayed:
		fmt.Fprintf(stdout,
			"prompt to %s dropped as replay of a previously-seen delivery_id=%s (dedup at sidecar)\n",
			sessionName, outcome.DeliveryID)
	default:
		fmt.Fprintf(stdout, "prompt delivered to %s via %s\n", sessionName, transport)
	}
	return nil
}

// deliverViaSidecarHostAPI dials the per-session host-API Unix socket and
// POSTs /prompt to deliver a prompt to a socket-pipe (e.g. PI) session. The
// socket-pipe path bypasses the HTTP harness API (used for HTTP harnesses) and
// instead routes through the sidecar's pipe-frame queue.
//
// deliverAs controls the delivery mode ("steer", "followUp", or "nextTurn").
// It is forwarded as the "deliver_as" JSON field so the sidecar knows how to
// inject the prompt. The sidecar validates unknown values and returns HTTP 400.
//
// The function dials the socket directly rather than going through the
// PRISM_HOST_API proxy mechanism: PRISM_HOST_API is only set inside
// containerised agent processes; the host-side prism CLI must look up the
// per-session socket itself via session.SidecarHostAPIPath.
func deliverViaSidecarHostAPI(sessionName, promptText, deliverAs string) error {
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		return fmt.Errorf("resolve sidecar host-api socket: %w", err)
	}
	if _, statErr := os.Stat(sockPath); statErr != nil {
		return fmt.Errorf("sidecar host-api socket missing at %s: %w", sockPath, statErr)
	}
	client := newHostAPIClient(sockPath)

	body, err := json.Marshal(map[string]string{
		"session":    sessionName,
		"prompt":     promptText,
		"deliver_as": deliverAs,
	})
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://prism-hostapi/prompt", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dial sidecar host-api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sidecar host-api returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// buildPromptBody constructs the request body for prompt_async.
// When root_agent_name and root_model_id are known, they are included
// so the session continues using its root agent/model. Falls back to
// agent_name/model_id for sessions created before the root fields migration.
func buildPromptBody(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
	}

	agentName := status.RootAgentName
	if agentName == nil {
		agentName = status.AgentName
	}
	modelID := status.RootModelID
	if modelID == nil {
		modelID = status.ModelID
	}

	if agentName != nil && modelID != nil {
		body["agent"] = *agentName

		// Split model_id on the first "/" to get providerID and modelID.
		slashIdx := strings.Index(*modelID, "/")
		providerID := *modelID
		modelIDStr := ""
		if slashIdx >= 0 {
			providerID = (*modelID)[:slashIdx]
			modelIDStr = (*modelID)[slashIdx+1:]
		}
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelIDStr,
		}
	}

	return body
}

// deriveSessionNameFromCWD walks up from cwd to find a .bare marker and
// derives the session name using the same logic as cmd/switch.go.
// Returns empty string if no .bare marker is found.
func deriveSessionNameFromCWD(cwd string) string {
	bareRoot := deriveBareRoot(cwd)
	if bareRoot == "" {
		return ""
	}
	return session.NameFor(cwd, bareRoot)
}
