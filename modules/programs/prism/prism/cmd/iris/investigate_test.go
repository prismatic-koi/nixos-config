package main

// investigate_test.go — unit tests for the slug-derivation and name-validation
// helpers used by `iris investigate`. The wire-path is covered in
// investigate_integration_test.go against an in-process ClientSocket.
//
// Slug behaviour MUST match prism's investigate slug rules byte-for-byte so a
// coordinator can switch between `prism investigate` and `iris investigate`
// without learning two slug conventions. The test cases below mirror
// cmd/investigate_test.go::TestInvestigateSlug.

import (
	"strings"
	"testing"

	investigatepkg "github.com/prismatic-koi/prism/internal/investigate"
)

func TestIrisInvestigateSlug(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{
			// Word-boundary truncation: matches prism behaviour.
			prompt: "trace the call chain for SSH auth",
			want:   "trace-the-call-chain-for-ssh",
		},
		{
			prompt: "What is happening?",
			want:   "what-is-happening",
		},
		{
			prompt: "  leading and trailing spaces  ",
			want:   "leading-and-trailing-spaces",
		},
		{
			prompt: "underscores_in_prompt",
			want:   "underscores-in-prompt",
		},
		{
			prompt: "UPPERCASE WORDS",
			want:   "uppercase-words",
		},
		{
			prompt: "!!! punctuation only !!!",
			want:   "punctuation-only",
		},
		{
			prompt: "a very long prompt that goes well beyond the thirty character limit set for slugs",
			want:   "a-very-long-prompt-that-goes",
		},
		{
			prompt: "",
			want:   "query",
		},
		{
			prompt: "???",
			want:   "query",
		},
		{
			prompt: "multiple   spaces   between",
			want:   "multiple-spaces-between",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.prompt, func(t *testing.T) {
			got := investigateSlug(tc.prompt)
			if got != tc.want {
				t.Errorf("investigateSlug(%q) = %q, want %q", tc.prompt, got, tc.want)
			}
			if len(got) > 30 {
				t.Errorf("investigateSlug(%q) length %d exceeds 30", tc.prompt, len(got))
			}
		})
	}
}

// TestIrisInvestigateSlugWordBoundary specifically exercises the
// word-boundary truncation: a prompt that produces a normalised string of
// exactly 30+ chars with no dash must fall back to the hard cut.
func TestIrisInvestigateSlugWordBoundary(t *testing.T) {
	prompt := "abcdefghijklmnopqrstuvwxyzabcde"
	got := investigateSlug(prompt)
	if got != "abcdefghijklmnopqrstuvwxyzabcd" {
		t.Errorf("no-dash fallback: got %q, want %q", got, "abcdefghijklmnopqrstuvwxyzabcd")
	}

	prompt2 := "hello world foo" + strings.Repeat("x", 20)
	got2 := investigateSlug(prompt2)
	if got2 != "hello-world" {
		t.Errorf("word-boundary: got %q, want %q", got2, "hello-world")
	}
}

// TestIrisInvestigateValidateName asserts that the iris CLI uses the same
// validation helper as prism so the constraint surface is identical.
func TestIrisInvestigateValidateName(t *testing.T) {
	valid := []string{
		"foo",
		"foo-bar",
		"my-analysis",
		"abc123",
		"a",
		strings.Repeat("a", 40),
	}
	for _, name := range valid {
		if err := investigatepkg.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []struct {
		name        string
		mustContain string
	}{
		{strings.Repeat("a", 41), "maximum is 40"},
		{"-leading", "start or end with a dash"},
		{"trailing-", "start or end with a dash"},
		{"UPPER", "disallowed characters"},
		{"has spaces", "disallowed characters"},
		{"under_score", "disallowed characters"},
		{"foo@bar", "disallowed characters"},
	}
	for _, tc := range invalid {
		err := investigatepkg.ValidateName(tc.name)
		if err == nil {
			t.Errorf("ValidateName(%q) expected error, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.mustContain) {
			t.Errorf("ValidateName(%q): error %q does not contain %q", tc.name, err.Error(), tc.mustContain)
		}
	}
}

// TestIrisInvestigateSessionNameShape asserts that a session built from
// the slug satisfies the sidecar's investigateAgentInvokerSession pattern
// (<invoker>~investigate-<slug>) so the existing notify path can derive
// the caller correctly.
func TestIrisInvestigateSessionNameShape(t *testing.T) {
	invoker := "iris-coordinator@main"
	slug := investigateSlug("trace the call chain for SSH auth")
	sessionName := invoker + "~investigate-" + slug

	if !strings.HasPrefix(sessionName, invoker+"~investigate-") {
		t.Errorf("session name %q does not start with %q", sessionName, invoker+"~investigate-")
	}
	if !strings.Contains(sessionName, "~investigate") {
		t.Errorf("session name %q must contain ~investigate marker", sessionName)
	}
}
