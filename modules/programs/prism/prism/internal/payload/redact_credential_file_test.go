package payload_test

// redact_credential_file_test.go — the value layer learns a credential that a
// session gets as a file (issue #3472). No environment variable holds the
// UniFi password: UNIFI_PASSWORD_FILE holds the path of the password file.
// The redactor reads that file, and the marker names the credential.
//
// SECURITY: every value in this file is synthetic.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

const syntheticUniFiPassword = "synthetic-unifi-password-0123456789abcdef"

var uniFiMarker = payload.RedactionMarker(payload.UniFiPasswordName)

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "unifi_password")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCredentialFileEnvNames_UniFiPassword(t *testing.T) {
	got := payload.CredentialFileEnvNames()
	if got[payload.UniFiPasswordFileEnvName] != payload.UniFiPasswordName {
		t.Errorf("CredentialFileEnvNames()[%s] = %q, want %q", payload.UniFiPasswordFileEnvName, got[payload.UniFiPasswordFileEnvName], payload.UniFiPasswordName)
	}
	got["X"] = "Y"
	if _, ok := payload.CredentialFileEnvNames()["X"]; ok {
		t.Error("CredentialFileEnvNames returned the registry itself, not a copy")
	}
	// The path variable names a file. Its value is a path, not a secret.
	if payload.IsCredentialEnvName(payload.UniFiPasswordFileEnvName) {
		t.Errorf("IsCredentialEnvName(%s) = true, want false", payload.UniFiPasswordFileEnvName)
	}
}

// The tool result of `cat "$UNIFI_PASSWORD_FILE"`, of `env` after an export of
// the password, of a login body, and the JSON form of a frame all show the
// marker.
func TestRedactor_ReadsTheFileThatTheEnvironmentNames(t *testing.T) {
	path := writeSecretFile(t, syntheticUniFiPassword+"\n")
	r := payload.NewRedactorFromEnviron([]string{payload.UniFiPasswordFileEnvName + "=" + path})
	if r.ValueCount() != 1 {
		t.Fatalf("ValueCount = %d, want 1", r.ValueCount())
	}
	for _, in := range []string{
		syntheticUniFiPassword + "\n",
		"HOME=/home/x\nPW=" + syntheticUniFiPassword + "\nUNIFI_PASSWORD_FILE=" + path + "\n",
		`{"username":"agent-readonly","password":"` + syntheticUniFiPassword + `","remember":false}`,
	} {
		got := r.Redact(in)
		if strings.Contains(got, syntheticUniFiPassword) {
			t.Errorf("password survived redaction: %q", got)
		}
		if !strings.Contains(got, uniFiMarker) {
			t.Errorf("Redact(%q) = %q, want the marker %s", in, got, uniFiMarker)
		}
	}
	doc, _ := json.Marshal(map[string]string{"output": "password " + syntheticUniFiPassword})
	if got := r.RedactJSON(string(doc)); strings.Contains(got, syntheticUniFiPassword) || !strings.Contains(got, uniFiMarker) {
		t.Errorf("RedactJSON = %q, want the marker in place of the password", got)
	}
	// The path is not a secret, so the output keeps it.
	if got := r.Redact("UNIFI_PASSWORD_FILE=" + path); got != "UNIFI_PASSWORD_FILE="+path {
		t.Errorf("the path was changed: %q", got)
	}
}

func TestRedactor_ReadsTheFilesOfTheHostConfig(t *testing.T) {
	path := writeSecretFile(t, syntheticUniFiPassword)
	r := payload.NewRedactorFromEnvironAndFiles(nil, map[string]string{payload.UniFiPasswordName: path})
	if got := r.Redact("k=" + syntheticUniFiPassword); got != "k="+uniFiMarker {
		t.Errorf("Redact = %q, want k=%s", got, uniFiMarker)
	}
}

// Two different values under one marker (the file of the environment and the
// file of the host config, across a rotation) are both masked.
func TestRedactor_OneMarkerWithTwoFileValues(t *testing.T) {
	const rotated = "synthetic-unifi-password-rotated-fedcba9876"
	envPath := writeSecretFile(t, syntheticUniFiPassword)
	hostPath := writeSecretFile(t, rotated)
	r := payload.NewRedactorFromEnvironAndFiles(
		[]string{payload.UniFiPasswordFileEnvName + "=" + envPath},
		map[string]string{payload.UniFiPasswordName: hostPath})
	if r.ValueCount() != 2 {
		t.Fatalf("ValueCount = %d, want 2", r.ValueCount())
	}
	got := r.Redact(syntheticUniFiPassword + " " + rotated)
	if want := uniFiMarker + " " + uniFiMarker; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

// A file that the redactor cannot use gives no value, and ordinary output is
// unchanged. A short value matches at too many places in ordinary output.
func TestRedactor_UnusableCredentialFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("k", payload.MaxCredentialFileSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":         filepath.Join(dir, "missing"),
		"empty":           writeSecretFile(t, ""),
		"whitespace only": writeSecretFile(t, " \n\t\n"),
		"short":           writeSecretFile(t, "abc1234\n"),
		"directory":       dir,
		"too large":       big,
		"empty path":      "",
	}
	const ordinary = "abc1234 abc ok\nlisting a b c 1 2 3 4\n"
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			r := payload.NewRedactorFromEnviron([]string{payload.UniFiPasswordFileEnvName + "=" + path})
			if r.ValueCount() != 0 {
				t.Errorf("ValueCount = %d, want 0", r.ValueCount())
			}
			if got := r.Redact(ordinary); got != ordinary {
				t.Errorf("ordinary output changed: %q", got)
			}
		})
	}
}

func TestReadSecretFile_ErrorsHoldNoContent(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(syntheticUniFiPassword+strings.Repeat("k", payload.MaxCredentialFileSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{big, dir, filepath.Join(dir, "missing"), ""} {
		v, err := payload.ReadSecretFile(p)
		if err == nil {
			t.Errorf("ReadSecretFile(%q) = %d bytes, nil; want an error", p, len(v))
			continue
		}
		if strings.Contains(err.Error(), syntheticUniFiPassword) {
			t.Errorf("error holds the file content: %v", err)
		}
	}
	at := writeSecretFile(t, strings.Repeat("k", payload.MaxCredentialFileSize))
	if v, err := payload.ReadSecretFile(at); err != nil || len(v) != payload.MaxCredentialFileSize {
		t.Errorf("ReadSecretFile of a file at the size limit = %d bytes, %v", len(v), err)
	}
}

func TestUsableSecretValue(t *testing.T) {
	for v, want := range map[string]bool{
		syntheticUniFiPassword: true,
		"12345678":             true,
		"1234567":              false,
		"":                     false,
		"   12345   ":          false,
		"$(cat /x/y/z)":        false,
	} {
		if got := payload.UsableSecretValue(v); got != want {
			t.Errorf("UsableSecretValue(%q) = %v, want %v", v, got, want)
		}
	}
}
