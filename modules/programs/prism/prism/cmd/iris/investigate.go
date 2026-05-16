package main

// investigate.go — `iris investigate` subcommand.
//
// `iris investigate` is the daemon-routed analogue of `prism investigate`
// (cmd/investigate.go). It spawns a read-only investigator session and
// returns the session name to stdout. The investigator is a child of the
// calling iris session: its terminal-state notifications flow back via
// the existing NotifyParent path (issue #1700, wired in main.go's
// makeNotifyParent and the supervisor's setState callback) so the
// coordinator sees an "Agent <name> has finished its current task" prompt
// once the investigation reaches a clean terminal state — the same
// exactly-once contract `iris escalate` and `iris spawn` inherit.
//
// Surface:
//
//	iris investigate --prompt "<question>"
//	iris investigate --prompt-file <path>
//	iris investigate --prompt -                    # stdin
//	iris investigate --name <slug> --prompt "..."  # explicit session-name slug
//
// --name constraints match prism's investigate (`[a-z0-9-]`, max 40 chars,
// no leading/trailing dash). Validation is shared with prism via
// internal/investigate.ValidateName.
//
// Behaviour summary (acceptance criteria for #1720):
//
//   - --prompt <text> | --prompt-file <path> | --prompt -
//   - Returns within ~2 seconds with the session name on stdout.
//   - The investigator's role is set to "investigate". This drives:
//       * tool_dispatcher gates `write`/`edit` with a clear "read-only" error;
//       * bash_permission.CheckBashPermission denies the investigator
//         deny list (prism spawn/review/merge, gh issue create/edit/..., ...).
//   - Daemon down            → canonical "systemctl --user start iris" hint.
//   - Caller not in an iris session → "not a registered iris session" error.
//   - Invalid --name slug    → exits before any wire activity with a clear error.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
	investigatepkg "github.com/prismatic-koi/prism/internal/investigate"
)

// investigateDialTimeout bounds how long `iris investigate` will wait when
// dialling the daemon socket. Same budget as `iris spawn`.
const investigateDialTimeout = 2 * time.Second

// investigateWriteTimeout bounds the write of the session_spawn frame.
const investigateWriteTimeout = 5 * time.Second

// investigateAckWindow is how long we wait after sending session_spawn to
// see the daemon's session_spawned (or error) frame. The daemon's spawn work
// (process fork + harness socket bind) typically completes well under a
// second; a 30 s ceiling matches `iris spawn`.
const investigateAckWindow = 30 * time.Second

// investigateCmd is the `iris investigate` subcommand.
var investigateCmd = &cobra.Command{
	Use:   "investigate",
	Short: "Spawn a read-only investigator session and return immediately",
	Long: `iris investigate dials the iris daemon's client IPC socket and asks it
to spawn a read-only investigator session as a child of the calling iris
session. The session is named <calling-session>~investigate-<slug>, where
the slug is derived from the prompt or supplied via --name.

The investigator role is read-only:

  - The 'write' and 'edit' tools fail with a clear "investigator is
    read-only" message at the iris tool-dispatch layer.
  - The 'bash' tool's permission gate denies state-mutating invocations:
    prism spawn / review / merge, iris spawn / review / merge,
    gh issue create / edit / close / comment, gh pr create / edit /
    merge / close / review / comment, and git push / commit / add /
    rebase / reset.
  - Read-only operations (rg, grep, find, cat, git log, git show, gh
    issue view / pr view / pr diff, etc.) are all permitted.

The investigator does not self-terminate. Each turn's output is delivered
back to the calling session via the existing parent-notification path
(issue #1700 / #1727) when the turn reaches a clean terminal state. The
coordinator runs 'iris cleanup' once the investigation is done.

Three input variants are accepted (mirrors 'iris prompt'):

  --prompt <text>          inline prompt text
  --prompt-file <path>     read the prompt body from a file
  --prompt -               read the prompt body from stdin

--prompt and --prompt-file are mutually exclusive. For file/stdin input a
single trailing newline is stripped.

The --name flag overrides the auto-derived slug:

    iris investigate --name my-analysis --prompt "..."

results in a session named <caller>~investigate-my-analysis. Validation
rules for --name:

  - Only lowercase alphanumerics and dashes ([a-z0-9-]) are allowed.
  - Must not start or end with a dash.
  - Maximum 40 characters.

When --name is omitted, the slug is derived automatically from the prompt
text using the same rules as 'prism investigate' (lowercased, punctuation
stripped, max 30 chars, trimmed at the last word boundary).

The calling session is identified via $IRIS_SESSION_NAME, set by the iris
supervisor for every pi child. Outside an iris-managed session, pass --from
explicitly.`,
	Args:          cobra.NoArgs,
	RunE:          runInvestigateCmd,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	investigateCmd.Flags().String(
		"prompt", "",
		"Text to send to the investigator. Use --prompt-file for complex strings or to avoid shell-escaping issues. Use '-' to read from stdin.",
	)
	investigateCmd.Flags().String(
		"prompt-file", "",
		"Path to a file containing the prompt text. Mutually exclusive with --prompt.",
	)
	investigateCmd.Flags().String(
		"name", "",
		"Human-readable slug for the session name (only [a-z0-9-], max 40 chars, no leading/trailing dash). Resulting session name is <caller>~investigate-<name>.",
	)
	investigateCmd.Flags().String(
		"from", "",
		"Calling session name (defaults to $IRIS_SESSION_NAME). Useful for scripts that spawn investigators on behalf of a known session.",
	)
	investigateCmd.Flags().String(
		"socket", "",
		"Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)",
	)
	investigateCmd.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
	rootCmd.AddCommand(investigateCmd)
}

