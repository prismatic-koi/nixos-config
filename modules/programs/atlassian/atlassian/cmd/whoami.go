package cmd

import (
	"github.com/prismatic-koi/atlassian/internal/client"
	"github.com/prismatic-koi/atlassian/internal/slim"
	"github.com/spf13/cobra"
)

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Print the current Atlassian user",
	Long:  "Fetches the current authenticated user from the Atlassian API and prints slim JSON.",
	Args:  cobra.NoArgs,
	RunE:  runWhoami,
}

func runWhoami(cmd *cobra.Command, args []string) error {
	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.GetMe()
	if err != nil {
		dieErr(err)
		return nil
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.UserInfoDropKeys)
	v = slim.Apply(v, dropKeys)

	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}
