package cmd

import (
	"github.com/prismatic-koi/atlassian/internal/client"
	"github.com/prismatic-koi/atlassian/internal/slim"
	"github.com/spf13/cobra"
)

var resourcesCmd = &cobra.Command{
	Use:   "resources",
	Short: "List accessible Atlassian cloud resources",
	Long:  "Lists accessible Atlassian cloud IDs as slim JSON. Drops the 'scopes' and 'url' fields from each entry.",
	Args:  cobra.NoArgs,
	RunE:  runResources,
}

func runResources(cmd *cobra.Command, args []string) error {
	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.GetResources()
	if err != nil {
		dieErr(err)
		return nil
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.ResourceDropKeys)
	v = slim.Apply(v, dropKeys)

	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}
