package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestPrCmd_OverBudgetFlag(t *testing.T) {
	if prCmd.Flags().Lookup("over-budget") == nil {
		t.Fatal("prism pr has no --over-budget flag")
	}
	c := &cobra.Command{Use: "pr"}
	c.Flags().String("over-budget", "", overBudgetHelp)
	if err := c.Flags().Set("over-budget", " "); err != nil {
		t.Fatal(err)
	}
	if _, err := overBudgetFlag(c); err == nil {
		t.Error("empty reason must be refused")
	}
}
