package verdict_test

import (
	"encoding/json"
	"testing"

	"github.com/prismatic-koi/prism/internal/verdict"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		text string
		want verdict.Kind
	}{
		{"pass", "summary\n<verdict>PASS</verdict>", verdict.Pass},
		{"pass lowercase", "<verdict>pass</verdict>", verdict.Pass},
		{"fail", "<verdict>FAIL</verdict>", verdict.Fail},
		{"pass with disagreement", "<verdict>PASS_WITH_DISAGREEMENT</verdict>", verdict.PassWithDisagreement},
		{"pass with disagreement lowercase", "<verdict>pass_with_disagreement</verdict>", verdict.PassWithDisagreement},
		{"none", "no marker here", verdict.None},
		{"empty", "", verdict.None},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdict.Parse(tc.text); got != tc.want {
				t.Errorf("Parse(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestParse_PassWithDisagreementDoesNotMatchPass pins that the ordering /
// substring boundary is right: a PASS_WITH_DISAGREEMENT marker must not be
// misread as a plain PASS.
func TestParse_PassWithDisagreementDoesNotMatchPass(t *testing.T) {
	if got := verdict.Parse("<verdict>PASS_WITH_DISAGREEMENT</verdict>"); got == verdict.Pass {
		t.Fatal("PASS_WITH_DISAGREEMENT was misclassified as a plain Pass")
	}
}

// TestParse_OnDecodedText documents the contract at the heart of #2862: Parse
// runs on the DECODED message text, not the raw JSON envelope. A payload that
// encoding/json produced escapes '<' as \u003c, so the marker is invisible to
// the substring rule until the caller decodes the "text" field first.
func TestParse_OnDecodedText(t *testing.T) {
	raw, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: "<verdict>PASS</verdict>"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The raw envelope must NOT match — this is the bug the decode fix removes.
	if verdict.Parse(string(raw)) != verdict.None {
		t.Fatalf("raw JSON envelope %q unexpectedly matched a verdict; the escaping should hide it", raw)
	}
	// After decoding the text field, the marker is visible.
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := verdict.Parse(p.Text); got != verdict.Pass {
		t.Errorf("Parse(decoded) = %v, want Pass", got)
	}
}
