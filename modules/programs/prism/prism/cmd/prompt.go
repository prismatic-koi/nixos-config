package cmd

// prism prompt — send a follow-up prompt to a running or finished agent session.
//
// Usage:
//
//	prism prompt <session> --prompt <text>
//	prism prompt <session> --prompt - < /tmp/prompt.txt
//	prism prompt <session> --prompt-file /tmp/prompt.txt

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
)

var promptCmd = &cobra.Command{
	Use:   "prompt <session>",
	Short: "Send a follow-up prompt to a running agent session",
	Long: `Send a follow-up message to the agent running in the named tmux
session. The session must already exist and have an agent window.

prism delivers the prompt at once through the host-API socket of the
session. It also writes a record to bus_messages (with delivered_at set)
for audit. If the delivery fails, the command returns an error.

The --deliver-as flag controls the delivery mode for socket-pipe (PI) sessions:

  steer     — inject mid-turn so the agent sees the correction immediately
              (default — use for coordinator mid-flight redirections)
  followUp  — queue as the next user turn after the current turn completes
  nextTurn  — alias for followUp; the sidecar's own default when no mode is set`,
	Args: cobra.ExactArgs(1),
	RunE: runPrompt,
}

func init() {
	addPromptFlags(promptCmd)
	promptCmd.Flags().String("deliver-as", "steer", `Delivery mode for socket-pipe sessions: steer (mid-turn), followUp, or nextTurn.`)
	promptCmd.Flags().Bool("json", false, `Emit a single JSON object to stdout instead of the human-readable success line. Shape: {"delivered_to":"<session>","delivery_id":"<uuid>","buffered":<bool>,"replayed":<bool>,"transport":"socket-pipe"}. When buffered=true the delivery was accepted but the PI extension was disconnected — the sidecar will replay it on the next handshake.`)
	rootCmd.AddCommand(promptCmd)
}

// validDeliverAsModes lists the accepted values for --deliver-as.
var validDeliverAsModes = []string{"steer", "followUp", "nextTurn"}

// waitingStateError returns the standard "session is waiting for user input"
// error used by every callsite that would otherwise deliver a prompt to a
// paused session. Shared by `prism prompt` and by `prism spawn --branch main`
// on the reuse-with-prompt path so the operator sees the same shape
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

	outcome, err := promptdelivery.DeliverToSessionEx(sessionName, status, promptText, "", deliverAs)
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	if err := database.WriteBusMessageDelivered(msg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write audit bus message: %v\n", err)
	}
	return emitPromptOutcome(cmd.OutOrStdout(), jsonOut, sessionName, "socket-pipe", outcome)
}

// emitPromptOutcome renders the result of a `prism prompt` invocation. In
// human mode it prints a single line that names the buffered outcome when
// the sidecar responded {"buffered": true} so the
// caller can distinguish "on the wire" from "parked awaiting reconnect".
// In --json mode it emits a single JSON object with delivered_to,
// delivery_id, buffered, replayed, and transport fields.
//
// Exit code stays 0 on buffered=true: the delivery is contractually
// promised, and the sidecar's durable buffer means it
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

// deriveSessionNameFromCWD walks up from cwd to find a .bare marker and
// derives the session name using the same logic as cmd/switch.go.
// Returns empty string if no .bare marker is found, or if the name is
// outside the session-name safe set (no session can have that name).
func deriveSessionNameFromCWD(cwd string) string {
	bareRoot := deriveBareRoot(cwd)
	if bareRoot == "" {
		return ""
	}
	name, err := session.NameFor(cwd, bareRoot)
	if err != nil {
		return ""
	}
	return name
}
