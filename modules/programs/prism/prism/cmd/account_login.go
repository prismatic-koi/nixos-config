package cmd

// `prism account login <name>` — native PKCE OAuth flow that registers
// a fresh Anthropic subscription as a named account under
// ~/.config/prism/accounts/<name>.json (#2284). When `--use` is set,
// the new account is activated immediately by calling account.Use.
//
// The OAuth protocol details — PKCE generation, callback server,
// token exchange — live in internal/account/login.go. This file is
// only the cobra wiring: argument validation, flag plumbing, and
// translating LoginOptions defaults from the CLI surface.

import (
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/account"
)

var accountLoginCmd = &cobra.Command{
	Use:   "login <name>",
	Short: "Log in to Anthropic via OAuth and save as a named account",
	Long: `Run the Anthropic OAuth + PKCE flow and write the resulting tokens to
` + "`" + `~/.config/prism/accounts/<name>.json` + "`" + ` (mode 0o600).

By default the new account is registered but NOT activated — run
` + "`" + `prism account use <name>` + "`" + ` when ready. Pass --use to activate immediately.

In a graphical session the system browser is opened to the authorize URL.
In a headless SSH session (DISPLAY and WAYLAND_DISPLAY unset, SSH_CONNECTION
set) the URL is printed to stdout and the flow waits for the user to paste
the callback URL or code#state value back into the terminal.`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountLogin,
}

func init() {
	accountLoginCmd.Flags().Bool("use", false, "Activate the account immediately after login (calls `prism account use`)")
	accountLoginCmd.Flags().Int("port", 53692, "Local callback port; use 0 for a random free port")
	accountCmd.AddCommand(accountLoginCmd)
}

func runAccountLogin(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	name := args[0]

	useFlag, _ := cmd.Flags().GetBool("use")
	portFlag, _ := cmd.Flags().GetInt("port")

	p, err := account.ResolvePaths()
	if err != nil {
		return err
	}

	opts := account.LoginOptions{
		Use:           useFlag,
		Stdout:        cmd.OutOrStdout(),
		UseRandomPort: portFlag == 0,
	}
	if !opts.UseRandomPort {
		opts.Port = portFlag
	}

	return account.Login(p, name, opts)
}
