package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func TestLoadReviewProfiles(t *testing.T) {
	want := &config.ProfilesFile{}
	ok := func() (*config.ProfilesFile, error) { return want, nil }
	missing := func() (*config.ProfilesFile, error) { return nil, errors.New("profiles: not found") }

	// Host mode loads the file when it exists.
	if pf, err := loadReviewProfiles(config.IsolationMode("host"), false, ok); err != nil || pf != want {
		t.Fatalf("host with file: pf=%v err=%v", pf, err)
	}
	// Host mode does not fail on a missing file alone.
	if pf, err := loadReviewProfiles(config.IsolationMode("host"), false, missing); err != nil || pf != nil {
		t.Fatalf("host missing: pf=%v err=%v", pf, err)
	}
	// Sandboxed modes keep the existing error.
	for _, m := range []string{"bwrap", "sandbox-exec"} {
		_, err := loadReviewProfiles(config.IsolationMode(m), true, missing)
		if err == nil || !strings.Contains(err.Error(), "requires profiles.json") {
			t.Fatalf("%s missing: err=%v", m, err)
		}
	}
}
