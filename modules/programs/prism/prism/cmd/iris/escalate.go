package main

// escalate.go — `iris escalate` subcommand.
//
// `iris escalate` is the worker-side analogue of prism escalate: it sends a
// targeted prompt to the same-host coordinator and transitions the calling
// iris session into the `escalated` state. The daemon performs auto-discovery
// (Role == "coordinator") when --to is not given, mirroring prism's
// auto-discovery shape (issue #1693).
//
// Surface:
//
//	iris escalate [--to <session>] --prompt "<message>"
//	iris escalate [--to <session>] --prompt -            # read from stdin
//	iris escalate [--to <session>] --prompt-file <path>
//
// The CLI resolves the calling session from $IRIS_SESSION_NAME, dials the
// iris daemon's client IPC socket, and sends a single `escalation_deliver`
// frame. The daemon:
//
//  1. Validates that the calling session is a registered iris session.
//  2. Resolves the coordinator target.
//  3. Transitions the calling session to the `escalated` state. While the
//     session is in this state, any subsequent prompt_deliver (from any
//     source — the coordinator's reply, a human via `iris prompt`, the
//     TUI) transitions the worker back to `active`.
//  4. Delivers the prompt body to the coordinator via the same path as
//     `iris prompt`. A delivery_id is minted per call so the underlying
//     harness can dedup retries (issue #1695 contract).
//
// Discovery:
//
//   - exactly one Role==coordinator session in the daemon  → auto-discover.
//   - multiple                                              → require --to;
//     the daemon returns an error listing the candidates and the worker
//     stays in `active` state.
//   - zero                                                  → the worker
//     still transitions to `escalated` and a self-marker event is written;
//     no prompt is delivered. A human is expected to attend the session
//     manually via `iris prompt`.
//
// State machine summary:
//
//	active ──escalation_deliver──▶ escalated
//	escalated ──any prompt_deliver──▶ active
//
// Errors:
//
//   - daemon down            → canonical "systemctl --user start iris" hint.
//   - --prompt and stdin both empty → "a prompt is required" multi-line message.
//   - calling session not in $IRIS_SESSION_NAME and not on the daemon →
//     "not a registered iris session".
//   - --to names a non-coordinator → daemon error surfaced verbatim.
//   - mid-send connection drop → "lost connection to daemon", no hang.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// escalateDialTimeout bounds how long `iris escalate` will wait when dialling
// the daemon socket. Same budget as `iris prompt`: the daemon is local, a
// healthy dial completes in microseconds, and a multi-second hang almost
// certainly means the daemon is not running.
const escalateDialTimeout = 2 * time.Second

// escalateWriteTimeout bounds the write of the escalation_deliver frame.
const escalateWriteTimeout = 5 * time.Second

// escalateAckWindow is how long we wait after sending escalation_deliver to
// see the daemon's ack frame. Unlike prompt_deliver (which has no success
// ack), escalation_deliver DOES emit an explicit
// DaemonEscalationDeliveredFrame so we know on the wire when the daemon has
// finished both the state transition AND the coordinator-side delivery.
const escalateAckWindow = 10 * time.Second

