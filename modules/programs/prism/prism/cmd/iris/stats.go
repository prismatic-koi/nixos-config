// stats.go — `iris stats ...` subcommands (D-7).
//
// `iris stats credentials <session>` prints the credential audit log for a
// session: for each tool_call event in the session, the credentials that
// were injected into the subprocess. Names only, never values.
//
// The data source is the agent_events table populated by the iris harness
// socket (internal/iris/harness_socket.go). Each tool_call event payload
// contains a `credentials_injected` JSON array; this command reads it back.
//
// This is an operator-facing diagnostic — wired up here in the iris binary
// rather than as a prism subcommand so the iris credential model is
// self-contained.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Iris audit and diagnostic subcommands",
}

var statsCredentialsCmd = &cobra.Command{
	Use:   "credentials <session>",
	Short: "List credentials injected into each tool call of a session",
	Long: `Print, for each tool_call event recorded against the named session, the
credentials that were injected into the corresponding tool subprocess.

The output is one line per tool call:

    <created_at>  <tool>  <credentials_injected>

Names are stable identifiers — never values. "[]" means no credentials were
injected (the normal case for read/edit/write/grep/find/ls). See
docs/iris-credential-model.md for the full set of names the broker emits.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStatsCredentials(args[0])
	},
}

func init() {
	rootCmd.AddCommand(statsCmd)
	statsCmd.AddCommand(statsCredentialsCmd)
}

// runStatsCredentials prints the credential audit log for session.
func runStatsCredentials(session string) error {
	p := iris.ResolvePaths()
	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris: open db: %w", err)
	}
	defer database.Close()

	events, err := database.QueryEvents(session, 0, nil, nil, []string{"tool_call"})
	if err != nil {
		return fmt.Errorf("iris: query events: %w", err)
	}

	if len(events) == 0 {
		fmt.Printf("no tool_call events for session %q\n", session)
		return nil
	}

	for _, e := range events {
		var payload struct {
			Name                 string   `json:"name"`
			CredentialsInjected  []string `json:"credentials_injected"`
			AgentRole            string   `json:"agent_role"`
		}
		// We tolerate malformed/legacy payloads so a single bad event does
		// not block the whole audit log.
		_ = json.Unmarshal([]byte(e.Payload), &payload)

		creds := "[]"
		if len(payload.CredentialsInjected) > 0 {
			creds = "[" + strings.Join(payload.CredentialsInjected, ",") + "]"
		}

		role := payload.AgentRole
		if role == "" {
			role = "-"
		}

		fmt.Printf("%s  tool=%s  role=%s  credentials_injected=%s\n",
			e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			payload.Name,
			role,
			creds,
		)
	}
	return nil
}
