package cmd

// prism feedback — a friction log for agents and humans (issue #1505 /
// principle 10 of #1497). Records short notes about CLI rough edges to a
// local JSONL store at $XDG_STATE_HOME/prism/feedback.jsonl, with optional
// upstream POST when PRISM_FEEDBACK_ENDPOINT is set.
//
// Surface:
//
//	prism feedback "<text>"          # record one note
//	prism feedback -                  # read note from stdin (mirrors --prompt -)
//	prism feedback list               # list local notes (human-readable)
//	prism feedback list --json        # JSON array of all notes
//	prism feedback list --days N      # only entries newer than N days
//	prism feedback prune --days N --yes  # drop entries older than N days
//
// The store is local-first: a missing or failing upstream endpoint never
// loses the local record. Upstream HTTP status (when configured) is
// surfaced in the success message so the operator can see what happened.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/feedback"
)

// PRISMFeedbackEndpointEnv is the environment variable that, when non-empty,
// causes `prism feedback` to POST each newly-recorded entry to the named URL
// after writing it locally. A failure to POST does not affect the local
// record (the local copy is the source of truth).
const PRISMFeedbackEndpointEnv = "PRISM_FEEDBACK_ENDPOINT"

// feedbackUpstreamTimeout caps how long a single upstream POST can run
// before we give up and report the timeout in the success message. The
// local record is already on disk before the POST is attempted.
const feedbackUpstreamTimeout = 30 * time.Second

// feedbackUpstreamClient is the HTTP client used for the upstream POST.
// Overridable from tests via the package-level variable so test servers
// can intercept requests without monkey-patching net/http.
var feedbackUpstreamClient = &http.Client{Timeout: feedbackUpstreamTimeout}

// feedbackStorePathFn lets tests inject a writable store path. When nil
// (production) the path is resolved from $XDG_STATE_HOME via
// feedback.DefaultPath. The injection point exists because the nix-build
// sandbox has HOME=/homeless-shelter; tests set XDG_STATE_HOME to a
// t.TempDir() and the production code path picks that up automatically.
var feedbackStorePathFn = func() (string, error) { return feedback.DefaultPath() }

// resolveFeedbackStore returns a Store rooted at the configured path.
func resolveFeedbackStore() (*feedback.Store, error) {
	path, err := feedbackStorePathFn()
	if err != nil {
		return nil, err
	}
	return feedback.NewStore(path), nil
}

var feedbackCmd = &cobra.Command{
	Use:   "feedback [text|-]",
	Short: "Record a short note about CLI friction (local JSONL log)",
	Long: `Record a short note about CLI friction.

Each entry is appended as one JSON object per line to
$XDG_STATE_HOME/prism/feedback.jsonl. The store is local-first: a missing
or failing upstream endpoint never loses the local record.

  prism feedback "the --tier flag rejects 'enterprise' but the docs list it"
  echo "feedback from a script" | prism feedback -

When PRISM_FEEDBACK_ENDPOINT is set, the entry is also POSTed upstream
after being written locally. The HTTP status is reported in the success
message.

Subcommands:
  prism feedback list              human-readable list of local entries
  prism feedback list --json       JSON array of all entries
  prism feedback list --days N     only entries within the last N days
  prism feedback prune --days N --yes
                                   drop entries older than N days
`,
	Args: cobra.MaximumNArgs(1),
	RunE: runFeedbackRecord,
}

var feedbackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recorded feedback entries",
	Args:  cobra.NoArgs,
	RunE:  runFeedbackList,
}

var feedbackPruneCmd = &cobra.Command{
	Use:   "prune --days N --yes",
	Short: "Remove feedback entries older than N days (requires --yes)",
	Args:  cobra.NoArgs,
	RunE:  runFeedbackPrune,
}

