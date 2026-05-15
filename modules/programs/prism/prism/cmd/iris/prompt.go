package main

// prompt.go — `iris prompt` subcommand.
//
// `iris prompt <session>` dials the iris daemon's client IPC socket and asks
// it to forward a prompt to the named running session. It is the analogue of
// `prism prompt <session>` for the daemon-mode world (issue #1677).
//
// The wire frame `prompt_deliver` is defined in
// internal/iris/client_protocol.go and handled by the daemon's ClientSocket
// (see internal/iris/client_socket.go::handlePromptDeliver). This file is
// pure CLI plumbing on top of that frame:
//
//	1. Resolve the prompt body from --prompt / --prompt-file / --prompt -
//	   (stdin), mirroring prism's conventions in cmd/prompt_input.go.
//	2. Dial the daemon client socket. If unreachable, emit the canonical
//	   "daemon not running" error (same wording used by `iris spawn`).
//	3. Fetch a sessions_snapshot to validate that the target session exists
//	   and is NOT in `waiting` state. The waiting-state guard mirrors
//	   prism's: a waiting session is paused for direct user input, and a
//	   programmatic prompt corrupts the input field.
//	4. Send the prompt_deliver frame.
//	5. Read for a brief window. The daemon's protocol does NOT emit an
//	   explicit ack on success — handlePromptDeliver only writes a frame
//	   on FAILURE (an error frame). Treat read-timeout / EOF-with-no-error
//	   as success.
//
// # Behaviour summary (acceptance criteria for #1677)
//
//   - --prompt <text> | --prompt-file <path> | --prompt -  (mutually exclusive
//     --prompt vs --prompt-file; `--prompt -` reads stdin)
//   - Trailing single newline stripped from file/stdin input.
//   - Daemon down → "iris daemon not running … systemctl --user start iris".
//   - No such session → clear error, no stack trace.
//   - Session in waiting state → refuse, exit non-zero with the documented
//     "switch and respond directly" message.
//   - Mid-send connection drop → "lost connection to daemon", no hang.
//   - On success: prints "prompt delivered to <session>" and exits 0.

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

// promptDialTimeout bounds how long `iris prompt` will wait when dialling
// the daemon socket. Matches the spawn-side budget — the daemon is local,
// a healthy dial completes in microseconds, and a multi-second hang almost
// certainly means the daemon is not running.
const promptDialTimeout = 2 * time.Second

// promptWriteTimeout bounds the write of the prompt_deliver frame.
const promptWriteTimeout = 5 * time.Second

// promptAckWindow is how long we wait after sending prompt_deliver to see
// whether the daemon returns an error frame. The current daemon protocol
// has NO success ack for prompt_deliver — only an error frame on failure
// (see internal/iris/client_socket.go::handlePromptDeliver). We therefore
// wait briefly: any error frame must arrive within this window or we treat
// the delivery as successful. The daemon's deliverFn is synchronous w.r.t.
// the SendRPC call, so a real failure surfaces well within this budget.
//
// This is deliberately short. A longer window would slow down every
// successful invocation. If a future protocol revision adds an explicit
// ack frame, callers should switch to read-until-ack rather than a fixed
// window — see the comment in readPromptAck for the path.
const promptAckWindow = 1500 * time.Millisecond

// promptCmd is the `iris prompt` subcommand. It dials the daemon client
// socket and asks the daemon to forward the prompt to the named session.
var promptCmd = &cobra.Command{
	Use:   "prompt <session>",
	Short: "Send a prompt to a running iris session via the daemon",
	Long: `iris prompt dials the iris daemon's client IPC socket and asks it to
deliver a prompt to the named session.

The daemon must already be running. If it is not, this command exits
non-zero with a clear error pointing at 'systemctl --user start iris'.

Three input variants are accepted:

  --prompt <text>          inline prompt text
  --prompt-file <path>     read the prompt body from a file
  --prompt -               read the prompt body from stdin

--prompt and --prompt-file are mutually exclusive. For file/stdin input a
single trailing newline is stripped (matches prism's behaviour).

Note: a literal "-" cannot be sent as a prompt via --prompt because it is
reserved as the stdin sentinel. Use --prompt-file for that case.

# Waiting-state guard

If the target session is in 'waiting' state — meaning the agent has paused
for direct user input — this command refuses to send. Injecting a
programmatic prompt would corrupt the input field. Switch to the session
in the TUI (C-f or C-w) and respond directly instead.`,
	Args:          cobra.ExactArgs(1),
	RunE:          runPromptCmd,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	promptCmd.Flags().String(
		"prompt", "",
		"Text to send to the agent. Use --prompt-file for complex strings or to avoid shell-escaping issues. Use '-' to read from stdin.",
	)
	promptCmd.Flags().String(
		"prompt-file", "",
		"Path to a file containing the prompt text. Mutually exclusive with --prompt.",
	)
	promptCmd.Flags().String(
		"socket", "",
		"Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)",
	)
	promptCmd.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
	rootCmd.AddCommand(promptCmd)
}

