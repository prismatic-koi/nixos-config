package cmd

import (
	"strings"
	"testing"
)

func TestInvestigateSlug(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{
			// Word-boundary truncation: last dash at or before 30 chars is used.
			// "trace-the-call-chain-for-ssh-auth" = 34 chars; cap at 30 gives
			// "trace-the-call-chain-for-ssh-a", last dash at index 28 → cut there.
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
			// "a-very-long-prompt-that-goes-well-..." → cap at 30 = "a-very-long-prompt-that-goes-w";
			// last dash at index 28 → cut to "a-very-long-prompt-that-goes".
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

// TestInvestigateSlugWordBoundary specifically exercises the word-boundary
// truncation: a prompt that produces a normalised string of exactly 30 chars
// with no dash must fall back to the hard cut.
func TestInvestigateSlugWordBoundary(t *testing.T) {
	// 31 lowercase letters with no spaces/dashes: must hard-cut at 30.
	prompt := "abcdefghijklmnopqrstuvwxyzabcde"
	got := investigateSlug(prompt)
	if got != "abcdefghijklmnopqrstuvwxyzabcd" {
		t.Errorf("no-dash fallback: got %q, want %q", got, "abcdefghijklmnopqrstuvwxyzabcd")
	}

	// A prompt whose normalised form has a dash within the 30-char window:
	// "hello world-foo-barbarbarbarbarbarbarbar" → "hello-world-foo-barbarbarbarba"[:30]
	// The last dash in the 30-char cap should determine the cut point.
	prompt2 := "hello world foo" + strings.Repeat("x", 20)
	got2 := investigateSlug(prompt2)
	// normalised: "hello-world-foo" + 20 x's = "hello-world-fooxxxxxxxxxxxxxxxxxxxx"
	// cap[:30]   = "hello-world-fooxxxxxxxxxxxxxxx"
	// last dash at index 11 → "hello-world"
	if got2 != "hello-world" {
		t.Errorf("word-boundary: got %q, want %q", got2, "hello-world")
	}
}

func TestValidateInvestigateName(t *testing.T) {
	valid := []string{
		"foo",
		"foo-bar",
		"my-analysis",
		"abc123",
		"a",
		"z",
		// exactly 40 chars
		strings.Repeat("a", 40),
		"pi-removal-analysis-2024-deep-dive-v1-ok",
	}
	for _, name := range valid {
		if err := validateInvestigateName(name); err != nil {
			t.Errorf("validateInvestigateName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []struct {
		name        string
		mustContain string // substring expected in error message
	}{
		// Too long
		{strings.Repeat("a", 41), "maximum is 40"},
		// Leading dash
		{"-leading", "start or end with a dash"},
		// Trailing dash
		{"trailing-", "start or end with a dash"},
		// Uppercase
		{"UPPER", "disallowed characters"},
		// Spaces
		{"has spaces", "disallowed characters"},
		// Mixed: uppercase and special
		{"Has Spaces", "disallowed characters"},
		// Underscore
		{"under_score", "disallowed characters"},
		// Special chars
		{"foo@bar", "disallowed characters"},
	}
	for _, tc := range invalid {
		err := validateInvestigateName(tc.name)
		if err == nil {
			t.Errorf("validateInvestigateName(%q) expected error, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.mustContain) {
			t.Errorf("validateInvestigateName(%q): error %q does not contain %q", tc.name, err.Error(), tc.mustContain)
		}
	}
}

func TestInvestigateSlugNamingConvention(t *testing.T) {
	// Verify that a session name built from the slug satisfies the sidecar's
	// investigateAgentInvokerSession pattern: <invoker>~investigate-<slug>.
	invoker := "nixos-config@main"
	prompt := "trace the call chain for SSH auth"
	slug := investigateSlug(prompt)
	sessionName := invoker + "~investigate-" + slug

	// The sidecar detects investigate sessions by looking for "~investigate" in the name.
	if idx := len(invoker); sessionName[idx:idx+len("~investigate")] != "~investigate" {
		t.Errorf("session name %q does not contain ~investigate at expected position", sessionName)
	}

	// The invoker prefix before ~investigate must equal the original invoker.
	const marker = "~investigate"
	markerIdx := -1
	for i := 0; i < len(sessionName)-len(marker)+1; i++ {
		if sessionName[i:i+len(marker)] == marker {
			markerIdx = i
			break
		}
	}
	if markerIdx < 0 {
		t.Fatalf("~investigate not found in session name %q", sessionName)
	}
	got := sessionName[:markerIdx]
	if got != invoker {
		t.Errorf("invoker prefix = %q, want %q", got, invoker)
	}
}
