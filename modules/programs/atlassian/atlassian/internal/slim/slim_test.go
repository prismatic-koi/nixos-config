package slim_test

import (
	"encoding/json"
	"testing"

	"github.com/prismatic-koi/atlassian/internal/slim"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestApply_UniversalDropKeys(t *testing.T) {
	input := decode(t, `{"expand":"names","self":"https://x","name":"Ben","avatarUrls":{"48x48":"u"}}`)
	result := slim.Apply(input, slim.UniversalDropKeys).(map[string]any)
	if _, ok := result["expand"]; ok {
		t.Error("expand should be dropped")
	}
	if _, ok := result["self"]; ok {
		t.Error("self should be dropped")
	}
	if _, ok := result["avatarUrls"]; ok {
		t.Error("avatarUrls should be dropped")
	}
	if result["name"] != "Ben" {
		t.Errorf("name should be preserved, got %v", result["name"])
	}
}

func TestApply_JiraIssueDropKeys(t *testing.T) {
	input := decode(t, `{"key":"FOO-1","renderedFields":{},"operations":{},"permissions":{},"transitions":[],"watchers":{},"worklog":{},"attachments":[],"properties":{},"names":{},"subtask":false,"hierarchyLevel":0,"editmeta":{},"versionedRepresentations":{},"colorName":"blue","fields":{"summary":"test"}}`)
	result := slim.Apply(input, slim.JiraIssueDropKeys).(map[string]any)
	for _, key := range []string{"renderedFields", "operations", "permissions", "transitions", "watchers", "worklog", "attachments", "properties", "names", "subtask", "hierarchyLevel", "editmeta", "versionedRepresentations", "colorName"} {
		if _, ok := result[key]; ok {
			t.Errorf("key %q should be dropped", key)
		}
	}
	if result["key"] != "FOO-1" {
		t.Error("key should be preserved")
	}
}

func TestApply_ConfluenceDropKeys(t *testing.T) {
	input := decode(t, `{"id":"123","_links":{"self":"u"},"status":"current","lastModified":"2024"}`)
	result := slim.Apply(input, slim.ConfluenceDropKeys).(map[string]any)
	for _, key := range []string{"_links", "status", "lastModified"} {
		if _, ok := result[key]; ok {
			t.Errorf("key %q should be dropped", key)
		}
	}
	if result["id"] != "123" {
		t.Error("id should be preserved")
	}
}

func TestApply_UserInfoDropKeys(t *testing.T) {
	input := decode(t, `{"account_id":"abc","account_status":"active","characteristics":{},"last_updated":"2024","created_at":"2023","nickname":"bn","locale":"en","extended_profile":{},"account_type":"atlassian","email_verified":true,"email":"b@x.com"}`)
	result := slim.Apply(input, slim.UserInfoDropKeys).(map[string]any)
	for _, key := range []string{"account_status", "characteristics", "last_updated", "created_at", "nickname", "locale", "extended_profile", "account_type", "email_verified"} {
		if _, ok := result[key]; ok {
			t.Errorf("key %q should be dropped", key)
		}
	}
	if result["account_id"] != "abc" {
		t.Error("account_id should be preserved")
	}
}

func TestApply_ResourceDropKeys(t *testing.T) {
	input := decode(t, `{"id":"cloud-123","name":"My Site","scopes":["read:jira"],"url":"https://mysite.atlassian.net"}`)
	result := slim.Apply(input, slim.ResourceDropKeys).(map[string]any)
	if _, ok := result["scopes"]; ok {
		t.Error("scopes should be dropped")
	}
	if _, ok := result["url"]; ok {
		t.Error("url should be dropped")
	}
	if result["id"] != "cloud-123" {
		t.Error("id should be preserved")
	}
}

func TestApply_SearchListDropKeys(t *testing.T) {
	input := decode(t, `{"key":"FOO-1","body":{"x":1},"description":"desc","content":[],"comments":{},"comment":{},"changelog":{},"history":{},"adf":{},"summary":"test"}`)
	result := slim.Apply(input, slim.SearchListDropKeys).(map[string]any)
	for _, key := range []string{"body", "description", "content", "comments", "comment", "changelog", "history", "adf"} {
		if _, ok := result[key]; ok {
			t.Errorf("key %q should be dropped", key)
		}
	}
	if result["summary"] != "test" {
		t.Error("summary should be preserved")
	}
}

func TestApply_Nested(t *testing.T) {
	input := decode(t, `{"fields":{"avatarUrls":{"48x48":"u"},"summary":"Hello"}}`)
	result := slim.Apply(input, slim.UniversalDropKeys).(map[string]any)
	fields := result["fields"].(map[string]any)
	if _, ok := fields["avatarUrls"]; ok {
		t.Error("nested avatarUrls should be dropped")
	}
	if fields["summary"] != "Hello" {
		t.Error("nested summary should be preserved")
	}
}

func TestApply_Array(t *testing.T) {
	input := decode(t, `[{"expand":"x","name":"a"},{"expand":"y","name":"b"}]`)
	result := slim.Apply(input, slim.UniversalDropKeys).([]any)
	for i, elem := range result {
		m := elem.(map[string]any)
		if _, ok := m["expand"]; ok {
			t.Errorf("element %d: expand should be dropped", i)
		}
	}
}

func TestMergeKeys(t *testing.T) {
	merged := slim.MergeKeys(slim.UniversalDropKeys, slim.ResourceDropKeys)
	for k := range slim.UniversalDropKeys {
		if !merged[k] {
			t.Errorf("merged missing universal key %q", k)
		}
	}
	for k := range slim.ResourceDropKeys {
		if !merged[k] {
			t.Errorf("merged missing resource key %q", k)
		}
	}
}
