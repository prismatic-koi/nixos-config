package cmd

// prism scrub-secrets — remediation for credential values captured into
// prism.db before the capture path redacted anything (issue #2589).
//
// Usage:
//
//	prism scrub-secrets --dry-run    report how many rows would change
//	prism scrub-secrets              rewrite them
//	prism scrub-secrets --json       machine-readable report
//
// The command reads the credential values out of its own environment, so run
// it from a shell that carries the same credentials the agents had. Values
// the environment does not hold are still covered by the shape layer.

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/payload"
)

var scrubSecretsCmd = &cobra.Command{
	Use:   "scrub-secrets",
	Short: "Remove captured credential values from stored events and harness frames",
	Long: `Rewrite stored payloads that carry a credential value.

Every frame an agent harness emits is stored in prism.db. Before issue #2589
nothing in that path removed a secret, so any credential a command printed to
stdout or stderr was stored verbatim and kept until the prune job ran. New
rows are now redacted twice — once by the harness before the frame reaches the
socket, once by the database layer before the INSERT. This command remediates
the rows that were written earlier.

Tables covered:

  agent_events.payload     every captured event, every type
  harness_frames.payload   the raw wire archive

On-disk session archives are NOT covered, and no other prism control covers
them either. Pi writes its own transcript JSONL and the prism extension is not
in that write path; "prism cleanup" only byte-copies the file into the
directory named by sessions.archive_path. Treat every archive as carrying
whatever the session printed: delete or rotate it separately, and rotate any
credential you believe reached it.

Matching has two layers. The value layer replaces the exact value of every
credential environment variable in this process's environment, so run the
command from a shell that carries the same credentials the agents had. The
shape layer replaces well-known credential shapes and always runs.

A redacted value becomes a marker naming what was removed, for example
[redacted:GITHUB_TOKEN], so the surrounding output stays diagnosable.

Re-running is safe: a marker matches no rule, so a second pass reports zero
rewrites.`,
	Args:         cobra.NoArgs,
	RunE:         runScrubSecrets,
	SilenceUsage: true,
}

func init() {
	scrubSecretsCmd.Flags().Bool("dry-run", false, "Report how many rows would change without writing anything")
	scrubSecretsCmd.Flags().Bool("json", false, "Emit the report as a single JSON object")
	rootCmd.AddCommand(scrubSecretsCmd)
}

// scrubSecretsJSON is the --json report shape. It carries counts only — never
// a payload, a row id, or a credential value.
type scrubSecretsJSON struct {
	DryRun                 bool `json:"dryRun"`
	CredentialValues       int  `json:"credentialValues"`
	EventsScanned          int  `json:"eventsScanned"`
	EventsRewritten        int  `json:"eventsRewritten"`
	HarnessFramesScanned   int  `json:"harnessFramesScanned"`
	HarnessFramesRewritten int  `json:"harnessFramesRewritten"`
}

func runScrubSecrets(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	jsonMode, _ := cmd.Flags().GetBool("json")

	fail := func(err error) error {
		if jsonMode {
			return emitJSONErrorEnvelope(err)
		}
		return err
	}

	redactor := payload.NewEnvRedactor()

	d, err := openDB()
	if err != nil {
		return fail(fmt.Errorf("scrub-secrets: %w", err))
	}
	defer d.Close()

	report, err := d.ScrubSecrets(redactor, dryRun)
	if err != nil {
		return fail(fmt.Errorf("scrub-secrets: %w", err))
	}

	out := cmd.OutOrStdout()

	if jsonMode {
		enc := json.NewEncoder(out)
		return enc.Encode(scrubSecretsJSON{
			DryRun:                 report.DryRun,
			CredentialValues:       redactor.ValueCount(),
			EventsScanned:          report.EventsScanned,
			EventsRewritten:        report.EventsRewritten,
			HarnessFramesScanned:   report.HarnessFramesScanned,
			HarnessFramesRewritten: report.HarnessFramesRewritten,
		})
	}

	verb := "rewrote"
	if dryRun {
		verb = "would rewrite"
	}
	fmt.Fprintf(out, "credential values known to the value layer: %d\n", redactor.ValueCount())
	fmt.Fprintf(out, "agent_events:   scanned %d, %s %d\n", report.EventsScanned, verb, report.EventsRewritten)
	fmt.Fprintf(out, "harness_frames: scanned %d, %s %d\n", report.HarnessFramesScanned, verb, report.HarnessFramesRewritten)

	if redactor.ValueCount() == 0 {
		fmt.Fprintln(os.Stderr,
			"warning: no credential environment variable is set in this shell — only the shape layer ran")
	}
	if dryRun && report.Changed() > 0 {
		fmt.Fprintln(out, "\nre-run without --dry-run to apply")
	}
	return nil
}