// escalateCmd is the `iris escalate` subcommand.
var escalateCmd = &cobra.Command{
	Use:   "escalate",
	Short: "Hand a question to the coordinator and enter the 'escalated' state",
	Long: `Send a prompt to the same-host coordinator (auto-discovered or specified
via --to) and transition the calling iris session to the "escalated" state.

While escalated, the iris daemon emits a session.escalated event on its
fan-out (the bus equivalent for iris) and suppresses the regular session
finish notification for that session until it returns to active. Any
subsequent iris prompt to the worker — from the coordinator's reply, a
human via 'iris prompt', or the TUI — clears the escalated state and
resumes the worker normally.

Auto-discovery rules:

  - exactly one same-host coordinator session  → auto-discover, send to it.
  - multiple same-host coordinator sessions    → refuse without --to and
    print the candidate list. The worker stays in active state.
  - zero coordinator sessions                  → still transition to
    escalated and write a self-marker event; no prompt is delivered.
    A human is expected to attend the worker via 'iris prompt'.

Three input variants are accepted (mirrors 'iris prompt'):

  --prompt <text>          inline prompt text
  --prompt-file <path>     read the prompt body from a file
  --prompt -               read the prompt body from stdin

--prompt and --prompt-file are mutually exclusive. For file/stdin input a
single trailing newline is stripped.

State machine:

  active     ──iris escalate──▶ escalated
  escalated  ──any iris prompt──▶ active

The calling session is identified via $IRIS_SESSION_NAME, set by the iris
supervisor for every pi child. Outside an iris-managed session, set the
env var manually or pass --from explicitly to the daemon (the daemon
rejects unregistered sessions with a clear error).`,
	Args:          cobra.NoArgs,
	RunE:          runEscalateCmd,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	escalateCmd.Flags().String(
		"prompt", "",
		"Text to send to the coordinator. Use --prompt-file for complex strings or to avoid shell-escaping issues. Use '-' to read from stdin.",
	)
	escalateCmd.Flags().String(
		"prompt-file", "",
		"Path to a file containing the prompt text. Mutually exclusive with --prompt.",
	)
	escalateCmd.Flags().String(
		"to", "",
		"Explicit coordinator session to receive the escalation. Required when auto-discovery finds multiple coordinator candidates.",
	)
	escalateCmd.Flags().String(
		"from", "",
		"Calling session name (defaults to $IRIS_SESSION_NAME). Useful for scripts that escalate on behalf of a known session.",
	)
	escalateCmd.Flags().String(
		"socket", "",
		"Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)",
	)
	escalateCmd.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
	rootCmd.AddCommand(escalateCmd)
}

// runEscalateCmd is the cobra entry point. It resolves the input and the
// calling session name, then defers to runEscalateAt for the wire path so
// that integration tests can drive the flow against an in-process
// ClientSocket.
func runEscalateCmd(cmd *cobra.Command, args []string) error {
	promptText, err := resolveIrisEscalatePromptInput(cmd)
	if err != nil {
		return err
	}

	to, _ := cmd.Flags().GetString("to")
	fromFlag, _ := cmd.Flags().GetString("from")
	from := fromFlag
	if from == "" {
		from = os.Getenv("IRIS_SESSION_NAME")
	}
	if from == "" {
		return errors.New(
			"iris escalate: could not determine calling session\n" +
				"hint: run from inside an iris-managed pi session (where $IRIS_SESSION_NAME is set),\n" +
				"or pass --from <session-name> explicitly",
		)
	}

	sockPath := resolveSocketPath(cmd)
	return runEscalateAt(cmd.Context(), sockPath, from, to, promptText, os.Stdout)
}

// runEscalateAt is the testable core of `iris escalate`. sockPath is passed
// explicitly so integration tests can point it at a tempdir socket. Returns
// nil on success (including the zero-coordinator branch — that path still
// exits 0 and prints a clear message).
func runEscalateAt(ctx context.Context, sockPath, from, to, promptText string, out io.Writer) error {
	if from == "" {
		return errors.New("iris escalate: from is required")
	}
	if promptText == "" {
		return errors.New(
			"a prompt is required — supply one of:\n" +
				"  --prompt <text>\n" +
				"  --prompt - (read from stdin)\n" +
				"  --prompt-file <path>",
		)
	}

	conn, err := dialDaemonForEscalate(ctx, sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := sendEscalationDeliverFrame(conn, from, to, promptText); err != nil {
		return err
	}

	ack, err := readEscalationAck(ctx, conn)
	if err != nil {
		return err
	}

	if !ack.Delivered {
		fmt.Fprintf(out,
			"iris escalate: no coordinator found; session %s now in 'escalated' state.\n"+
				"please wait for a human to come check on you.\n",
			from,
		)
		return nil
	}
	fmt.Fprintf(out,
		"iris escalate: delivered to %s (delivery_id=%s); session %s now in 'escalated' state\n",
		ack.To, ack.DeliveryID, from,
	)
	return nil
}

// resolveIrisEscalatePromptInput reads the prompt body from --prompt,
// --prompt-file, or stdin (when --prompt is "-"). Mirrors prompt.go's
// resolveIrisPromptInput exactly so the two CLIs share the same conventions.
func resolveIrisEscalatePromptInput(cmd *cobra.Command) (string, error) {
	promptFile, _ := cmd.Flags().GetString("prompt-file")
	promptText, _ := cmd.Flags().GetString("prompt")

	if promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return "", fmt.Errorf("read prompt file %q: %w", promptFile, err)
		}
		return strings.TrimSuffix(string(data), "\n"), nil
	}

	if promptText == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read prompt from stdin: %w", err)
		}
		return strings.TrimSuffix(string(data), "\n"), nil
	}

	return promptText, nil
}

