package gitlab

import (
	"errors"
	"reflect"
	"testing"
)

// fakeRunner is a scripted Runner: it records the args it was called with and
// returns canned output, so no live glab or gitlab.com access is needed.
type fakeRunner struct {
	stdout   string
	stderr   string
	err      error
	lastArgs []string
	calls    int
}

func (f *fakeRunner) Run(args ...string) (string, string, error) {
	f.calls++
	f.lastArgs = args
	return f.stdout, f.stderr, f.err
}

func TestViewMR_ParsesJSON(t *testing.T) {
	r := &fakeRunner{stdout: `{
		"iid": 7,
		"title": "Add IAM auth",
		"description": "Closes #3",
		"source_branch": "feat/iam",
		"target_branch": "main",
		"state": "opened",
		"merged_at": null,
		"sha": "abc123"
	}`}
	mr, err := ViewMRWith(r, "owner/repo", "7")
	if err != nil {
		t.Fatalf("ViewMRWith: unexpected error: %v", err)
	}
	want := &MR{
		IID:          7,
		Title:        "Add IAM auth",
		Description:  "Closes #3",
		SourceBranch: "feat/iam",
		TargetBranch: "main",
		State:        "opened",
		SHA:          "abc123",
	}
	if !reflect.DeepEqual(mr, want) {
		t.Errorf("ViewMRWith = %+v, want %+v", mr, want)
	}
	// -R <repo> must be forwarded, then the mr view args.
	wantArgs := []string{"-R", "owner/repo", "mr", "view", "7", "-F", "json"}
	if !reflect.DeepEqual(r.lastArgs, wantArgs) {
		t.Errorf("args = %v, want %v", r.lastArgs, wantArgs)
	}
}

func TestViewMR_EmptyRepoOmitsDashR(t *testing.T) {
	r := &fakeRunner{stdout: `{"iid":1,"state":"opened","target_branch":"main"}`}
	if _, err := ViewMRWith(r, "", "1"); err != nil {
		t.Fatalf("ViewMRWith: unexpected error: %v", err)
	}
	wantArgs := []string{"mr", "view", "1", "-F", "json"}
	if !reflect.DeepEqual(r.lastArgs, wantArgs) {
		t.Errorf("args = %v, want %v (no -R when repo empty)", r.lastArgs, wantArgs)
	}
}

func TestViewMR_NotFoundMapsToSentinel(t *testing.T) {
	// glab prints a JSON error to stdout and a human message to stderr, and
	// exits non-zero, on a missing MR.
	r := &fakeRunner{
		stdout: `{"error":{"message":"failed to get merge request 99999: 404 Not Found"}}`,
		stderr: "ERROR: Failed to get merge request 99999: 404 Not Found.",
		err:    errors.New("exit status 1"),
	}
	_, err := ViewMRWith(r, "owner/repo", "99999")
	if !errors.Is(err, ErrMRNotFound) {
		t.Fatalf("ViewMRWith: err = %v, want ErrMRNotFound", err)
	}
}

func TestViewMR_TransientErrorNotSentinel(t *testing.T) {
	r := &fakeRunner{
		stderr: "ERROR: dial tcp: lookup gitlab.com: no such host",
		err:    errors.New("exit status 1"),
	}
	_, err := ViewMRWith(r, "owner/repo", "7")
	if err == nil {
		t.Fatal("ViewMRWith: expected error, got nil")
	}
	if errors.Is(err, ErrMRNotFound) {
		t.Error("ViewMRWith: transient error must NOT map to ErrMRNotFound")
	}
}

func TestViewMR_MalformedJSON(t *testing.T) {
	r := &fakeRunner{stdout: "not json"}
	if _, err := ViewMRWith(r, "", "7"); err == nil {
		t.Fatal("ViewMRWith: expected parse error for malformed JSON, got nil")
	}
}

func TestDiffMR(t *testing.T) {
	r := &fakeRunner{stdout: "--- a\n+++ b\n"}
	diff, err := DiffMRWith(r, "owner/repo", "7")
	if err != nil {
		t.Fatalf("DiffMRWith: unexpected error: %v", err)
	}
	if diff != "--- a\n+++ b\n" {
		t.Errorf("DiffMRWith diff = %q", diff)
	}
	wantArgs := []string{"-R", "owner/repo", "mr", "diff", "7", "--color", "never"}
	if !reflect.DeepEqual(r.lastArgs, wantArgs) {
		t.Errorf("args = %v, want %v", r.lastArgs, wantArgs)
	}
}

func TestDiffMR_Error(t *testing.T) {
	r := &fakeRunner{stderr: "boom", err: errors.New("exit status 1")}
	if _, err := DiffMRWith(r, "", "7"); err == nil {
		t.Fatal("DiffMRWith: expected error, got nil")
	}
}

func TestViewIssue(t *testing.T) {
	r := &fakeRunner{stdout: "issue body"}
	out, err := ViewIssueWith(r, "owner/repo", "3")
	if err != nil {
		t.Fatalf("ViewIssueWith: unexpected error: %v", err)
	}
	if out != "issue body" {
		t.Errorf("ViewIssueWith = %q", out)
	}
	wantArgs := []string{"-R", "owner/repo", "issue", "view", "3"}
	if !reflect.DeepEqual(r.lastArgs, wantArgs) {
		t.Errorf("args = %v, want %v", r.lastArgs, wantArgs)
	}
}
