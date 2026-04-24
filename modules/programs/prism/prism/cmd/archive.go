package cmd

// prism archive — resolve the archive directory for a session incarnation.
//
// Usage:
//
//	prism archive <instance-id>                  print archive_path and exit 0
//	prism archive <session-name>                 print archive_path for most recent incarnation
//	prism archive <session-name> --all           print one archive_path per line, newest first
//	prism archive --instance <full-uuid>         force UUID lookup (disambiguate from session name)

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var archiveCmd = &cobra.Command{
	Use:   "archive <instance-id|session-name>",
	Short: "Print the archive path for a session incarnation",
	Long: `Print sessions.archive_path for the specified incarnation and exit 0.

The argument may be a full 36-character UUID (instance_id), an unambiguous
UUID prefix, or a session name (resolves to the most recent incarnation).

Use --instance to force UUID lookup even when the argument could also match a
session name.

Use --all with a session name to print one archive_path per line for every
incarnation of that name, newest first.

Exits non-zero when the incarnation is unknown or archive_path IS NULL
(session not yet archived).`,
	Args: cobra.ExactArgs(1),
	RunE: runArchive,
}

func init() {
	archiveCmd.Flags().Bool("all", false, "Print archive_path for every incarnation of the session name, newest first")
	archiveCmd.Flags().String("instance", "", "Force UUID instance_id lookup for this full UUID (alternative to positional arg)")
	rootCmd.AddCommand(archiveCmd)
}

func runArchive(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	instanceFlag, _ := cmd.Flags().GetString("instance")

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer d.Close()

	// --instance flag takes precedence over the positional arg.
	if instanceFlag != "" {
		return printArchivePath(d, instanceFlag, true, false)
	}

	arg := args[0]

	// --all: iterate all incarnations of the session name.
	if all {
		return printArchivePathAll(d, arg)
	}

	// Disambiguate: full UUID or session name.
	forceInstance := len(arg) == 36
	return printArchivePath(d, arg, forceInstance, false)
}

// printArchivePath resolves arg to a single sessions row and prints its archive_path.
// If forceInstance is true, arg is treated as an instance_id (UUID lookup).
// Returns a non-zero exit code (via error) when archive_path IS NULL.
func printArchivePath(d *db.DB, arg string, forceInstance bool, silent bool) error {
	sess, err := resolveSessionArg(d, arg, forceInstance)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}

	if sess.ArchivePath == nil || *sess.ArchivePath == "" {
		return fmt.Errorf("archive: session not yet archived")
	}

	fmt.Println(*sess.ArchivePath)
	return nil
}

// printArchivePathAll prints one archive_path per line for every incarnation of
// session_name = arg, ordered by started_at DESC (newest first).
// Exits non-zero when no incarnations exist for that name.
func printArchivePathAll(d *db.DB, sessionName string) error {
	sessions, err := d.SessionsByName(sessionName)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}

	if len(sessions) == 0 {
		return fmt.Errorf("archive: no incarnations found for session %q", sessionName)
	}

	for _, s := range sessions {
		if s.ArchivePath != nil && *s.ArchivePath != "" {
			fmt.Println(*s.ArchivePath)
		} else {
			fmt.Println("(not yet archived)")
		}
	}
	return nil
}
