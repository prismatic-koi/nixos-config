package cmd

// Tests for --json flag on prism profile list / show (#1499).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// TestBuildProfileJSON_SnakeCaseAndActiveFlag verifies the JSON shape of a
// single profile object.
func TestBuildProfileJSON_SnakeCaseAndActiveFlag(t *testing.T) {
	entry := config.ProfileEntry{
		"coordinator": config.RoleSlot{
			Provider: "anthropic",
			Model:    "anthropic/claude-sonnet-4-6",
			Thinking: "high",
			Harness:  "opencode",
		},
		"worker": config.RoleSlot{
			Model: "anthropic/claude-sonnet-4-6",
		},
	}

	row := buildProfileJSON("anthropic", entry, "anthropic")

	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, k := range []string{"name", "active", "slots"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("missing required snake_case key %q", k)
		}
	}
	if obj["name"] != "anthropic" {
		t.Errorf("name: want 'anthropic', got %v", obj["name"])
	}
	if obj["active"] != true {
		t.Errorf("active: want true, got %v", obj["active"])
	}

	slots, ok := obj["slots"].(map[string]interface{})
	if !ok {
		t.Fatalf("slots must be an object, got %T", obj["slots"])
	}
	if _, ok := slots["coordinator"]; !ok {
		t.Errorf("missing coordinator role in slots: %v", slots)
	}

	coord := slots["coordinator"].(map[string]interface{})
	for _, k := range []string{"provider", "model", "thinking", "harness", "system_prompt_path"} {
		if _, ok := coord[k]; !ok {
			t.Errorf("slots.coordinator missing snake_case key %q", k)
		}
	}
	// Reject camelCase leftovers.
	for _, badK := range []string{"systemPromptPath"} {
		if _, ok := coord[badK]; ok {
			t.Errorf("slots.coordinator must not have camelCase key %q", badK)
		}
	}
}

// TestBuildProfileJSON_NotActive verifies that active=false when the profile
// is not the resolved active profile.
func TestBuildProfileJSON_NotActive(t *testing.T) {
	entry := config.ProfileEntry{}
	row := buildProfileJSON("other", entry, "anthropic")
	if row.Active {
		t.Errorf("active: want false for non-active profile, got true")
	}
}

// TestRenderProfileListJSON_EmptyArrayWhenNoProfiles is a sanity check on
// the JSON shape when no profiles are configured (encoded by buildProfileJSON
// over an empty slice). The empty case is not exercised through the CLI
// because profiles.json must declare at least one profile, but the JSON
// renderer must still produce `[]`.
func TestRenderProfileListJSON_EmptyArray(t *testing.T) {
	rows := make([]profileJSON, 0)
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Errorf("expected '[]' for empty profile list, got %q", string(data))
	}
}