// runPromptCmd is the cobra entry point. It resolves the input, then defers
// to runPromptAt for the wire path so that integration tests can drive the
// flow against an in-process ClientSocket.
func runPromptCmd(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	promptText, err := resolveIrisPromptInput(cmd)
	if err != nil {
		return err
	}

	sockPath := resolveSocketPath(cmd)

	return runPromptAt(cmd.Context(), sockPath, sessionName, promptText, os.Stdin, os.Stdout)
}

// runPromptAt is the testable core of `iris prompt`. sockPath is passed
// explicitly so integration tests can point it at a t.TempDir() socket.
// stdin is currently unused but is passed in to mirror runSpawnAt's shape
// and to keep the door open for future ReadAll-from-stdin paths.
func runPromptAt(ctx context.Context, sockPath, sessionName, promptText string, _ io.Reader, out io.Writer) error {
	if sessionName == "" {
		return errors.New("iris prompt: <session> is required")
	}
	if promptText == "" {
		return errors.New(
			"a prompt is required — supply one of:\n" +
				"  --prompt <text>\n" +
				"  --prompt - (read from stdin)\n" +
				"  --prompt-file <path>",
		)
	}

	// Phase 1: validate target session via sessions_snapshot.
	// This reuses the existing daemon surface — no new request frame needed.
	// It also exercises the "daemon not running" error path before we
	// commit to opening a second connection for prompt_deliver.
	if err := assertSessionAcceptsPrompt(ctx, sockPath, sessionName); err != nil {
		return err
	}

	// Phase 2: send the prompt_deliver frame on a fresh connection.
	conn, err := dialDaemonForPrompt(ctx, sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := sendPromptDeliverFrame(conn, sessionName, promptText); err != nil {
		return err
	}

	// Phase 3: brief read window for an error frame. No news is good news.
	if err := readPromptAck(ctx, conn); err != nil {
		return err
	}

	fmt.Fprintf(out, "prompt delivered to %s\n", sessionName)
	return nil
}

// resolveIrisPromptInput reads the prompt body from --prompt, --prompt-file,
// or stdin (when --prompt is "-"). It mirrors the behaviour of prism's
// resolvePromptWithSource in cmd/prompt_input.go:
//
//   - --prompt-file <path>  → file contents, single trailing newline stripped
//   - --prompt -            → stdin contents, single trailing newline stripped
//   - --prompt <text>       → text verbatim
//
// Cobra enforces the mutual exclusion of --prompt vs --prompt-file before
// RunE is called, so no manual check is needed here.
func resolveIrisPromptInput(cmd *cobra.Command) (string, error) {
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

// assertSessionAcceptsPrompt fetches a sessions_snapshot from the daemon,
// looks up sessionName, and returns:
//
//   - nil                    — session exists and is NOT in waiting state.
//   - "daemon not running"   — the snapshot fetch failed because the daemon
//                              is not reachable (fetchSessionsSnapshot wraps
//                              this with the canonical wording).
//   - "no such session"      — the snapshot returned but did not contain
//                              sessionName.
//   - waiting-state refusal  — the named session exists with state=="waiting".
func assertSessionAcceptsPrompt(ctx context.Context, sockPath, sessionName string) error {
	snap, err := fetchSessionsSnapshot(ctx, sockPath)
	if err != nil {
		return err
	}
	for _, s := range snap.Sessions {
		if s.Name != sessionName {
			continue
		}
		if s.State == "waiting" {
			// Mirror prism's documented message verbatim (minus the
			// `prism checkin` hint, which has no iris equivalent yet —
			// the C-f / C-w tmux switch hint applies in both worlds).
			return fmt.Errorf(
				"session %q is waiting for user input\n\n"+
					"The agent has paused and is expecting a direct response from the user.\n"+
					"Please switch to that session and respond there, or escalate to the user\n"+
					"so they can address it directly.\n\n"+
					"  (C-f or C-w)       — switch to the session in tmux",
				sessionName,
			)
		}
		return nil
	}
	// Build a short list of known names for a helpful error.
	names := make([]string, 0, len(snap.Sessions))
	for _, s := range snap.Sessions {
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return fmt.Errorf(
			"iris prompt: no such session %q — no active sessions reported by daemon (run `iris sessions list` to verify)",
			sessionName,
		)
	}
	return fmt.Errorf(
		"iris prompt: no such session %q — active sessions: %s",
		sessionName, strings.Join(names, ", "),
	)
}

// dialDaemonForPrompt dials the daemon client socket for the prompt_deliver
// frame. It uses the same canonical "daemon not running" wording as
// `iris spawn`. We re-dial (rather than reusing the connection used for
// sessions_list) to keep the two phases independent: the sessions_snapshot
// fetch can use its own short read window, and the prompt_deliver write
// path doesn't need to share state with it.
func dialDaemonForPrompt(ctx context.Context, sockPath string) (net.Conn, error) {
	d := net.Dialer{Timeout: promptDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf(
			"iris daemon not running (could not dial %s: %v); start it with: systemctl --user start iris",
			sockPath, err,
		)
	}
	return conn, nil
}

// sendPromptDeliverFrame marshals and writes a ClientPromptDeliverFrame to
// the daemon connection. The frame format is the D-6 wire protocol
// (internal/iris/client_protocol.go); this function MUST NOT add fields not
// defined there.
func sendPromptDeliverFrame(conn net.Conn, sessionName, promptText string) error {
	frame := iris.ClientPromptDeliverFrame{
		Type: iris.ClientFramePromptDeliver,
		Name: sessionName,
		Text: promptText,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("iris prompt: marshal prompt_deliver: %w", err)
	}
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(promptWriteTimeout))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("iris prompt: lost connection to daemon during send: %w", err)
	}
	return nil
}