func init() {
	feedbackListCmd.Flags().Bool("json", false, "Emit the JSONL contents as a JSON array")
	feedbackListCmd.Flags().Int("days", 0, "Only show entries within the last N days (0 = no filter)")

	feedbackPruneCmd.Flags().Int("days", 0, "Remove entries older than N days (required, must be > 0)")
	feedbackPruneCmd.Flags().Bool("yes", false, "Confirm the prune. Without --yes, prune errors instead of prompting (principle 1)")

	feedbackCmd.AddCommand(feedbackListCmd)
	feedbackCmd.AddCommand(feedbackPruneCmd)
	rootCmd.AddCommand(feedbackCmd)
}

// runFeedbackRecord handles `prism feedback "<text>"` and `prism feedback -`.
func runFeedbackRecord(cmd *cobra.Command, args []string) error {
	text, err := readFeedbackText(args, os.Stdin)
	if err != nil {
		return err
	}

	entry := buildFeedbackEntry(text, time.Now().UTC())

	// Sandbox proxy path: when $PRISM_HOST_API is set the process is running
	// inside a bwrap worker sandbox whose filesystem namespace is ephemeral.
	// Writes to ~/.local/state/prism/ inside the sandbox never reach the host,
	// so the data would be silently lost on sandbox exit (issue #1644).
	// Route through the host-API instead so the sidecar — which runs on the
	// host — performs the actual append.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return runFeedbackRecordViaHostAPI(cmd, apiURL, entry)
	}

	// Host path: write directly to the local feedback store.
	store, err := resolveFeedbackStore()
	if err != nil {
		return err
	}
	if err := store.Append(entry); err != nil {
		return err
	}

	endpoint := resolveFeedbackEndpoint()
	if endpoint == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "feedback recorded locally (%s)\n", store.Path)
		return nil
	}
	status, postErr := postFeedbackUpstream(endpoint, entry)
	if postErr != nil {
		// Local record is unaffected; surface the upstream failure but exit 0.
		fmt.Fprintf(cmd.OutOrStdout(),
			"feedback recorded locally (%s); upstream POST failed: %v\n",
			store.Path, postErr)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"feedback recorded locally and sent upstream (status: %d)\n", status)
	return nil
}

