package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/prismatic-koi/atlassian/internal/adf"
	"github.com/prismatic-koi/atlassian/internal/client"
	"github.com/prismatic-koi/atlassian/internal/slim"
	"github.com/spf13/cobra"
)

var confluenceCmd = &cobra.Command{
	Use:   "confluence",
	Short: "Confluence Cloud commands",
	Long:  "Commands for interacting with Confluence Cloud.",
}

// --- confluence search ---

var confluenceSearchLimit int

var confluenceSearchCmd = &cobra.Command{
	Use:   "search '<CQL>'",
	Short: "Search Confluence pages with CQL",
	Long: `Search Confluence pages using CQL syntax. Returns a slim JSON list of page results.

Example:
  atlassian confluence search 'space = ENG AND title ~ "design"' --limit 10`,
	Args: cobra.ExactArgs(1),
	RunE: runConfluenceSearch,
}

func runConfluenceSearch(cmd *cobra.Command, args []string) error {
	if confluenceSearchLimit < 0 {
		fmt.Fprintln(os.Stderr, "--limit must be >= 0")
		os.Exit(1)
		return nil
	}

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.SearchConfluence(args[0], confluenceSearchLimit)
	if err != nil {
		dieErr(err)
		return nil
	}

	// Slim each result: universal + confluence + search/list drops
	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.ConfluenceDropKeys, slim.SearchListDropKeys)
	v = slim.Apply(v, dropKeys)

	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// --- confluence get ---

var confluenceGetFull bool

var confluenceGetCmd = &cobra.Command{
	Use:   "get <pageId>",
	Short: "Get a Confluence page by ID",
	Long: `Fetch a single Confluence page. By default returns slim JSON with the page body
replaced by rendered Markdown under body_markdown.

Use --full to get the raw untouched API response.

Example:
  atlassian confluence get 123456789
  atlassian confluence get 123456789 --full`,
	Args: cobra.ExactArgs(1),
	RunE: runConfluenceGet,
}

func runConfluenceGet(cmd *cobra.Command, args []string) error {
	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.GetConfluencePage(args[0])
	if err != nil {
		dieErr(err)
		return nil
	}

	if confluenceGetFull {
		if err := printJSON(v); err != nil {
			dieErr(err)
		}
		return nil
	}

	// Extract and render ADF body before slimming.
	// Confluence v2 with atlas_doc_format returns:
	//   body: { atlas_doc_format: { value: "<JSON string>" } }
	if m, ok := v.(map[string]any); ok {
		var bodyADF any
		if body, ok := m["body"].(map[string]any); ok {
			if adfObj, ok := body["atlas_doc_format"].(map[string]any); ok {
				if valStr, ok := adfObj["value"].(string); ok {
					parsedADF, parseErr := parseADFJSON(valStr)
					if parseErr == nil {
						bodyADF = parsedADF
					}
				}
			}
		}
		if bodyADF != nil {
			m["body_markdown"] = adf.Render(bodyADF)
		}
		delete(m, "body")
	}

	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.ConfluenceDropKeys)
	v = slim.Apply(v, dropKeys)

	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// parseADFJSON decodes a JSON string into an any value.
func parseADFJSON(s string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// --- confluence create ---

var (
	confluenceCreateSpace    string
	confluenceCreateTitle    string
	confluenceCreateParent   string
	confluenceCreateBodyFile string
	confluenceCreateADF      bool
	confluenceCreateStorage  bool
)

var confluenceCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a Confluence page",
	Long: `Create a new Confluence page. Prints the slim JSON of the created page
(including its ID and version) to stdout on success.

Examples:
  atlassian confluence create --space=ENG --title="My Page" --body-file=page.md
  printf '# Hello' | atlassian confluence create --space=ENG --title="Hello" --body-file=-
  atlassian confluence create --space=ENG --title="Child" --parent=123456 --body-file=page.md`,
	RunE: runConfluenceCreate,
}

