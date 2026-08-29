package payload_test

// redact_gitlab_test.go — the GitLab credential must be redacted on the
// capture path exactly as the GitHub one is.
//
// prism now forwards GITLAB_TOKEN into every agent sandbox, so the value can
// reach a captured frame the same ways a GitHub token can: an `env` dump, a
// `glab` debug line, a test that prints its argv. Both redaction layers must
// cover it — the value layer (exact match on the literal the capturing
// process holds) and the shape layer (the `glpat-` issuer prefix, for the
// case where the capturing process does not hold the value).
//
// SECURITY: every value in this file is synthetic.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

// TestGitLabTokenIsRegisteredCredentialName pins GITLAB_TOKEN into the exact
// name registry. The `_TOKEN` suffix heuristic would also catch it, but the
// heuristic is a backstop; the forwarded credential belongs in the list that
// the pi extension mirrors and the parity test compares.
func TestGitLabTokenIsRegisteredCredentialName(t *testing.T) {
	if payload.GitLabTokenEnvName != "GITLAB_TOKEN" {
		t.Fatalf("GitLabTokenEnvName = %q, want GITLAB_TOKEN", payload.GitLabTokenEnvName)
	}
	if !slices.Contains(payload.CredentialEnvNames(), "GITLAB_TOKEN") {
		t.Errorf("GITLAB_TOKEN missing from CredentialEnvNames(): %v", payload.CredentialEnvNames())
	}
	if !payload.IsCredentialEnvName("GITLAB_TOKEN") {
		t.Error("IsCredentialEnvName(GITLAB_TOKEN) = false, want true")
	}
	// The path-naming variant must stay OUT — redacting a path corrupts
	// diagnosable output and hides nothing.
	if payload.IsCredentialEnvName("GITLAB_TOKEN_PATH") {
		t.Error("IsCredentialEnvName(GITLAB_TOKEN_PATH) = true, want false — it names a file, not a secret")
	}
}

// TestRedactor_GitLabTokenValueLayer is the AC assertion: a captured payload
// carrying the literal GITLAB_TOKEN value is rewritten to a marker naming the
// variable, exactly as GITHUB_TOKEN is.
func TestRedactor_GitLabTokenValueLayer(t *testing.T) {
	const (
		gitlabValue = "synthetic-gitlab-value-0123456789"
		githubValue = "synthetic-github-value-0123456789"
	)
	r := payload.NewRedactorFromEnviron([]string{
		"GITLAB_TOKEN=" + gitlabValue,
		"GITHUB_TOKEN=" + githubValue,
	})

	got := r.Redact("glab api user with " + gitlabValue + " and gh with " + githubValue)
	if strings.Contains(got, gitlabValue) {
		t.Errorf("GITLAB_TOKEN value survived redaction: %q", got)
	}
	if strings.Contains(got, githubValue) {
		t.Errorf("GITHUB_TOKEN value survived redaction: %q", got)
	}
	if !strings.Contains(got, payload.RedactionMarker("GITLAB_TOKEN")) {
		t.Errorf("redacted text does not name GITLAB_TOKEN: %q", got)
	}
}

// TestRedactor_GitLabTokenInJSONPayload covers the DB-write shape: agent
// events are stored as JSON, so the value must be removed from a JSON
// document without breaking its structure.
func TestRedactor_GitLabTokenInJSONPayload(t *testing.T) {
	const gitlabValue = "synthetic-gitlab-value-0123456789"
	r := payload.NewRedactorFromEnviron([]string{"GITLAB_TOKEN=" + gitlabValue})

	doc := `{"tool":"bash","output":"GITLAB_TOKEN=` + gitlabValue + `","exit":0}`
	got := r.RedactJSON(doc)

	if strings.Contains(got, gitlabValue) {
		t.Errorf("GITLAB_TOKEN value survived RedactJSON: %q", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("RedactJSON produced invalid JSON: %v\n%s", err, got)
	}
	if parsed["exit"] == nil {
		t.Errorf("RedactJSON dropped a sibling field: %s", got)
	}
}

// TestRedactor_GitLabPatShapeLayer proves the shape layer stands on its own
// for a `glpat-` token — the case where the capturing process does not hold
// the value (a token pasted into a prompt, or one read from a file inside the
// sandbox).
func TestRedactor_GitLabPatShapeLayer(t *testing.T) {
	r := payload.NewShapeOnlyRedactor()

	const synthetic = "glpat-AbCdEfGhIjKlMnOpQrSt"
	got := r.Redact("glab auth login --token " + synthetic)
	if strings.Contains(got, synthetic) {
		t.Errorf("glpat- shape survived the shape layer: %q", got)
	}
	if !strings.Contains(got, payload.RedactionMarker("gitlab-pat")) {
		t.Errorf("redacted text does not name the gitlab-pat shape: %q", got)
	}

	// Ordinary prose that merely mentions the prefix must not be shredded:
	// the rule needs the issuer prefix AND a token-length body.
	plain := "the prefix glpat- identifies a GitLab PAT"
	if out := r.Redact(plain); out != plain {
		t.Errorf("shape layer matched ordinary prose: %q", out)
	}
}

// TestGitLabPatShapeIsRegistered pins the shape rule into the registry the
// pi extension mirrors, so the parity guard covers it.
func TestGitLabPatShapeIsRegistered(t *testing.T) {
	if !slices.Contains(payload.CredentialShapeNames(), "gitlab-pat") {
		t.Fatalf("gitlab-pat missing from CredentialShapeNames(): %v", payload.CredentialShapeNames())
	}
	triggers := payload.CredentialShapeTriggers()["gitlab-pat"]
	if len(triggers) != 1 || triggers[0] != "glpat-" {
		t.Errorf("gitlab-pat triggers = %v, want [glpat-]", triggers)
	}
}
