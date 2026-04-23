package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:          "prism",
	Short:        "Prism — tmux-based AI development environment",
	SilenceUsage: true,
}

func Execute() error {
	return rootCmd.Execute()
}