// runFeedbackRecordViaHostAPI proxies the feedback entry to the host-API
// sidecar when running inside a bwrap sandbox (PRISM_HOST_API is set).
// The sidecar appends the entry to the host's feedback.jsonl and returns
// the resolved path, which is printed in the success message so the
// worker sees where the data actually landed (AC: "message prints the path
// the entry actually landed at").
//
// On any error (missing socket, HTTP error, malformed response) this
// function returns a non-nil error and does NOT fall back to the sandbox-
// internal write path — that fallback is the current failure mode this fix
// is resolving (issue #1644).
func runFeedbackRecordViaHostAPI(cmd *cobra.Command, apiURL string, entry feedback.Entry) error {
	var resp struct {
		Path string `json:"path"`
	}
	if err := proxyToHostAPI(apiURL, "/feedback", entry, &resp); err != nil {
		return fmt.Errorf("feedback: host-API proxy failed (not writing to sandbox-local path): %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "feedback recorded via host-API (%s)\n", resp.Path)
	return nil
}

// readFeedbackText resolves the feedback text from either the positional
// argument or stdin (when the argument is "-"). Returns an error when no
// argument is supplied and stdin is a terminal — the AC requires a
// non-zero exit with usage in that case.
func readFeedbackText(args []string, stdin io.Reader) (string, error) {
	if len(args) == 0 {
		return "", errors.New(
			"a feedback text is required — supply one of:\n" +
				"  prism feedback \"<text>\"\n" +
				"  prism feedback - (read from stdin)",
		)
	}
	arg := args[0]
	if arg != "-" {
		s := strings.TrimSpace(arg)
		if s == "" {
			return "", errors.New("feedback text is empty")
		}
		return s, nil
	}
	// Stdin path. Refuse if stdin is a terminal so the user gets a clear
	// error rather than a hung command.
	if f, ok := stdin.(*os.File); ok {
		info, statErr := f.Stat()
		if statErr == nil && (info.Mode()&os.ModeCharDevice) != 0 {
			return "", errors.New(
				"prism feedback -: stdin is a terminal; pipe text or use prism feedback \"<text>\"",
			)
		}
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", errors.New("prism feedback -: stdin produced no text")
	}
	return s, nil
}

// buildFeedbackEntry assembles an Entry from the recording context. The
// minimum required fields (timestamp, text, session, prism_version) are
// always populated; optional fields are filled when discoverable.
func buildFeedbackEntry(text string, now time.Time) feedback.Entry {
	e := feedback.Entry{
		Timestamp:    now.Format(time.RFC3339),
		Text:         text,
		Session:      os.Getenv("PRISM_SESSION_NAME"),
		PrismVersion: archive.PrismGitSHA(),
	}
	if cwd, err := os.Getwd(); err == nil {
		e.CWD = cwd
	}
	return e
}

// postFeedbackUpstream POSTs a single entry to endpoint as JSON. Returns
// the HTTP status code on success. A non-2xx response is treated as an
// error so the caller can surface it (the local record is already saved).
func postFeedbackUpstream(endpoint string, e feedback.Entry) (int, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("marshal entry: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedbackUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := feedbackUpstreamClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// resolveFeedbackEndpoint returns the configured upstream URL: the
// PRISM_FEEDBACK_ENDPOINT environment variable wins over the
// feedback_endpoint config key, matching the precedence used elsewhere
// in the codebase (env > config). Returns "" when neither is set.
func resolveFeedbackEndpoint() string {
	if env := strings.TrimSpace(os.Getenv(PRISMFeedbackEndpointEnv)); env != "" {
		return env
	}
	// LoadFresh (not Load) so a test that sets PRISM_CONFIG_FILE between
	// invocations sees the new value; Load is sync.Once-cached.
	return strings.TrimSpace(config.LoadFresh().FeedbackEndpoint)
}

// runFeedbackList implements `prism feedback list [--json] [--days N]`.
func runFeedbackList(cmd *cobra.Command, _ []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	days, _ := cmd.Flags().GetInt("days")
	if days < 0 {
		return fmt.Errorf("--days must be a non-negative integer")
	}

	store, err := resolveFeedbackStore()
	if err != nil {
		return err
	}
	entries, err := store.List()
	if err != nil {
		return err
	}
	if days > 0 {
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		entries = feedback.FilterSince(entries, cutoff)
	}

	w := cmd.OutOrStdout()
	if jsonOut {
		// AC: --json emits a JSON array. Use a non-nil slice so the empty
		// case marshals to [] rather than null.
		if entries == nil {
			entries = []feedback.Entry{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	for _, e := range entries {
		fmt.Fprintf(w, "%s  %s\n", e.Timestamp, e.Text)
	}
	return nil
}

// runFeedbackPrune implements `prism feedback prune --days N --yes`.
//
// Per principle 1 (no implicit confirmations), --yes is mandatory. Without
// it we error out instead of prompting; the operator has to opt in
// explicitly.
func runFeedbackPrune(cmd *cobra.Command, _ []string) error {
	days, _ := cmd.Flags().GetInt("days")
	yes, _ := cmd.Flags().GetBool("yes")

	if days <= 0 {
		return fmt.Errorf("--days must be a positive integer (e.g. --days 30)")
	}
	if !yes {
		return fmt.Errorf(
			"prism feedback prune requires --yes to confirm (principle 1: no implicit confirmations)\n" +
				"    re-run with --yes to drop entries older than the cutoff",
		)
	}

	store, err := resolveFeedbackStore()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	kept, removed, err := store.Prune(cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"feedback pruned: removed %d entries older than %s; kept %d (%s)\n",
		removed, cutoff.Format(time.RFC3339), kept, store.Path)
	return nil
}
