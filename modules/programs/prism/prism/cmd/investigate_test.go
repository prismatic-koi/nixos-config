package cmd

import (
	"testing"
)

func TestInvestigateSlug(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{
			// Issue example — slug is ≤30 chars from the start of the prompt.
			prompt: "trace the call chain for SSH auth",
			want:   "trace-the-call-chain-for-ssh-a",
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
			want:   "a-very-long-prompt-that-goes-w",
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
