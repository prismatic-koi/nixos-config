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
	Long:  "Commands for interacting with Confluence Cloud (read-only).",
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

func init() {
	confluenceCmd.AddCommand(confluenceSearchCmd)
	confluenceCmd.AddCommand(confluenceGetCmd)

	confluenceSearchCmd.Flags().IntVar(&confluenceSearchLimit, "limit", 25, "Maximum number of results to return (0 = empty result)")
	confluenceGetCmd.Flags().BoolVar(&confluenceGetFull, "full", false, "Return untouched API response without slimming")
}
