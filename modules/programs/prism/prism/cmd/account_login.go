package cmd

import (
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/account"
)

var accountLoginCmd = &cobra.Command{
	Use:   "login <name>",
	Short: "Authenticate with Claude OAuth and save a named account",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccountLogin,
}

func init() {
	accountCmd.AddCommand(accountLoginCmd)
	accountLoginCmd.Flags().Bool("use", false, "Activate the account after saving it")
	accountLoginCmd.Flags().Int("port", account.DefaultCallbackPort, "Local OAuth callback port (0 chooses a random free port)")
}

func runAccountLogin(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	p, err := account.ResolvePaths()
	if err != nil {
		return err
	}
	useAccount, err := cmd.Flags().GetBool("use")
	if err != nil {
		return err
	}
	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		return err
	}
	return account.Login(cmd.Context(), p, args[0], account.LoginOptions{
		Use:    useAccount,
		Port:   port,
		Stdin:  cmd.InOrStdin(),
		Stdout: cmd.OutOrStdout(),
	})
}