func runConfluenceCreate(cmd *cobra.Command, args []string) error {
	if confluenceCreateSpace == "" {
		fmt.Fprintln(os.Stderr, "--space is required")
		os.Exit(1)
	}
	if confluenceCreateTitle == "" {
		fmt.Fprintln(os.Stderr, "--title is required")
		os.Exit(1)
	}

	// Mutual exclusion checks.
	flags := countTrue(confluenceCreateBodyFile != "", confluenceCreateADF, confluenceCreateStorage)
	if flags > 1 {
		fmt.Fprintln(os.Stderr, "--body-file, --adf, and --storage are mutually exclusive")
		os.Exit(1)
	}
	if flags == 0 {
		fmt.Fprintln(os.Stderr, "one of --body-file, --adf, or --storage is required")
		os.Exit(1)
	}

	body, format, err := readConfluenceBody(
		confluenceCreateBodyFile,
		confluenceCreateADF,
		confluenceCreateStorage,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	payload := map[string]any{
		"spaceId": confluenceCreateSpace,
		"title":   confluenceCreateTitle,
		"body":    body,
	}
	if confluenceCreateParent != "" {
		payload["parentId"] = confluenceCreateParent
	}
	_ = format

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	v, err := c.CreateConfluencePage(payload)
	if err != nil {
		dieErr(err)
		return nil
	}

	v = slimConfluencePage(v)
	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// --- confluence update ---

var (
	confluenceUpdateTitle    string
	confluenceUpdateBodyFile string
	confluenceUpdateADF      bool
	confluenceUpdateStorage  bool
)

var confluenceUpdateCmd = &cobra.Command{
	Use:   "update <pageId>",
	Short: "Update an existing Confluence page",
	Long: `Update an existing Confluence page. The current version is fetched
automatically — you do not need to pass a version number.

If --body-file is omitted but --title is provided, the existing body is fetched
and re-sent with the new title (body is never silently emptied).

Examples:
  printf '# Updated content' | atlassian confluence update 123456 --body-file=-
  atlassian confluence update 123456 --title="New Title"
  atlassian confluence update 123456 --title="New Title" --body-file=page.md`,
	Args: cobra.ExactArgs(1),
	RunE: runConfluenceUpdate,
}

func runConfluenceUpdate(cmd *cobra.Command, args []string) error {
	pageID := args[0]

	// Mutual exclusion checks.
	flags := countTrue(confluenceUpdateBodyFile != "", confluenceUpdateADF, confluenceUpdateStorage)
	if flags > 1 {
		fmt.Fprintln(os.Stderr, "--body-file, --adf, and --storage are mutually exclusive")
		os.Exit(1)
	}

	if confluenceUpdateTitle == "" && flags == 0 {
		fmt.Fprintln(os.Stderr, "no fields to update: provide at least one of --title, --body-file, --adf, --storage")
		os.Exit(1)
	}

	c, err := client.New()
	if err != nil {
		dieErr(err)
		return nil
	}

	// Fetch current page to get version and (if needed) existing body.
	current, err := c.GetConfluencePage(pageID)
	if err != nil {
		dieErr(err)
		return nil
	}

	currentMap, ok := current.(map[string]any)
	if !ok {
		fmt.Fprintln(os.Stderr, "unexpected response shape from Confluence")
		os.Exit(1)
	}

	// Extract current version number.
	nextVersion, verErr := nextConfluenceVersion(currentMap)
	if verErr != nil {
		fmt.Fprintln(os.Stderr, verErr.Error())
		os.Exit(1)
	}

	// Determine title.
	title := confluenceUpdateTitle
	if title == "" {
		title, _ = currentMap["title"].(string)
	}

	// Determine body.
	var bodyPayload any
	if flags > 0 {
		// Caller is providing a new body.
		bodyPayload, _, err = readConfluenceBody(
			confluenceUpdateBodyFile,
			confluenceUpdateADF,
			confluenceUpdateStorage,
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	} else {
		// Re-use existing body from the fetched page.
		// Confluence v2 with atlas_doc_format body.
		if body, ok := currentMap["body"].(map[string]any); ok {
			if adfObj, ok := body["atlas_doc_format"].(map[string]any); ok {
				if valStr, ok := adfObj["value"].(string); ok {
					var parsedADF any
					if jsonErr := json.Unmarshal([]byte(valStr), &parsedADF); jsonErr == nil {
						bodyPayload = map[string]any{
							"representation": "atlas_doc_format",
							"value":          valStr,
						}
					}
				}
			}
		}
		if bodyPayload == nil {
			// Fallback: empty paragraph.
			emptyDoc, _ := json.Marshal(map[string]any{
				"version": 1,
				"type":    "doc",
				"content": []any{},
			})
			bodyPayload = map[string]any{
				"representation": "atlas_doc_format",
				"value":          string(emptyDoc),
			}
		}
	}

	payload := map[string]any{
		"id":      pageID,
		"title":   title,
		"version": map[string]any{"number": nextVersion},
		"body":    bodyPayload,
	}

	v, err := c.UpdateConfluencePage(pageID, payload)
	if err != nil {
		// Special message for concurrent modification.
		if apiErr, ok := err.(*client.APIError); ok && apiErr.StatusCode == 409 {
			fmt.Fprintln(os.Stderr, "page was modified concurrently — refetch and retry")
			os.Exit(1)
		}
		dieErr(err)
		return nil
	}

	v = slimConfluencePage(v)
	if err := printJSON(v); err != nil {
		dieErr(err)
	}
	return nil
}

// readConfluenceBody reads a Confluence body from the appropriate source and
// returns the ADF body payload and format string.
func readConfluenceBody(bodyFile string, isADF, isStorage bool) (any, string, error) {
	if isADF {
		raw, err := readBodyRaw("stdin")
		if err != nil {
			return nil, "", err
		}
		var adfDoc any
		if jsonErr := json.Unmarshal(raw, &adfDoc); jsonErr != nil {
			return nil, "", fmt.Errorf("--adf: invalid JSON: %w", jsonErr)
		}
		adfStr := string(raw)
		return map[string]any{
			"representation": "atlas_doc_format",
			"value":          adfStr,
		}, "atlas_doc_format", nil
	}

	if isStorage {
		raw, err := readBodyRaw("stdin")
		if err != nil {
			return nil, "", err
		}
		return map[string]any{
			"representation": "storage",
			"value":          string(raw),
		}, "storage", nil
	}

	// Markdown → ADF
	mdBytes, err := readBodyFile(bodyFile)
	if err != nil {
		return nil, "", err
	}
	adfDoc, err := adf.Build(mdBytes)
	if err != nil {
		return nil, "", err
	}
	adfJSON, err := json.Marshal(adfDoc)
	if err != nil {
		return nil, "", fmt.Errorf("marshal ADF: %w", err)
	}
	return map[string]any{
		"representation": "atlas_doc_format",
		"value":          string(adfJSON),
	}, "atlas_doc_format", nil
}

// nextConfluenceVersion extracts the current version number and returns number+1.
func nextConfluenceVersion(m map[string]any) (int, error) {
	ver, ok := m["version"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("could not extract version from Confluence page response")
	}
	number, ok := ver["number"]
	if !ok {
		return 0, fmt.Errorf("version.number missing from Confluence page response")
	}
	switch n := number.(type) {
	case float64:
		return int(n) + 1, nil
	case int:
		return n + 1, nil
	case int64:
		return int(n) + 1, nil
	default:
		return 0, fmt.Errorf("version.number has unexpected type %T", number)
	}
}

// slimConfluencePage applies slim drop keys to a Confluence page response.
func slimConfluencePage(v any) any {
	dropKeys := slim.MergeKeys(slim.UniversalDropKeys, slim.ConfluenceDropKeys)
	return slim.Apply(v, dropKeys)
}

// countTrue counts the number of bool arguments that are true.
func countTrue(vals ...bool) int {
	n := 0
	for _, v := range vals {
		if v {
			n++
		}
	}
	return n
}

func init() {
	confluenceCmd.AddCommand(confluenceSearchCmd)
	confluenceCmd.AddCommand(confluenceGetCmd)
	confluenceCmd.AddCommand(confluenceCreateCmd)
	confluenceCmd.AddCommand(confluenceUpdateCmd)

	confluenceSearchCmd.Flags().IntVar(&confluenceSearchLimit, "limit", 25, "Maximum number of results to return (0 = empty result)")
	confluenceGetCmd.Flags().BoolVar(&confluenceGetFull, "full", false, "Return untouched API response without slimming")

	confluenceCreateCmd.Flags().StringVar(&confluenceCreateSpace, "space", "", "Confluence space key (required)")
	confluenceCreateCmd.Flags().StringVar(&confluenceCreateTitle, "title", "", "Page title (required)")
	confluenceCreateCmd.Flags().StringVar(&confluenceCreateParent, "parent", "", "Parent page ID")
	confluenceCreateCmd.Flags().StringVar(&confluenceCreateBodyFile, "body-file", "", "Path to markdown body file (- for stdin)")
	confluenceCreateCmd.Flags().BoolVar(&confluenceCreateADF, "adf", false, "Read raw ADF JSON from stdin")
	confluenceCreateCmd.Flags().BoolVar(&confluenceCreateStorage, "storage", false, "Read raw storage-format XHTML from stdin")

	confluenceUpdateCmd.Flags().StringVar(&confluenceUpdateTitle, "title", "", "New page title")
	confluenceUpdateCmd.Flags().StringVar(&confluenceUpdateBodyFile, "body-file", "", "Path to markdown body file (- for stdin)")
	confluenceUpdateCmd.Flags().BoolVar(&confluenceUpdateADF, "adf", false, "Read raw ADF JSON from stdin")
	confluenceUpdateCmd.Flags().BoolVar(&confluenceUpdateStorage, "storage", false, "Read raw storage-format XHTML from stdin")
}
