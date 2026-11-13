//go:build linux

package container

// unifi_bwrap_test.go — Linux live coverage of the read-only bind of the
// UniFi password file (issue #3472). unifi_test.go asserts the argv. These
// tests run a real bwrap with the production emitter (bwrapUniFiPasswordBind)
// and no bind of the host root, so the bind under test is the only route to
// the password:
//
//   - the session reads the password at the path that UNIFI_PASSWORD_FILE
//     holds;
//   - it cannot write, delete, or replace the file;
//   - it cannot read the concrete sops dir or another secret in it;
//   - without the bind, the read fails (the paired negative);
//   - with a read-write bind, the write works (the paired negative of the
//     write test).
//
// The tests skip where bwrap cannot run (requireUsableBwrapForUsageTest).
//
// SECURITY: the password value is synthetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBwrapUniFiPassword_ReadableInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	link, _ := uniFiPasswordChain(t)
	out, err := runInBwrap(t, bwrapBin, bwrapUniFiPasswordBind(link), "cat "+shQuoteForTest(link))
	if err != nil {
		t.Fatalf("read of the password file in the sandbox failed: %v — %s", err, out)
	}
	if strings.TrimSpace(out) != syntheticUniFiPassword {
		t.Errorf("the sandbox read %d bytes that are not the password", len(out))
	}
}

func TestBwrapUniFiPassword_UnreadableWithoutBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	link, concrete := uniFiPasswordChain(t)
	for _, p := range []string{link, concrete} {
		out, err := runInBwrap(t, bwrapBin, nil, "cat "+shQuoteForTest(p))
		if err == nil || strings.Contains(out, syntheticUniFiPassword) {
			t.Errorf("read of %s worked with no bind, so the positive test proves nothing: %v", p, err)
		}
	}
}

func TestBwrapUniFiPassword_WriteDeleteReplaceDenied(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	link, concrete := uniFiPasswordChain(t)
	mounts := bwrapUniFiPasswordBind(link)
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
		out, err := runInBwrap(t, bwrapBin, mounts, script+" && cat "+q)
		if err == nil {
			t.Errorf("%s: the sandbox changed the password file: %s", name, strings.ReplaceAll(out, syntheticUniFiPassword, "<password>"))
		}
	}
	data, err := os.ReadFile(concrete)
	if err != nil || string(data) != syntheticUniFiPassword+"\n" {
		t.Errorf("the password file on the host changed (err=%v)", err)
	}
	info, err := os.Stat(concrete)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("the mode of the password file on the host changed: %v %v", info.Mode(), err)
	}
}

// TestBwrapUniFiPassword_WriteWorksWithReadWriteBind is the paired negative of
// the write test: with a read-write bind of the same file, the same write
// works, so the denied write above is the effect of --ro-bind.
func TestBwrapUniFiPassword_WriteWorksWithReadWriteBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	link, concrete := uniFiPasswordChain(t)
	if err := os.Chmod(concrete, 0o600); err != nil {
		t.Fatal(err)
	}
	rw := []string{"--bind", concrete, link}
	if out, err := runInBwrap(t, bwrapBin, rw, "echo forged > "+shQuoteForTest(link)); err != nil {
		t.Errorf("the write failed with a read-write bind, so the write test proves nothing: %v — %s", err, out)
	}
}

// The bind exposes the one file. The concrete sops dir and the other
// secrets in it stay out of the sandbox.
func TestBwrapUniFiPassword_SopsDirUnreadable(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	link, concrete := uniFiPasswordChain(t)
	sibling := filepath.Join(filepath.Dir(concrete), "github_token")
	if err := os.WriteFile(sibling, []byte("synthetic-sibling-secret"), 0o400); err != nil {
		t.Fatal(err)
	}
	mounts := bwrapUniFiPasswordBind(link)
	for _, script := range []string{
		"cat " + shQuoteForTest(sibling),
		"ls " + shQuoteForTest(filepath.Dir(concrete)),
		"cat " + shQuoteForTest(concrete),
	} {
		out, err := runInBwrap(t, bwrapBin, mounts, script)
		if err == nil || strings.Contains(out, "synthetic-sibling-secret") || strings.Contains(out, syntheticUniFiPassword) {
			t.Errorf("%q worked in the sandbox: %v", script, err)
		}
	}
}