// readPromptAck waits up to promptAckWindow for an error frame from the
// daemon. The current protocol does NOT emit a success ack — handlePromptDeliver
// only writes a frame on failure (an error frame with request_type ==
// "prompt_deliver"). We therefore wait briefly and treat:
//
//   - error frame received        → return the daemon-side error.
//   - read deadline / timeout     → success (no error means delivered).
//   - EOF before deadline         → if context is still live, "lost
//                                   connection to daemon". The daemon
//                                   closes the connection if we close
//                                   first; we don't, so EOF here means
//                                   the daemon went away mid-window.
//   - context cancelled           → return ctx.Err().
//
// If a future protocol revision adds an explicit prompt_delivered ack
// frame, swap the timeout branch for a hard-fail and accept the new frame
// type here.
func readPromptAck(ctx context.Context, conn net.Conn) error {
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

	_ = conn.SetReadDeadline(time.Now().Add(promptAckWindow))
	r := bufio.NewReaderSize(conn, 1<<20)

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				// Timeout with no error frame → delivery succeeded.
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New("iris prompt: lost connection to daemon before ack")
			}
			return fmt.Errorf("iris prompt: read ack: %w", err)
		}

		var generic struct {
			Type        string `json:"type"`
			RequestType string `json:"request_type"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			// Malformed frame — keep reading; the daemon may follow up
			// with a real error. Don't treat as fatal.
			fmt.Fprintf(os.Stderr, "[iris] warning: ignoring malformed frame from daemon: %v\n", err)
			continue
		}
		switch generic.Type {
		case iris.DaemonFrameError:
			// Only treat error frames whose request_type matches as fatal;
			// stray errors from other in-flight requests on the same
			// connection shouldn't surface here (we use a fresh conn) but
			// being explicit avoids surprises.
			if generic.RequestType == "" || generic.RequestType == iris.ClientFramePromptDeliver {
				if strings.Contains(generic.Message, "not found") ||
					strings.Contains(generic.Message, "no such session") {
					return fmt.Errorf("iris prompt: no such session: %s", generic.Message)
				}
				return fmt.Errorf("iris prompt: daemon rejected prompt: %s", generic.Message)
			}
			// Unrelated error frame — log and keep waiting.
			fmt.Fprintf(os.Stderr, "[iris] note: unrelated error frame (request_type=%q): %s\n", generic.RequestType, generic.Message)
		default:
			// Any other frame type is unexpected on this connection (we
			// did not subscribe). Skip and keep reading until timeout.
		}
	}
}
