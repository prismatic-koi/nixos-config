package review

import (
	"errors"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/gitlab"
)

// These tests exercise the pure GitLab mapping helpers that back the GitLab
// branch of CheckPRState and ResolvePRBaseRef. They take a canned
// (*gitlab.MR, error) directly, so no live glab or gitlab.com access is
// needed — the glab exec seam is tested in internal/gitlab.

func TestGitLabMRStateToError_Opened(t *testing.T) {
	err := gitLabMRStateToError("7", &gitlab.MR{State: "opened", TargetBranch: "main"}, nil)
	if err != nil {
		t.Fatalf("opened MR should pass, got: %v", err)
	}
}

func TestGitLabMRStateToError_Merged(t *testing.T) {
	err := gitLabMRStateToError("7", &gitlab.MR{State: "merged"}, nil)
	assertKind(t, err, PRStateMerged, "is merged")
}

func TestGitLabMRStateToError_MergedViaMergedAt(t *testing.T) {
	at := "2026-08-07T05:25:52.324Z"
	// state still "closed" but merged_at set → merged takes precedence.
	err := gitLabMRStateToError("7", &gitlab.MR{State: "closed", MergedAt: &at}, nil)
	assertKind(t, err, PRStateMerged, "is merged")
}

func TestGitLabMRStateToError_Closed(t *testing.T) {
	err := gitLabMRStateToError("7", &gitlab.MR{State: "closed"}, nil)
	assertKind(t, err, PRStateClosed, "is closed")
}

func TestGitLabMRStateToError_Locked(t *testing.T) {
	err := gitLabMRStateToError("7", &gitlab.MR{State: "locked"}, nil)
	assertKind(t, err, PRStateClosed, "is closed")
}

func TestGitLabMRStateToError_NotFound(t *testing.T) {
	err := gitLabMRStateToError("99999", nil, gitlab.ErrMRNotFound)
	assertKind(t, err, PRStateMissing, "does not exist")
}

func TestGitLabMRStateToError_Transient(t *testing.T) {
	err := gitLabMRStateToError("7", nil, errors.New("dial tcp: no such host"))
	assertKind(t, err, PRStateTransient, "could not determine MR state")
}

func TestGitLabMRStateToError_UnknownState(t *testing.T) {
	err := gitLabMRStateToError("7", &gitlab.MR{State: "wibble"}, nil)
	assertKind(t, err, PRStateTransient, "unrecognised state")
}

func assertKind(t *testing.T, err error, wantKind PRStateErrorKind, wantMsgSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *PRStateError, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PRStateError, got %T: %v", err, err)
	}
	if pe.Kind != wantKind {
		t.Errorf("Kind = %q, want %q", pe.Kind, wantKind)
	}
	if !strings.Contains(pe.Msg, wantMsgSubstr) {
		t.Errorf("Msg = %q, want substring %q", pe.Msg, wantMsgSubstr)
	}
}

func TestResolveGitLabBaseRef(t *testing.T) {
	if got := resolveGitLabBaseRef(&gitlab.MR{TargetBranch: "main"}, nil); got != "main" {
		t.Errorf("base ref = %q, want main", got)
	}
	if got := resolveGitLabBaseRef(&gitlab.MR{TargetBranch: "release-2.0"}, nil); got != "release-2.0" {
		t.Errorf("base ref = %q, want release-2.0", got)
	}
	// Error or nil MR collapses to "" (fall back to default base), matching
	// the GitHub best-effort contract.
	if got := resolveGitLabBaseRef(nil, errors.New("boom")); got != "" {
		t.Errorf("base ref on error = %q, want empty", got)
	}
	if got := resolveGitLabBaseRef(nil, nil); got != "" {
		t.Errorf("base ref on nil MR = %q, want empty", got)
	}
}
