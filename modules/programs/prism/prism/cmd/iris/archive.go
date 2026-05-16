// archive.go — `iris archive <session>` standalone archive subcommand (#1697).
//
// Standalone archive operation, separate from cleanup. Copies the pi JSONL
// session file into the iris archive tree at the documented path:
//
//	~/code/archives/iris/<session>/<instance_id>/raw/session.jsonl
//
// Differences from `iris cleanup`:
//
//   - The session keeps running. No session_kill, no DB end_state mutation,
//     no run dir / log file / worktree removal.
//   - No daemon dependency. The DB read + file copy work whether or not the
//     iris daemon is running. (Same carve-out as `iris checkin` — read-only
//     DB queries do not need to route through the daemon socket.)
//   - Idempotent. Running it twice on a still-running session re-copies the
//     latest JSONL snapshot to the same archive path.
//
// The archive copy itself reuses the same internal helper that
// CleanupSession invokes (internal/iris/archive.go::archiveSessionJSONL) so
// cleanup-archived and standalone-archived sessions produce identical
// output paths and content.

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

var (
	archiveInstanceID string
	archivePIAgentDir string
	archiveAllJSON    bool
)

var archiveCmd = &cobra.Command{
	Use:   "archive <session>",
	Short: "Archive an iris session's pi JSONL (session keeps running)",
	Long: `Copy the pi JSONL session file for the named iris session into the
iris archive tree, leaving the session running.

This is the standalone archive verb — it is intentionally narrower than
'iris cleanup', which also marks the session row ended and removes the
per-session run directory, log file, and (optionally) worktree. Use
'iris archive' when you want a snapshot of the session JSONL while the
session continues to run; use 'iris cleanup' when you want to tear the
session down.

Lookup:

  iris archive <session-name>            most-recent incarnation of <session-name>
  iris archive --instance-id <uuid>      explicit instance_id lookup

Output path (matches cleanup):

  ~/code/archives/iris/<session>/<instance_id>/raw/session.jsonl

When the session has no pi JSONL on disk yet (pi crashed before writing
or never started), the command exits 0 with an informative message and
does not create an empty archive directory.

This subcommand reads iris.db directly and copies a file. It does not
require the iris daemon to be running.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runArchive,
	// SilenceUsage so a user-facing failure (missing session, copy error)
	// does not dump the cobra usage block over the actual error message.
	SilenceUsage:  true,
	SilenceErrors: false,
}

func init() {
	archiveCmd.Flags().StringVar(&archiveInstanceID, "instance-id", "", "Look up the session by full instance_id (UUID) instead of session name")
	archiveCmd.Flags().StringVar(&archivePIAgentDir, "pi-agent-dir", "", "Override the pi agent dir (default: ~/.pi/agent/)")
	archiveCmd.Flags().BoolVar(&archiveAllJSON, "all-json", false, "Emit a JSON object with the full archive result instead of the human-readable summary")
	rootCmd.AddCommand(archiveCmd)
}

// archiveJSONOutput is the stable JSON shape emitted by `iris archive
// --all-json`. Field names use snake_case to match the rest of the iris
// JSON surface (sessions list, checkin --json). Adding fields is fine;
// renames and removals are breaking changes.
type archiveJSONOutput struct {
	SessionName      string  `json:"session_name"`
	InstanceID       string  `json:"instance_id"`
	HarnessSessionID *string `json:"harness_session_id,omitempty"`
	Worktree         string  `json:"worktree,omitempty"`
	// ArchivePath is the destination session.jsonl path on success. Null
	// when Skipped=true (no JSONL to copy).
	ArchivePath *string `json:"archive_path"`
	// Skipped is true when the source JSONL did not exist. The command
	// still exits 0 in this case — Skipped is the documented "empty JSONL"
	// outcome from the spec.
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// runArchive is the cobra RunE for `iris archive <session>`.
//
// Flow:
//
//  1. Validate flag/arg combinations. Either a positional session-name or
//     --instance-id must be supplied; passing both is a usage error so the
//     intent is unambiguous.
//  2. Open iris.db for read/write (the archive copy itself doesn't write
//     the DB, but iris.OpenDB returns a writable handle and the archive
//     adapter may want to write metadata in the future).
//  3. Resolve the sessions row via name or instance-id.
//  4. Call iris.ArchiveSessionRow to perform the copy.
//  5. Print a human-readable summary or JSON, per --all-json.
func runArchive(cmd *cobra.Command, args []string) error {
	switch {
	case archiveInstanceID != "" && len(args) > 0:
		return fmt.Errorf("iris archive: pass either <session> or --instance-id, not both")
	case archiveInstanceID == "" && len(args) == 0:
		return fmt.Errorf("iris archive: requires a <session> argument or --instance-id <uuid>")
	}

	p := iris.ResolvePaths()
	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris archive: open db: %w", err)
	}
	defer database.Close()

	cfg := iris.ArchiveConfig{
		Database:    database,
		ArchiveRoot: p.ArchiveRoot,
		PIAgentDir:  archivePIAgentDir,
	}

	ctx := context.Background()
	var (
		res  *iris.ArchiveResult
		aerr error
	)
	if archiveInstanceID != "" {
		res, aerr = iris.ArchiveSessionByInstanceID(ctx, cfg, archiveInstanceID)
	} else {
		res, aerr = iris.ArchiveSession(ctx, cfg, args[0])
	}
	if aerr != nil {
		return aerr
	}

	if archiveAllJSON {
		return emitArchiveJSON(cmd, res)
	}
	return emitArchiveHuman(cmd, res)
}

// emitArchiveHuman writes the human-readable summary form to cmd.OutOrStdout.
// Layout mirrors `iris cleanup`'s indented summary so users moving between
// the two commands see the same shape.
func emitArchiveHuman(cmd *cobra.Command, res *iris.ArchiveResult) error {
	w := cmd.OutOrStdout()
	sess := res.Session
	fmt.Fprintf(w, "iris archive: %s\n", sess.SessionName)
	fmt.Fprintf(w, "  instance:       %s\n", sess.InstanceID)
	if res.Skipped {
		fmt.Fprintf(w, "  archive:        (skipped — %s)\n", res.SkipReason)
		return nil
	}
	fmt.Fprintf(w, "  archive:        %s\n", res.ArchivePath)
	return nil
}

// emitArchiveJSON writes the --all-json form to cmd.OutOrStdout. Always
// emits a single JSON object (never an array) so consumers can json.Unmarshal
// without a wrapper type.
func emitArchiveJSON(cmd *cobra.Command, res *iris.ArchiveResult) error {
	out := archiveJSONOutput{
		SessionName:      res.Session.SessionName,
		InstanceID:       res.Session.InstanceID,
		HarnessSessionID: res.Session.HarnessSessionID,
		Worktree:         res.Session.Worktree,
		Skipped:          res.Skipped,
		SkipReason:       res.SkipReason,
	}
	if res.ArchivePath != "" {
		ap := res.ArchivePath
		out.ArchivePath = &ap
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("iris archive --all-json: marshal: %w", err)
	}
	_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return werr
}
