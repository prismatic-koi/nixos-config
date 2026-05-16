package main

// iris feedback — friction log analogue of `prism feedback` (issue #1721).
//
// Records short notes about iris CLI rough edges to a local JSONL store at
// $XDG_STATE_HOME/iris/feedback.jsonl, with optional upstream POST when
// IRIS_FEEDBACK_ENDPOINT is set.
//
// Surface (mirrors prism feedback byte-for-byte except for the binary
// name and the env var):
//
//	iris feedback "<text>"           # record one note
//	iris feedback -                   # read note from stdin
//	iris feedback list                # list local notes (human-readable)
//	iris feedback list --json         # JSON array of all notes
//	iris feedback list --days N       # only entries newer than N days
//	iris feedback prune --days N --yes  # drop entries older than N days
//
// The store is local-first: a missing or failing upstream endpoint never
// loses the local record. Upstream HTTP status (when configured) is
// surfaced in the success message so the operator can see what happened.
//
// The Entry struct from internal/feedback is reused as-is so that a future
// tool can merge prism + iris feedback stores without schema gymnastics
// (per the watch-out on #1721). That means the JSON key `prism_version`
// is retained even when populated from the iris binary's git SHA — the
// field is a "tool version" slot, not a strict prism-only identifier.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/feedback"
)

// IRISFeedbackEndpointEnv is the environment variable that, when non-empty,
// causes `iris feedback` to POST each newly-recorded entry to the named URL
// after writing it locally. A failure to POST does not affect the local
// record (the local copy is the source of truth).
const IRISFeedbackEndpointEnv = "IRIS_FEEDBACK_ENDPOINT"

// irisFeedbackUpstreamTimeout caps how long a single upstream POST can run
// before we give up and report the timeout in the success message. The
// local record is already on disk before the POST is attempted.
const irisFeedbackUpstreamTimeout = 30 * time.Second

// irisFeedbackUpstreamClient is the HTTP client used for the upstream POST.
// Overridable from tests via the package-level variable so test servers
// can intercept requests without monkey-patching net/http.
var irisFeedbackUpstreamClient = &http.Client{Timeout: irisFeedbackUpstreamTimeout}

// irisFeedbackStorePathFn lets tests inject a writable store path. When nil
// (production) the path is resolved from $XDG_STATE_HOME via
// irisFeedbackDefaultPath. The injection point exists because the nix-build
// sandbox has HOME=/homeless-shelter; tests set XDG_STATE_HOME to a
// t.TempDir() and the production code path picks that up automatically.
var irisFeedbackStorePathFn = func() (string, error) { return irisFeedbackDefaultPath() }

// irisFeedbackDefaultPath returns the canonical iris feedback.jsonl path,
// honouring $XDG_STATE_HOME first and falling back to
// $HOME/.local/state/iris/feedback.jsonl.
//
// This is a deliberate near-duplicate of feedback.DefaultPath() — the only
// difference is the "iris" namespace directory instead of "prism". Reusing
// the prism path would conflate the two stores; the issue explicitly calls
// for separate stores at $XDG_STATE_HOME/iris/feedback.jsonl.
func irisFeedbackDefaultPath() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "iris", "feedback.jsonl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("iris feedback: cannot resolve store path — neither XDG_STATE_HOME nor HOME is set")
	}
	return filepath.Join(home, ".local", "state", "iris", "feedback.jsonl"), nil
}

// resolveIrisFeedbackStore returns a Store rooted at the configured path.
func resolveIrisFeedbackStore() (*feedback.Store, error) {
	path, err := irisFeedbackStorePathFn()
	if err != nil {
		return nil, err
	}
	return feedback.NewStore(path), nil
}

var feedbackCmd = &cobra.Command{
	Use:   "feedback [text|-]",
	Short: "Record a short note about iris CLI friction (local JSONL log)",
	Long: `Record a short note about iris CLI friction.

Each entry is appended as one JSON object per line to
$XDG_STATE_HOME/iris/feedback.jsonl. The store is local-first: a missing
or failing upstream endpoint never loses the local record.

  iris feedback "the --tier flag rejects 'enterprise' but the docs list it"
  echo "feedback from a script" | iris feedback -

When IRIS_FEEDBACK_ENDPOINT is set, the entry is also POSTed upstream
after being written locally. The HTTP status is reported in the success
message.

Subcommands:
  iris feedback list               human-readable list of local entries
  iris feedback list --json        JSON array of all entries
  iris feedback list --days N      only entries within the last N days
  iris feedback prune --days N --yes
                                   drop entries older than N days
`,
	Args:         cobra.MaximumNArgs(1),
	RunE:         runIrisFeedbackRecord,
	SilenceUsage: true,
}

var feedbackListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List recorded iris feedback entries",
	Args:         cobra.NoArgs,
	RunE:         runIrisFeedbackList,
	SilenceUsage: true,
}

