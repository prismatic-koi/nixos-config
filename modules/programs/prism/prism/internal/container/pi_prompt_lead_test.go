package container

import "testing"

func TestPIInvocation_ProtectsLeadingDashAndAt(t *testing.T) {
	for _, p := range []string{"-x", "@file", "plain"} {
		args := PIInvocation(Config{InitialPrompt: p})
		got := args[len(args)-1]
		want := PIPromptLead(p) + p
		if got != want {
			t.Errorf("prompt %q: got %q want %q", p, got, want)
		}
		if p != "plain" && got[0] != ' ' {
			t.Errorf("prompt %q not protected: %q", p, got)
		}
		if p == "plain" && got != p {
			t.Errorf("plain prompt altered: %q", got)
		}
	}
}
