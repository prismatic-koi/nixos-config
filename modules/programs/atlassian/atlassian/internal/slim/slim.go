// Package slim applies drop-key sets to JSON values, mirroring the
// mcp-atlassian-slim-proxy.mjs behaviour exactly.
package slim

// Drop-key sets mirrored verbatim from mcp-atlassian-slim-proxy.mjs.

// UniversalDropKeys are removed from every response.
var UniversalDropKeys = map[string]bool{
	"expand":     true,
	"self":       true,
	"iconUrl":    true,
	"avatarUrl":  true,
	"avatarUrls": true,
	"avatarId":   true,
	"picture":    true,
	"schema":     true,
}

// JiraIssueDropKeys are removed from Jira issue responses.
var JiraIssueDropKeys = map[string]bool{
	"renderedFields":          true,
	"operations":              true,
	"permissions":             true,
	"transitions":             true,
	"watchers":                true,
	"worklog":                 true,
	"attachments":             true,
	"properties":              true,
	"names":                   true,
	"subtask":                 true,
	"hierarchyLevel":          true,
	"editmeta":                true,
	"versionedRepresentations": true,
	"colorName":               true,
}

// ConfluenceDropKeys are removed from Confluence responses.
var ConfluenceDropKeys = map[string]bool{
	"_links":       true,
	"status":       true,
	"lastModified": true,
}

// UserInfoDropKeys are removed from user-info responses.
var UserInfoDropKeys = map[string]bool{
	"account_status":   true,
	"characteristics":  true,
	"last_updated":     true,
	"created_at":       true,
	"nickname":         true,
	"locale":           true,
	"extended_profile": true,
	"account_type":     true,
	"email_verified":   true,
}

// ResourceDropKeys are removed from resource-listing responses.
var ResourceDropKeys = map[string]bool{
	"scopes": true,
	"url":    true,
}

// SearchListDropKeys are additional keys dropped from search/list responses.
var SearchListDropKeys = map[string]bool{
	"body":        true,
	"description": true,
	"content":     true,
	"comments":    true,
	"comment":     true,
	"changelog":   true,
	"history":     true,
	"adf":         true,
}

// Apply recursively removes any key in dropKeys from v (which must be a
// JSON-decoded value, i.e. map[string]any, []any, or a scalar).
func Apply(v any, dropKeys map[string]bool) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if dropKeys[k] {
				continue
			}
			out[k] = Apply(child, dropKeys)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, elem := range val {
			out[i] = Apply(elem, dropKeys)
		}
		return out
	default:
		return v
	}
}

// MergeKeys merges multiple drop-key maps into one.
func MergeKeys(sets ...map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for _, s := range sets {
		for k, v := range s {
			out[k] = v
		}
	}
	return out
}