var feedbackPruneCmd = &cobra.Command{
	Use:          "prune --days N --yes",
	Short:        "Remove iris feedback entries older than N days (requires --yes)",
	Args:         cobra.NoArgs,
	RunE:         runIrisFeedbackPrune,
	SilenceUsage: true,
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

// runIrisFeedbackRecord handles `iris feedback "<text>"` and `iris feedback -`.
func runIrisFeedbackRecord(cmd *cobra.Command, args []string) error {
	text, err := readIrisFeedbackText(args, os.Stdin)
	if err != nil {
		return err
	}

	entry := buildIrisFeedbackEntry(text, time.Now().UTC())

	store, err := resolveIrisFeedbackStore()
	if err != nil {
		return err
	}
	if err := store.Append(entry); err != nil {
		return err
	}

	endpoint := resolveIrisFeedbackEndpoint()
	if endpoint == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "feedback recorded locally (%s)\n", store.Path)
		return nil
	}
	status, postErr := postIrisFeedbackUpstream(endpoint, entry)
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

// readIrisFeedbackText resolves the feedback text from either the positional
// argument or stdin (when the argument is "-"). Returns an error when no
// argument is supplied — the AC requires a non-zero exit with usage in
// that case.
func readIrisFeedbackText(args []string, stdin io.Reader) (string, error) {
	if len(args) == 0 {
		return "", errors.New(
			"a feedback text is required — supply one of:\n" +
				"  iris feedback \"<text>\"\n" +
				"  iris feedback - (read from stdin)",
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
				"iris feedback -: stdin is a terminal; pipe text or use iris feedback \"<text>\"",
			)
		}
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", errors.New("iris feedback -: stdin produced no text")
	}
	return s, nil
}

// buildIrisFeedbackEntry assembles an Entry from the recording context.
// The Session field is auto-populated from IRIS_SESSION_NAME (set by the
// iris supervisor's buildEnv for every pi child — issue #1704/#1706); it
// remains empty when invoked from a non-iris shell.
//
// The PrismVersion field is reused as the "tool version" slot per the
// #1721 watch-out (keep field names byte-for-byte with cmd/feedback.go so
// future tooling can merge stores) — for iris-originated entries it
// carries the iris binary's VCS revision.
func buildIrisFeedbackEntry(text string, now time.Time) feedback.Entry {
	e := feedback.Entry{
		Timestamp:    now.Format(time.RFC3339),
		Text:         text,
		Session:      os.Getenv("IRIS_SESSION_NAME"),
		PrismVersion: archive.PrismGitSHA(),
	}
	if cwd, err := os.Getwd(); err == nil {
		e.CWD = cwd
	}
	return e
}

// postIrisFeedbackUpstream POSTs a single entry to endpoint as JSON. Returns
// the HTTP status code on success. A non-2xx response is treated as an
// error so the caller can surface it (the local record is already saved).
func postIrisFeedbackUpstream(endpoint string, e feedback.Entry) (int, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("marshal entry: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), irisFeedbackUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := irisFeedbackUpstreamClient.Do(req)
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

// resolveIrisFeedbackEndpoint returns the configured upstream URL from the
// IRIS_FEEDBACK_ENDPOINT environment variable. Unlike prism, iris has no
// config-file equivalent for this slot — env var only. Returns "" when
// unset.
func resolveIrisFeedbackEndpoint() string {
	return strings.TrimSpace(os.Getenv(IRISFeedbackEndpointEnv))
}

// runIrisFeedbackList implements `iris feedback list [--json] [--days N]`.
//
// Human-readable output is newest-first; --json emits the same set as a
// JSON array (also newest-first). An empty store prints nothing in
// human mode and `[]` in JSON mode.
func runIrisFeedbackList(cmd *cobra.Command, _ []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	days, _ := cmd.Flags().GetInt("days")
	if days < 0 {
		return fmt.Errorf("--days must be a non-negative integer")
	}

	store, err := resolveIrisFeedbackStore()
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

	// Newest-first ordering (AC). Store.List() returns append order
	// (oldest-first); reverse in place.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
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

// runIrisFeedbackPrune implements `iris feedback prune --days N --yes`.
//
// Per principle 1 (no implicit confirmations), --yes is mandatory. Without
// it we error out instead of prompting; the operator has to opt in
// explicitly.
func runIrisFeedbackPrune(cmd *cobra.Command, _ []string) error {
	days, _ := cmd.Flags().GetInt("days")
	yes, _ := cmd.Flags().GetBool("yes")

	if days <= 0 {
		return fmt.Errorf("--days must be a positive integer (e.g. --days 30)")
	}
	if !yes {
		return fmt.Errorf(
			"iris feedback prune requires --yes to confirm (principle 1: no implicit confirmations)\n" +
				"    re-run with --yes to drop entries older than the cutoff",
		)
	}

	store, err := resolveIrisFeedbackStore()
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