// dialDaemonForEscalate dials the daemon client socket. Same canonical
// "daemon not running" wording as `iris spawn` / `iris prompt`.
func dialDaemonForEscalate(ctx context.Context, sockPath string) (net.Conn, error) {
	if _, err := os.Stat(sockPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"iris daemon not running: socket %s does not exist; start it with `systemctl --user start iris` (or `launchctl kickstart -k gui/$UID/iris` on Darwin)",
				sockPath,
			)
		}
		return nil, fmt.Errorf("iris escalate: stat socket %s: %w", sockPath, err)
	}
	d := net.Dialer{Timeout: escalateDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf(
			"iris daemon not running (could not dial %s: %v); start it with `systemctl --user start iris`",
			sockPath, err,
		)
	}
	return conn, nil
}

// sendEscalationDeliverFrame marshals and writes a ClientEscalationDeliverFrame
// to the daemon connection.
func sendEscalationDeliverFrame(conn net.Conn, from, to, promptText string) error {
	frame := iris.ClientEscalationDeliverFrame{
		Type:   iris.ClientFrameEscalationDeliver,
		From:   from,
		To:     to,
		Prompt: promptText,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("iris escalate: marshal escalation_deliver: %w", err)
	}
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(escalateWriteTimeout))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("iris escalate: lost connection to daemon during send: %w", err)
	}
	return nil
}

// readEscalationAck reads frames from the daemon until it sees either an
// escalation_delivered or an error frame. The daemon emits exactly one of
// these in response to an escalation_deliver frame.
func readEscalationAck(ctx context.Context, conn net.Conn) (*iris.DaemonEscalationDeliveredFrame, error) {
	// Honour context cancellation by tripping the read deadline.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(escalateAckWindow))
	r := bufio.NewReaderSize(conn, 1<<20)

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return nil, errors.New("iris escalate: timed out waiting for daemon ack")
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("iris escalate: lost connection to daemon before ack")
			}
			return nil, fmt.Errorf("iris escalate: read ack: %w", err)
		}

		var head struct {
			Type        string `json:"type"`
			RequestType string `json:"request_type"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			fmt.Fprintf(os.Stderr, "[iris] warning: ignoring malformed frame from daemon: %v\n", err)
			continue
		}
		switch head.Type {
		case iris.DaemonFrameEscalationDelivered:
			var frame iris.DaemonEscalationDeliveredFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				return nil, fmt.Errorf("iris escalate: parse escalation_delivered: %w", err)
			}
			return &frame, nil
		case iris.DaemonFrameError:
			if head.RequestType == "" || head.RequestType == iris.ClientFrameEscalationDeliver {
				return nil, fmt.Errorf("iris escalate: %s", head.Message)
			}
			fmt.Fprintf(os.Stderr, "[iris] note: unrelated error frame (request_type=%q): %s\n", head.RequestType, head.Message)
		default:
			// Unknown frame on this connection — ignore (we did not
			// subscribe). Keep reading until ack or timeout.
		}
	}
}
