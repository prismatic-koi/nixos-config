package cmd

// prism account — manage named Claude OAuth subscriptions (#2283).
//
// Each subcommand calls account.Init first so the on-disk store is
// guaranteed to exist (and the first-run migration is applied) before
// any read or write. Shape mirrors `prism profile`:
//
//   prism account list                  Print account names, one per line,
//                                       with the active account marked.
//                                       --json emits a structured array.
//
//   prism account current               Print the active account name. If
//                                       the pointer is absent, prints "none".
//
//   prism account save <name>           Snapshot the live auth.json's
//                                       anthropic blob to accounts/<name>.json
//                                       at mode 0o600.
//
//   prism account login <name>          Run Anthropic OAuth PKCE and save the
//                                       resulting blob to accounts/<name>.json.
//
//   prism account use <name>            Atomically swap the active anthropic
//                                       blob (see internal/account.Use for
//                                       the step-by-step contract).
//
//   prism account rm <name>             Delete accounts/<name>.json. Refuses
//                                       to delete the currently-active
//                                       account.
//
// None of the subcommands print any token value at any verbosity.

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/account"
)

var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Manage named Claude OAuth subscriptions",
	Long: `Manage named Claude OAuth subscriptions.

Each account is stored as ~/.config/prism/accounts/<name>.json — the
"anthropic" key of pi's auth.json. ` + "`" + `prism account use <name>` + "`" + ` swaps
the live auth.json's anthropic blob atomically; pi's credential cache
picks up the new tokens on its next outbound request.`,
}

var accountListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured accounts",
	Args:  cobra.NoArgs,
	RunE:  runAccountList,
}

var accountCurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Print the active account name",
	Args:  cobra.NoArgs,
	RunE:  runAccountCurrent,
}

var accountSaveCmd = &cobra.Command{
	Use:   "save <name>",
	Short: "Snapshot the live auth.json's anthropic blob to accounts/<name>.json",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccountSave,
}

var accountUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Atomically swap the active anthropic blob to accounts/<name>.json",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccountUse,
}

var accountRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Delete accounts/<name>.json (refuses the active account)",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccountRm,
}

func init() {
	accountCmd.AddCommand(accountListCmd)
	accountCmd.AddCommand(accountCurrentCmd)
	accountCmd.AddCommand(accountSaveCmd)
	accountCmd.AddCommand(accountUseCmd)
	accountCmd.AddCommand(accountRmCmd)
	rootCmd.AddCommand(accountCmd)

	accountListCmd.Flags().Bool("json", false, "Emit a JSON array of account objects instead of the human-readable list")
}

// resolveAndInit centralises the "resolve paths then ensure the on-disk
// store is initialised" preamble. Every subcommand calls this first.
func resolveAndInit() (account.Paths, error) {
	p, err := account.ResolvePaths()
	if err != nil {
		return account.Paths{}, err
	}
	if err := account.Init(p); err != nil {
		return account.Paths{}, err
	}
	return p, nil
}

// accountJSON is the snake_case shape for --json output.
type accountJSON struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

func runAccountList(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	p, err := resolveAndInit()
	if err != nil {
		return err
	}
	names, err := account.List(p)
	if err != nil {
		return err
	}
	active, _, err := account.Current(p)
	if err != nil {
		// A corrupt `current` pointer should not block listing. Surface as
		// a warning so the user can recover via `prism account use`.
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		active = ""
	}

	jsonMode, _ := cmd.Flags().GetBool("json")
	if jsonMode {
		rows := make([]accountJSON, 0, len(names))
		for _, n := range names {
			rows = append(rows, accountJSON{Name: n, Active: n == active && active != ""})
		}
		data, mErr := json.Marshal(rows)
		if mErr != nil {
			return fmt.Errorf("account list --json: marshal: %w", mErr)
		}
		return printJSON(data)
	}

	w := cmd.OutOrStdout()
	for _, n := range names {
		marker := "  "
		if n == active && active != "" {
			marker = "* "
		}
		fmt.Fprintf(w, "%s%s\n", marker, n)
	}
	return nil
}

func runAccountCurrent(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	p, err := resolveAndInit()
	if err != nil {
		return err
	}
	cur, ok, err := account.Current(p)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if !ok {
		fmt.Fprintln(w, "none")
		return nil
	}
	fmt.Fprintln(w, cur)
	return nil
}

func runAccountSave(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	p, err := resolveAndInit()
	if err != nil {
		return err
	}
	if err := account.Save(p, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "saved account %q\n", args[0])
	return nil
}

func runAccountUse(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	p, err := resolveAndInit()
	if err != nil {
		return err
	}
	if err := account.Use(p, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "switched to account %q\n", args[0])
	return nil
}

func runAccountRm(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	p, err := resolveAndInit()
	if err != nil {
		return err
	}
	if err := account.Remove(p, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed account %q\n", args[0])
	return nil
}
