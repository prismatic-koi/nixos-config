package container

// gitconfig_credential_test.go — the [credential] reset and the
// thankyou-payroll SSH rewrite in the per-session gitconfig (#3460).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	wantCredentialSection = "\n[credential]\n    helper =\n"
	wantURLSection        = "\n[url \"git@github.com:thankyou-payroll/\"]\n" +
		"    insteadOf = https://github.com/thankyou-payroll/\n"
)

// sessionGitconfigWriters gives each isolator path that writes a session
// gitconfig, and the host path of the file that it writes.
var sessionGitconfigWriters = []struct {
	name  string
	write func(t *testing.T, m *Manager) string
}{
	{"bwrap", func(t *testing.T, m *Manager) string {
		t.Helper()
		if err := (&bwrapIsolator{}).WriteGitconfig(m); err != nil {
			t.Fatalf("bwrap WriteGitconfig: %v", err)
		}
		t.Cleanup(func() {
			_ = os.Remove(m.gitconfigFilePath())
			_ = os.Remove(m.allowedSignersFilePath())
		})
		return m.gitconfigFilePath()
	}},
	{"sandbox-exec", func(t *testing.T, m *Manager) string {
		t.Helper()
		if err := (&sandboxExecIsolator{name: m.name}).WriteGitconfig(m); err != nil {
			t.Fatalf("sandbox-exec WriteGitconfig: %v", err)
		}
		sessionDir, err := m.sessionWorkDirPath()
		if err != nil {
			t.Fatalf("sessionWorkDirPath: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })
		return SessionWorkDirGitconfigPath(sessionDir)
	}},
}

// writeSessionGitconfig makes a fake HOME, with or without the signing key
// pair, and returns the content and path of the gitconfig that the writer
// makes.
func writeSessionGitconfig(t *testing.T, writer int, signing bool) (string, string) {
	t.Helper()
	fakeHome := newFakeHome(t)
	if !signing {
		for _, k := range []string{"prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
			if err := os.Remove(filepath.Join(fakeHome, ".ssh", k)); err != nil {
				t.Fatalf("remove %s: %v", k, err)
			}
		}
	}
	m := New(Config{
		SessionName:   "repo@feat",
		InstanceID:    "gitconfig-credential-test",
		AllocatedPort: 14000,
		GitUserName:   "test-user",
		GitUserEmail:  "test@example.com",
	})
	path := sessionGitconfigWriters[writer].write(t, m)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	return string(data), path
}

// TestSessionGitconfig_CredentialResetAndSSHRewrite: each writer puts the
// empty credential helper and the thankyou-payroll rewrite in the session
// gitconfig, also when the signing keys are not available.
func TestSessionGitconfig_CredentialResetAndSSHRewrite(t *testing.T) {
	for i, w := range sessionGitconfigWriters {
		for _, signing := range []bool{true, false} {
			label := "signing"
			if !signing {
				label = "no-signing"
			}
			t.Run(w.name+"/"+label, func(t *testing.T) {
				content, _ := writeSessionGitconfig(t, i, signing)
				if got := strings.Contains(content, "gpgsign = true"); got != signing {
					t.Fatalf("gpgsign present = %v, want %v: the fixture did not select the signing path; content:\n%s",
						got, signing, content)
				}
				for _, want := range []string{wantCredentialSection, wantURLSection} {
					if !strings.Contains(content, want) {
						t.Errorf("gitconfig missing %q; content:\n%s", want, content)
					}
				}
			})
		}
	}
}

// gitWithConfig runs git with the given global and system config files and
// no other config. GIT_CONFIG_COUNT=0 drops the overrides that a host shell
// puts in the environment (user-git-overrides.md).
func gitWithConfig(t *testing.T, global, system, stdin string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+global,
		"GIT_CONFIG_SYSTEM="+system,
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_PARAMETERS=",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
	return stdout.String(), stderr.String()
}

// TestSessionGitconfig_GitResolvesCredentialAndURL runs git against the
// generated file. A fake system gitconfig names a credential helper that
// prints a marker, in place of the osxkeychain helper of the nix git. The
// control runs show that the marker helper runs and that the URL stays
// HTTPS when the session gitconfig is absent.
func TestSessionGitconfig_GitResolvesCredentialAndURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	const marker = "PRISM-TEST-SYSTEM-HELPER-RAN"
	system := filepath.Join(t.TempDir(), "system-gitconfig")
	if err := os.WriteFile(system, []byte("[credential]\n    helper = \"!f() { echo "+marker+" >&2; }; f\"\n"), 0o600); err != nil {
		t.Fatalf("write system gitconfig: %v", err)
	}
	const fillInput = "protocol=https\nhost=github.com\npath=thankyou-payroll/repo\n\n"

	for i, w := range sessionGitconfigWriters {
		for _, signing := range []bool{true, false} {
			label := "signing"
			if !signing {
				label = "no-signing"
			}
			t.Run(w.name+"/"+label, func(t *testing.T) {
				_, global := writeSessionGitconfig(t, i, signing)

				out, _ := gitWithConfig(t, global, system, "", "config", "--get-all", "credential.helper")
				lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
				if last := lines[len(lines)-1]; last != "" {
					t.Errorf("last credential.helper value = %q, want the empty reset; all values:\n%s", last, out)
				}

				_, stderr := gitWithConfig(t, global, system, fillInput, "credential", "fill")
				if strings.Contains(stderr, marker) {
					t.Errorf("the system credential helper ran with the session gitconfig; stderr:\n%s", stderr)
				}

				for url, want := range map[string]string{
					"https://github.com/thankyou-payroll/repo":       "git@github.com:thankyou-payroll/repo",
					"https://github.com/NixOS/nixpkgs":               "https://github.com/NixOS/nixpkgs",
					"https://github.com/thankyou-payroll-other/repo": "https://github.com/thankyou-payroll-other/repo",
				} {
					got, stderr := gitWithConfig(t, global, system, "", "ls-remote", "--get-url", url)
					if got = strings.TrimSpace(got); got != want {
						t.Errorf("ls-remote --get-url %s = %q, want %q; stderr:\n%s", url, got, want, stderr)
					}
				}
			})
		}
	}

	t.Run("control", func(t *testing.T) {
		_, stderr := gitWithConfig(t, os.DevNull, system, fillInput, "credential", "fill")
		if !strings.Contains(stderr, marker) {
			t.Errorf("the system credential helper did not run with no session gitconfig; the test cannot detect a missing reset; stderr:\n%s", stderr)
		}
		const url = "https://github.com/thankyou-payroll/repo"
		got, _ := gitWithConfig(t, os.DevNull, system, "", "ls-remote", "--get-url", url)
		if got = strings.TrimSpace(got); got != url {
			t.Errorf("ls-remote --get-url %s = %q with no session gitconfig, want it unchanged", url, got)
		}
	})
}
