//go:build linux

package container

// agent_credentials_bwrap_test.go — Linux live coverage of the read-only
// binds of the agent credential files. agent_credentials_test.go asserts the
// argv. These tests run a real bwrap with the production emitter
// (bwrapCredentialBinds) and no bind of the host root, so the binds under test
// are the only route to the files:
//
//   - the session reads each file at the path that its file variable holds;
//   - it cannot write, delete, or replace a file;
//   - it cannot read the concrete sops dir, another secret in it, or another
//     symlink in the stable dir;
//   - without the binds, the read fails (the paired negative);
//   - with a read-write bind, the write works (the paired negative of the
//     write test).
//
// The tests skip where bwrap cannot run (requireUsableBwrapForUsageTest).
//
// SECURITY: every value is synthetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticSiblingSecret = "synthetic-sibling-secret"

// liveCredentialFixture makes two credential files and a sibling secret that
// no credential delivers, all in one fake sops tree with a stable symlink
// for each. It returns the links, the concrete paths, and the production
// binds of a delivery of the two credentials.
type liveCredentialFixture struct {
	passwordLink, passwordConcrete string
	tokenLink, tokenConcrete       string
	siblingLink, siblingConcrete   string
	binds                          []string
}

func newLiveCredentialFixture(t *testing.T) liveCredentialFixture {
	t.Helper()
	root := credentialRoot(t)
	var f liveCredentialFixture
	f.passwordLink, f.passwordConcrete = credentialFileChain(t, root, passwordSecretName, syntheticCredentialPassword)
	f.tokenLink, f.tokenConcrete = credentialFileChain(t, root, tokenSecretName, syntheticCredentialToken)
	f.siblingLink, f.siblingConcrete = credentialFileChain(t, root, "github_token", syntheticSiblingSecret)
	kept, binds := bwrapCredentialBinds(twoCredentials(f.passwordLink, f.tokenLink))
	if len(kept.Credentials) != 2 {
		t.Fatalf("bwrapCredentialBinds kept %d credentials, want 2", len(kept.Credentials))
	}
	f.binds = binds
	return f
}

func hidesSecrets(out string) string {
	for _, v := range []string{syntheticCredentialPassword, syntheticCredentialToken, syntheticSiblingSecret} {
		out = strings.ReplaceAll(out, v, "<secret>")
	}
	return out
}

func TestBwrapCredentialFile_ReadableInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	f := newLiveCredentialFixture(t)
	for link, want := range map[string]string{f.passwordLink: syntheticCredentialPassword, f.tokenLink: syntheticCredentialToken} {
		out, err := runInBwrap(t, bwrapBin, f.binds, "cat "+shQuoteForTest(link))
		if err != nil {
			t.Fatalf("read of %s in the sandbox failed: %v — %s", link, err, hidesSecrets(out))
		}
		if strings.TrimSpace(out) != want {
			t.Errorf("the sandbox read %d bytes from %s that are not its secret", len(out), link)
		}
	}
}

func TestBwrapCredentialFile_UnreadableWithoutBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	f := newLiveCredentialFixture(t)
	for _, p := range []string{f.passwordLink, f.passwordConcrete, f.tokenLink, f.tokenConcrete} {
		out, err := runInBwrap(t, bwrapBin, nil, "cat "+shQuoteForTest(p))
		if err == nil || strings.Contains(out, syntheticCredentialPassword) || strings.Contains(out, syntheticCredentialToken) {
			t.Errorf("read of %s worked with no bind, so the positive test proves nothing: %v", p, err)
		}
	}
}

func TestBwrapCredentialFile_WriteDeleteReplaceDenied(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	f := newLiveCredentialFixture(t)
	for link, concrete := range map[string]string{f.passwordLink: f.passwordConcrete, f.tokenLink: f.tokenConcrete} {
		q := shQuoteForTest(link)
		other := shQuoteForTest(filepath.Join(filepath.Dir(link), "forged"))
		for name, script := range map[string]string{
			"write":           "echo forged > " + q,
			"append":          "echo forged >> " + q,
			"delete":          "rm -f " + q,
			"rename onto":     "echo forged > " + other + " && mv -f " + other + " " + q,
			"symlink onto":    "ln -sf /etc/passwd " + q,
			"rename away":     "mv " + q + " " + other,
			"truncate":        ": > " + q,
			"change the mode": "chmod 0666 " + q,
		} {
			out, err := runInBwrap(t, bwrapBin, f.binds, script+" && cat "+q)
			if err == nil {
				t.Errorf("%s of %s: the sandbox changed the file: %s", name, filepath.Base(link), hidesSecrets(out))
			}
		}
		info, err := os.Stat(concrete)
		if err != nil || info.Mode().Perm() != 0o400 {
			t.Errorf("the mode of %s on the host changed: %v %v", concrete, info.Mode(), err)
		}
	}
	for concrete, want := range map[string]string{f.passwordConcrete: syntheticCredentialPassword, f.tokenConcrete: syntheticCredentialToken} {
		if data, err := os.ReadFile(concrete); err != nil || string(data) != want+"\n" {
			t.Errorf("the file %s on the host changed (err=%v)", concrete, err)
		}
	}
}

// TestBwrapCredentialFile_WriteWorksWithReadWriteBind is the paired negative
// of the write test: with a read-write bind of the same file, the same write
// works, so the denied write above is the effect of --ro-bind.
func TestBwrapCredentialFile_WriteWorksWithReadWriteBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	f := newLiveCredentialFixture(t)
	if err := os.Chmod(f.passwordConcrete, 0o600); err != nil {
		t.Fatal(err)
	}
	rw := []string{"--bind", f.passwordConcrete, f.passwordLink}
	if out, err := runInBwrap(t, bwrapBin, rw, "echo forged > "+shQuoteForTest(f.passwordLink)); err != nil {
		t.Errorf("the write failed with a read-write bind, so the write test proves nothing: %v — %s", err, hidesSecrets(out))
	}
}

// The binds expose the delivered files only. The concrete sops dir, the
// other secrets in it, and the other symlinks of the stable dir stay out of
// the sandbox.
func TestBwrapCredentialFile_SopsDirUnreadable(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	f := newLiveCredentialFixture(t)
	for _, script := range []string{
		"cat " + shQuoteForTest(f.siblingConcrete),
		"cat " + shQuoteForTest(f.siblingLink),
		"ls " + shQuoteForTest(filepath.Dir(f.passwordConcrete)),
		"cat " + shQuoteForTest(f.passwordConcrete),
		"cat " + shQuoteForTest(f.tokenConcrete),
	} {
		out, err := runInBwrap(t, bwrapBin, f.binds, script)
		if err == nil || strings.Contains(out, syntheticSiblingSecret) || strings.Contains(out, syntheticCredentialPassword) || strings.Contains(out, syntheticCredentialToken) {
			t.Errorf("%q worked in the sandbox: %v", script, err)
		}
	}
	out, err := runInBwrap(t, bwrapBin, f.binds, "ls "+shQuoteForTest(filepath.Dir(f.passwordLink)))
	if err != nil {
		t.Fatalf("ls of the stable dir failed: %v — %s", err, hidesSecrets(out))
	}
	if got, want := strings.Fields(out), []string{passwordSecretName, tokenSecretName}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the stable dir in the sandbox lists %q, want only the delivered files %q", got, want)
	}
}