// runInvestigateCmd is the cobra entry point. It resolves the input, the
// calling session name, and the slug; validates them; then defers to
// runInvestigateAt for the wire path.
func runInvestigateCmd(cmd *cobra.Command, args []string) error {
	promptText, err := resolveInvestigatePromptInput(cmd)
	if err != nil {
		return err
	}
	if strings.TrimSpace(promptText) == "" {
		return errors.New(
			"a prompt is required — supply one of:\n" +
				"  --prompt <text>\n" +
				"  --prompt - (read from stdin)\n" +
				"  --prompt-file <path>",
		)
	}

	suppliedName, _ := cmd.Flags().GetString("name")
	suppliedName = strings.TrimSpace(suppliedName)
	if suppliedName != "" {
		if err := investigatepkg.ValidateName(suppliedName); err != nil {
			// Re-shape the prism-flavoured error message under the iris
			// prefix so the CLI surface is consistent.
			return errors.New(strings.Replace(err.Error(), "prism investigate:", "iris investigate:", 1))
		}
	}

	fromFlag, _ := cmd.Flags().GetString("from")
	from := fromFlag
	if from == "" {
		from = os.Getenv("IRIS_SESSION_NAME")
	}
	if from == "" {
		return errors.New(
			"iris investigate: could not determine calling session\n" +
				"hint: run from inside an iris-managed pi session (where $IRIS_SESSION_NAME is set),\n" +
				"or pass --from <session-name> explicitly",
		)
	}

	sockPath := resolveSocketPath(cmd)
	return runInvestigateAt(cmd.Context(), sockPath, from, suppliedName, promptText, os.Stdout)
}

// runInvestigateAt is the testable core of `iris investigate`. sockPath is
// passed explicitly so integration tests can point it at a t.TempDir()
// socket. Returns nil on success and prints the new session name to out.
func runInvestigateAt(ctx context.Context, sockPath, from, suppliedName, promptText string, out io.Writer) error {
	if from == "" {
		return errors.New("iris investigate: from is required")
	}
	if promptText == "" {
		return errors.New("iris investigate: prompt is required")
	}

	// Resolve the slug + session name BEFORE dialling so an invalid
	// --name fails fast without ever touching the daemon.
	slug := suppliedName
	if slug == "" {
		slug = investigateSlug(promptText)
	}
	sessionName := from + "~investigate-" + slug

	// Resolve the calling session's worktree via a sessions_snapshot. The
	// investigator must run against the same worktree as the caller so
	// that read-only inspection sees the same HEAD the coordinator is on.
	// The lookup also doubles as a "calling session is registered" gate:
	// when the daemon returns no matching session, we exit before spawn
	// with a clear "not a registered iris session" error (per AC).
	worktree, role, err := resolveCallerWorktree(ctx, sockPath, from)
	if err != nil {
		return err
	}
	// Defence-in-depth: do not allow an investigator to spawn from inside
	// another investigator session. The issue's Out-of-scope clause is
	// explicit about no nesting.
	if role == "investigate" {
		return fmt.Errorf("iris investigate: calling session %q is itself an investigator; investigators may not spawn further investigators", from)
	}

	conn, err := dialDaemonForInvestigate(ctx, sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := sendInvestigateSpawnFrame(conn, sessionName, worktree, from); err != nil {
		return err
	}

	ack, err := readInvestigateAck(ctx, conn)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, ack.Name)
	return nil
}

