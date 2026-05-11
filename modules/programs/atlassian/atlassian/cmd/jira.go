package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/prismatic-koi/atlassian/internal/adf"
	"github.com/prismatic-koi/atlassian/internal/client"
	"github.com/prismatic-koi/atlassian/internal/slim"
	"github.com/spf13/cobra"
)

var jiraCmd = &cobra.Command{
	Use:   "jira",
	Short: "Jira Cloud commands",
	Long:  "Commands for interacting with Jira Cloud.",
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

// --- jira create ---

var (
	jiraCreateProject     string
	jiraCreateType        string
	jiraCreateSummary     string
	jiraCreateDescFile    string
	jiraCreateLabels      string
	jiraCreateAssignee    string
	jiraCreateADF         bool
)

var jiraCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a Jira issue",
	Long: `Create a new Jira issue. Prints the slim JSON of the created issue (including
its key) to stdout on success.

Examples:
  atlassian jira create --project=FOO --type=Task --summary="Fix the bug"
  printf '## Steps\n\n1. reproduce' | atlassian jira create --project=FOO --type=Bug --summary="Title" --description-file=-`,
	RunE: runJiraCreate,
}

func runJiraCreate(cmd *cobra.Command, args []string) error {
	if jiraCreateProject == "" {
		fmt.Fprintln(os.Stderr, "--project is required")
		os.Exit(1)
	}
	if jiraCreateType == "" {
		fmt.Fprintln(os.Stderr, "--type is required")
		os.Exit(1)
	}
	if jiraCreateSummary == "" {
		fmt.Fprintln(os.Stderr, "--summary is required")
		os.Exit(1)
	}
	if jiraCreateADF && jiraCreateDescFile != "" {
		fmt.Fprintln(os.Stderr, "--adf and --description-file are mutually exclusive")
		os.Exit(1)
	}

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	fields := map[string]any{
		"project":   map[string]any{"key": jiraCreateProject},
		"issuetype": map[string]any{"name": jiraCreateType},
		"summary":   jiraCreateSummary,
	}

	if jiraCreateLabels != "" {
		labels := strings.Split(jiraCreateLabels, ",")
		for i, l := range labels {
			labels[i] = strings.TrimSpace(l)
		}
		fields["labels"] = labels
	}
	if jiraCreateAssignee != "" {
		fields["assignee"] = map[string]any{"accountId": jiraCreateAssignee}
	}

	if jiraCreateADF {
		docBody, readErr := readBodyRaw("stdin")
		if readErr != nil {
			dieErr(readErr)
			return nil
		}
		var adfDoc any
		if jsonErr := json.Unmarshal(docBody, &adfDoc); jsonErr != nil {
			fmt.Fprintln(os.Stderr, "--adf: invalid JSON: "+jsonErr.Error())
			os.Exit(1)
		}
		fields["description"] = adfDoc
	} else if jiraCreateDescFile != "" {
		docBody, readErr := readBodyFile(jiraCreateDescFile)
		if readErr != nil {
			dieErr(readErr)
			return nil
		}
		adfDoc, buildErr := adf.Build(docBody)
		if buildErr != nil {
			fmt.Fprintln(os.Stderr, buildErr.Error())
			os.Exit(1)
		}
		fields["description"] = adfDoc
	}

	payload := map[string]any{"fields": fields}
	v, err := c.CreateJiraIssue(payload)
	if err != nil {
		dieErr(err)
		return nil
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.JiraIssueDropKeys)
	v = slim.Apply(v, dropKeys)
	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// --- jira update ---

var (
	jiraUpdateSummary  string
	jiraUpdateDescFile string
	jiraUpdateLabels   string
	jiraUpdateAssignee string
	jiraUpdateADF      bool
)

var jiraUpdateCmd = &cobra.Command{
	Use:   "update <KEY>",
	Short: "Update a Jira issue (partial update)",
	Long: `Partially update an existing Jira issue. Only fields explicitly passed are
sent — omitting a flag leaves that field unchanged in Jira.

Rejects the call if no field flags are provided (no-op write prevention).

Examples:
  atlassian jira update FOO-123 --summary="New title"
  printf 'Updated description' | atlassian jira update FOO-123 --description-file=-`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraUpdate,
}

func runJiraUpdate(cmd *cobra.Command, args []string) error {
	if jiraUpdateADF && jiraUpdateDescFile != "" {
		fmt.Fprintln(os.Stderr, "--adf and --description-file are mutually exclusive")
		os.Exit(1)
	}

	fields := map[string]any{}

	if jiraUpdateSummary != "" {
		fields["summary"] = jiraUpdateSummary
	}
	if jiraUpdateLabels != "" {
		labels := strings.Split(jiraUpdateLabels, ",")
		for i, l := range labels {
			labels[i] = strings.TrimSpace(l)
		}
		fields["labels"] = labels
	}
	if jiraUpdateAssignee != "" {
		fields["assignee"] = map[string]any{"accountId": jiraUpdateAssignee}
	}

	if jiraUpdateADF {
		docBody, readErr := readBodyRaw("stdin")
		if readErr != nil {
			dieErr(readErr)
			return nil
		}
		var adfDoc any
		if jsonErr := json.Unmarshal(docBody, &adfDoc); jsonErr != nil {
			fmt.Fprintln(os.Stderr, "--adf: invalid JSON: "+jsonErr.Error())
			os.Exit(1)
		}
		fields["description"] = adfDoc
	} else if jiraUpdateDescFile != "" {
		docBody, readErr := readBodyFile(jiraUpdateDescFile)
		if readErr != nil {
			dieErr(readErr)
			return nil
		}
		adfDoc, buildErr := adf.Build(docBody)
		if buildErr != nil {
			fmt.Fprintln(os.Stderr, buildErr.Error())
			os.Exit(1)
		}
		fields["description"] = adfDoc
	}

	if len(fields) == 0 {
		fmt.Fprintln(os.Stderr, "no fields to update: provide at least one of --summary, --description-file, --adf, --labels, --assignee")
		os.Exit(1)
	}

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	payload := map[string]any{"fields": fields}
	v, err := c.UpdateJiraIssue(args[0], payload)
	if err != nil {
		dieErr(err)
		return nil
	}

	// Jira PUT /issue/{key} returns 204 No Content on success.
	// Fetch the updated issue to return its slim JSON.
	updated, err := c.GetJiraIssue(args[0])
	if err != nil {
		dieErr(err)
		return nil
	}
	_ = v

	// Render description as markdown
	if m, ok := updated.(map[string]any); ok {
		if fields, ok := m["fields"].(map[string]any); ok {
			if descADF, ok := fields["description"]; ok && descADF != nil {
				md := adf.Render(descADF)
				fields["description_markdown"] = md
				delete(fields, "description")
			}
		}
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.JiraIssueDropKeys)
	updated = slim.Apply(updated, dropKeys)
	if err := printJSON(updated); err != nil {
		dieErr(err)
	}
	return nil
}

// --- jira comment ---

var (
	jiraCommentBodyFile string
	jiraCommentADF      bool
)

var jiraCommentCmd = &cobra.Command{
	Use:   "comment <KEY>",
	Short: "Add a comment to a Jira issue",
	Long: `Add a comment to a Jira issue. The body is read from --body-file (use - for stdin).
The markdown body is converted to ADF before posting.

Prints the slim JSON of the created comment to stdout.

Examples:
  printf 'This is a comment' | atlassian jira comment FOO-123 --body-file=-
  atlassian jira comment FOO-123 --body-file=comment.md`,
	Args: cobra.ExactArgs(1),
	RunE: runJiraComment,
}

func runJiraComment(cmd *cobra.Command, args []string) error {
	if jiraCommentADF && jiraCommentBodyFile != "" {
		fmt.Fprintln(os.Stderr, "--adf and --body-file are mutually exclusive")
		os.Exit(1)
	}
	if !jiraCommentADF && jiraCommentBodyFile == "" {
		fmt.Fprintln(os.Stderr, "--body-file is required (use - to read from stdin)")
		os.Exit(1)
	}

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	var commentBody any
	if jiraCommentADF {
		docBody, readErr := readBodyRaw("stdin")
		if readErr != nil {
			dieErr(readErr)
			return nil
		}
		if jsonErr := json.Unmarshal(docBody, &commentBody); jsonErr != nil {
			fmt.Fprintln(os.Stderr, "--adf: invalid JSON: "+jsonErr.Error())
			os.Exit(1)
		}
	} else {
		docBody, readErr := readBodyFile(jiraCommentBodyFile)
		if readErr != nil {
			dieErr(readErr)
			return nil
		}
		adfDoc, buildErr := adf.Build(docBody)
		if buildErr != nil {
			fmt.Fprintln(os.Stderr, buildErr.Error())
			os.Exit(1)
		}
		commentBody = adfDoc
	}

	payload := map[string]any{"body": commentBody}
	v, err := c.AddJiraComment(args[0], payload)
	if err != nil {
		dieErr(err)
		return nil
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.JiraIssueDropKeys)
	v = slim.Apply(v, dropKeys)
	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// --- jira transition ---

var jiraTransitionCmd = &cobra.Command{
	Use:   "transition <KEY> <name|id>",
	Short: "Transition a Jira issue to a new workflow state",
	Long: `Transition a Jira issue to a new workflow state.

The second argument may be either a transition name (e.g. "In Progress") or a
numeric ID. If a name is given, it is resolved to an ID via the transitions API.
If the name is not found, an error listing available transitions is printed.

Examples:
  atlassian jira transition FOO-123 "In Progress"
  atlassian jira transition FOO-123 21`,
	Args: cobra.ExactArgs(2),
	RunE: runJiraTransition,
}

func runJiraTransition(cmd *cobra.Command, args []string) error {
	key := args[0]
	nameOrID := args[1]

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	// Fetch available transitions.
	transitionsRaw, err := c.GetJiraTransitions(key)
	if err != nil {
		dieErr(err)
		return nil
	}

	transitions := extractTransitionList(transitionsRaw)

	// Resolve name or ID.
	transitionID := resolveTransition(nameOrID, transitions)
	if transitionID == "" {
		fmt.Fprintf(os.Stderr, "unknown transition %q for %s\nAvailable transitions:\n", nameOrID, key)
		for _, t := range transitions {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(os.Stderr, "  id=%v name=%v\n", tm["id"], tm["name"])
		}
		os.Exit(1)
	}

	_, err = c.TransitionJiraIssue(key, transitionID)
	if err != nil {
		dieErr(err)
		return nil
	}

	// Fetch and return the updated issue.
	updated, err := c.GetJiraIssue(key)
	if err != nil {
		dieErr(err)
		return nil
	}

	if m, ok := updated.(map[string]any); ok {
		if fields, ok := m["fields"].(map[string]any); ok {
			if descADF, ok := fields["description"]; ok && descADF != nil {
				fields["description_markdown"] = adf.Render(descADF)
				delete(fields, "description")
			}
		}
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.JiraIssueDropKeys)
	updated = slim.Apply(updated, dropKeys)
	if err := printJSON(updated); err != nil {
		dieErr(err)
	}
	return nil
}

// extractTransitionList extracts the transitions slice from the raw API response.
func extractTransitionList(v any) []any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	list, _ := m["transitions"].([]any)
	return list
}

// resolveTransition resolves a name-or-ID string to a transition ID.
// Returns empty string if not found.
func resolveTransition(nameOrID string, transitions []any) string {
	for _, t := range transitions {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		id, _ := tm["id"].(string)
		name, _ := tm["name"].(string)
		if id == nameOrID || strings.EqualFold(name, nameOrID) {
			return id
		}
	}
	return ""
}

func init() {
	jiraCmd.AddCommand(jiraSearchCmd)
	jiraCmd.AddCommand(jiraGetCmd)
	jiraCmd.AddCommand(jiraTransitionsCmd)
	jiraCmd.AddCommand(jiraCreateCmd)
	jiraCmd.AddCommand(jiraUpdateCmd)
	jiraCmd.AddCommand(jiraCommentCmd)
	jiraCmd.AddCommand(jiraTransitionCmd)

	jiraSearchCmd.Flags().IntVar(&jiraSearchLimit, "limit", 50, "Maximum number of issues to return (0 = empty result)")
	jiraSearchCmd.Flags().StringVar(&jiraSearchFields, "fields", "", "Comma-separated list of fields to include")

	jiraGetCmd.Flags().BoolVar(&jiraGetFull, "full", false, "Return untouched API response without slimming")

	jiraCreateCmd.Flags().StringVar(&jiraCreateProject, "project", "", "Jira project key (required)")
	jiraCreateCmd.Flags().StringVar(&jiraCreateType, "type", "", "Issue type name, e.g. Task, Bug, Story (required)")
	jiraCreateCmd.Flags().StringVar(&jiraCreateSummary, "summary", "", "Issue summary/title (required)")
	jiraCreateCmd.Flags().StringVar(&jiraCreateDescFile, "description-file", "", "Path to markdown description file (- for stdin)")
	jiraCreateCmd.Flags().StringVar(&jiraCreateLabels, "labels", "", "Comma-separated labels")
	jiraCreateCmd.Flags().StringVar(&jiraCreateAssignee, "assignee", "", "Assignee accountId")
	jiraCreateCmd.Flags().BoolVar(&jiraCreateADF, "adf", false, "Read raw ADF JSON from stdin instead of markdown")

	jiraUpdateCmd.Flags().StringVar(&jiraUpdateSummary, "summary", "", "New issue summary/title")
	jiraUpdateCmd.Flags().StringVar(&jiraUpdateDescFile, "description-file", "", "Path to markdown description file (- for stdin)")
	jiraUpdateCmd.Flags().StringVar(&jiraUpdateLabels, "labels", "", "Comma-separated labels")
	jiraUpdateCmd.Flags().StringVar(&jiraUpdateAssignee, "assignee", "", "Assignee accountId")
	jiraUpdateCmd.Flags().BoolVar(&jiraUpdateADF, "adf", false, "Read raw ADF JSON from stdin instead of markdown")

	jiraCommentCmd.Flags().StringVar(&jiraCommentBodyFile, "body-file", "", "Path to markdown body file (- for stdin)")
	jiraCommentCmd.Flags().BoolVar(&jiraCommentADF, "adf", false, "Read raw ADF JSON from stdin instead of markdown")
}
