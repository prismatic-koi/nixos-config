package cmd

import (
	"fmt"
	"os"

	"github.com/prismatic-koi/atlassian/internal/adf"
	"github.com/prismatic-koi/atlassian/internal/client"
	"github.com/prismatic-koi/atlassian/internal/slim"
	"github.com/spf13/cobra"
)

var jiraCmd = &cobra.Command{
	Use:   "jira",
	Short: "Jira Cloud commands",
	Long:  "Commands for interacting with Jira Cloud (read-only).",
}

// --- jira search ---

var (
	jiraSearchLimit  int
	jiraSearchFields string
)

var jiraSearchCmd = &cobra.Command{
	Use:   "search '<JQL>'",
	Short: "Search Jira issues with JQL",
	Long: `Search Jira issues using JQL syntax. Returns a slim JSON wrapper with issues:[...].

Example:
  atlassian jira search 'project = FOO ORDER BY created DESC' --limit 20`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraSearch,
}

func runJiraSearch(cmd *cobra.Command, args []string) error {
	if jiraSearchLimit < 0 {
		fmt.Fprintln(os.Stderr, "--limit must be >= 0")
		os.Exit(1)
		return nil
	}

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.SearchJira(args[0], jiraSearchLimit, jiraSearchFields)
	if err != nil {
		dieErr(err)
		return nil
	}

	// Apply drop keys: universal + jira issue + search/list extras
	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.JiraIssueDropKeys, slim.SearchListDropKeys)

	// Slim the result wrapper; issues array is nested
	v = slim.Apply(v, dropKeys)

	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// --- jira get ---

var jiraGetFull bool

var jiraGetCmd = &cobra.Command{
	Use:   "get <KEY>",
	Short: "Get a Jira issue by key",
	Long: `Fetch a single Jira issue. By default returns slim JSON with ADF description
replaced by rendered Markdown under fields.description_markdown.

Use --full to get the raw untouched API response.

Example:
  atlassian jira get FOO-123
  atlassian jira get FOO-123 --full`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraGet,
}

func runJiraGet(cmd *cobra.Command, args []string) error {
	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.GetJiraIssue(args[0])
	if err != nil {
		dieErr(err)
		return nil
	}

	if jiraGetFull {
		if err := printJSON(v); err != nil {
			dieErr(err)
		}
		return nil
	}

	// Extract and render ADF description before slimming
	if m, ok := v.(map[string]any); ok {
		if fields, ok := m["fields"].(map[string]any); ok {
			if descADF, ok := fields["description"]; ok && descADF != nil {
				md := adf.Render(descADF)
				fields["description_markdown"] = md
				delete(fields, "description")
			}
		}
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.JiraIssueDropKeys)
	v = slim.Apply(v, dropKeys)

	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// --- jira transitions ---

var jiraTransitionsCmd = &cobra.Command{
	Use:   "transitions <KEY>",
	Short: "List available workflow transitions for a Jira issue",
	Long: `Lists the available workflow transitions for a Jira issue.
Returns slim JSON with id and name per transition.

Example:
  atlassian jira transitions FOO-123`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraTransitions,
}

func runJiraTransitions(cmd *cobra.Command, args []string) error {
	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.GetJiraTransitions(args[0])
	if err != nil {
		dieErr(err)
		return nil
	}

	// Extract just id and name from each transition
	result := slimTransitions(v)

	if err := printJSON(result); err != nil {
		dieErr(err)
	}
	return nil
}

// slimTransitions extracts id and name from the transitions array.
func slimTransitions(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	transitions, ok := m["transitions"].([]any)
	if !ok {
		return map[string]any{"transitions": []any{}}
	}
	out := make([]any, 0, len(transitions))
	for _, t := range transitions {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		entry := map[string]any{}
		if id, ok := tm["id"]; ok {
			entry["id"] = id
		}
		if name, ok := tm["name"]; ok {
			entry["name"] = name
		}
		out = append(out, entry)
	}
	return map[string]any{"transitions": out}
}

func init() {
	jiraCmd.AddCommand(jiraSearchCmd)
	jiraCmd.AddCommand(jiraGetCmd)
	jiraCmd.AddCommand(jiraTransitionsCmd)

	jiraSearchCmd.Flags().IntVar(&jiraSearchLimit, "limit", 50, "Maximum number of issues to return (0 = empty result)")
	jiraSearchCmd.Flags().StringVar(&jiraSearchFields, "fields", "", "Comma-separated list of fields to include")

	jiraGetCmd.Flags().BoolVar(&jiraGetFull, "full", false, "Return untouched API response without slimming")
}