// resolveInvestigatePromptInput reads the prompt body from --prompt,
// --prompt-file, or stdin (when --prompt is "-"). Mirrors resolveIrisPromptInput
// in prompt.go byte-for-byte so the two CLIs share the same conventions.
func resolveInvestigatePromptInput(cmd *cobra.Command) (string, error) {
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

// resolveCallerWorktree fetches a sessions_snapshot from the daemon and
// returns the worktree and role of the named session. It also acts as the
// "registered iris session" check (AC: caller must be a registered iris
// session). Errors are shaped for direct CLI display.
func resolveCallerWorktree(ctx context.Context, sockPath, from string) (worktree, role string, err error) {
	snap, ferr := fetchSessionsSnapshot(ctx, sockPath)
	if ferr != nil {
		return "", "", ferr
	}
	for _, s := range snap.Sessions {
		if s.Name == from {
			return s.Worktree, s.Role, nil
		}
	}
	return "", "", fmt.Errorf(
		"iris investigate: not a registered iris session: %q (run `iris sessions list` to verify)",
		from,
	)
}

// dialDaemonForInvestigate dials the daemon client socket. Same canonical
// "daemon not running" wording as `iris spawn` / `iris prompt`.
func dialDaemonForInvestigate(ctx context.Context, sockPath string) (net.Conn, error) {
	if _, err := os.Stat(sockPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"iris daemon not running: socket %s does not exist; start it with `systemctl --user start iris`",
				sockPath,
			)
		}
		return nil, fmt.Errorf("iris investigate: stat socket %s: %w", sockPath, err)
	}
	d := net.Dialer{Timeout: investigateDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf(
			"iris daemon not running (could not dial %s: %v); start it with `systemctl --user start iris`",
			sockPath, err,
		)
	}
	return conn, nil
}

// sendInvestigateSpawnFrame marshals and writes a session_spawn frame that
// asks the daemon to spawn an investigator session with a pre-computed
// session name, worktree borrowed from the caller, role="investigate",
// and parent=from (so the existing NotifyParent path delivers terminal
// notifications back to the caller).
func sendInvestigateSpawnFrame(conn net.Conn, sessionName, worktree, from string) error {
	frame := iris.ClientSessionSpawnFrame{
		Type:        iris.ClientFrameSessionSpawn,
		Worktree:    worktree,
		Role:        "investigate",
		Parent:      from,
		SessionName: sessionName,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("iris investigate: marshal session_spawn: %w", err)
	}
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(investigateWriteTimeout))
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("iris investigate: lost connection to daemon during send: %w", err)
	}
	return nil
}

// readInvestigateAck reads frames from the daemon until it sees either a
// session_spawned or an error frame. Mirrors readSpawnAck but typed to the
// investigate wire path so the error messages mention "iris investigate".
func readInvestigateAck(ctx context.Context, conn net.Conn) (*iris.DaemonSessionSpawnedFrame, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(investigateAckWindow))
	r := bufio.NewReaderSize(conn, 1<<20)

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return nil, fmt.Errorf("iris investigate: timed out after %s waiting for session_spawned ack", investigateAckWindow)
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("iris investigate: daemon closed connection before sending session_spawned ack (daemon may have died mid-spawn)")
			}
			return nil, fmt.Errorf("iris investigate: read ack: %w", err)
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
		case iris.DaemonFrameSessionSpawned:
			var frame iris.DaemonSessionSpawnedFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				return nil, fmt.Errorf("iris investigate: parse session_spawned: %w", err)
			}
			return &frame, nil
		case iris.DaemonFrameError:
			if head.RequestType == "" || head.RequestType == iris.ClientFrameSessionSpawn {
				return nil, fmt.Errorf("iris investigate: daemon rejected spawn: %s", head.Message)
			}
			fmt.Fprintf(os.Stderr, "[iris] note: unrelated error frame (request_type=%q): %s\n", head.RequestType, head.Message)
		default:
			// Unknown frame on this connection — skip and keep reading.
		}
	}
}

// investigateSlug derives a short kebab-case slug from the prompt text.
// Identical rules to prism's cmd/investigate.go::investigateSlug:
// lowercase, strip punctuation, replace spaces/underscores with "-",
// truncate to ≤30 chars at a word boundary, trim trailing "-".
//
// Kept as a literal copy (not a shared package import) because the prism
// CLI helper lives in package cmd; an iris-side dependency on package cmd
// would pull all of the prism subcommand machinery into the iris binary.
// The shared validation helper (ValidateName for --name) is small enough
// to live in its own package (internal/investigate); the slug derivation
// is a single function and stays here.
func investigateSlug(prompt string) string {
	s := strings.ToLower(prompt)
	s = strings.Map(func(r rune) rune {
		if r == '_' || unicode.IsSpace(r) {
			return '-'
		}
		return r
	}, s)
	var b strings.Builder
	for _, r := range s {
		if r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s = b.String()
	multiDash := regexp.MustCompile(`-{2,}`)
	s = multiDash.ReplaceAllString(s, "-")
	s = strings.TrimLeft(s, "-")
	if len(s) > 30 {
		cap := s[:30]
		if idx := strings.LastIndex(cap, "-"); idx >= 0 {
			s = s[:idx]
		} else {
			s = cap
		}
	}
	s = strings.TrimRight(s, "-")
	if s == "" {
		return "query"
	}
	return s
}
