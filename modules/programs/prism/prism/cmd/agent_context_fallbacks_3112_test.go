package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestAgentContextShowsProfileFallbacks covers issue #3112: a slot that names
// fallback models appears under profile_fallbacks, and a slot without them
// does not.
func TestAgentContextShowsProfileFallbacks(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "prism"), 0o755); err != nil {
		t.Fatal(err)
	}
	profiles := `{"default":"max","profiles":{"max":{
		"review-security":{"model":"anthropic/claude-opus-5-5","fallbacks":["anthropic/claude-opus-4-8","anthropic/claude-sonnet-5-5"]},
		"review-goal":{"model":"anthropic/claude-opus-5-5"}}}}`
	if err := os.WriteFile(filepath.Join(cfg, "prism", "profiles.json"), []byte(profiles), 0o644); err != nil {
		t.Fatal(err)
	}

	doc := buildAgentContextDocument(false)

	want := map[string]map[string][]string{
		"max": {"review-security": {"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5-5"}},
	}
	if !reflect.DeepEqual(doc.ProfileFallbacks, want) {
		t.Errorf("ProfileFallbacks = %v, want %v", doc.ProfileFallbacks, want)
	}
	if len(doc.Precedence["review_fallback"]) == 0 {
		t.Error(`precedence["review_fallback"] is empty`)
	}
}

// TestAgentContextProfileFallbacksEmptyWhenMissing checks that a missing
// profiles.json gives {} and not null.
func TestAgentContextProfileFallbacksEmptyWhenMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	doc := buildAgentContextDocument(false)
	if doc.ProfileFallbacks == nil || len(doc.ProfileFallbacks) != 0 {
		t.Errorf("ProfileFallbacks = %#v, want an empty non-nil map", doc.ProfileFallbacks)
	}
}
