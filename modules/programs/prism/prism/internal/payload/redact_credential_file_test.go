package payload_test

// redact_credential_file_test.go — the value layer learns a credential that a
// session gets as a file. No environment variable holds the value: a path
// variable holds the path of the file, and CredentialFilesEnvName lists each
// path variable with the marker name of its credential. The redactor reads
// the file, and the marker names the credential.
//
// SECURITY: every value in this file is synthetic.

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

const (
	syntheticFilePassword = "synthetic-credential-file-password-0123456789abcdef"
	examplePasswordEnv    = "EXAMPLE_PASSWORD_FILE"
	examplePasswordMarker = "EXAMPLE_PASSWORD"
)

var exampleMarker = payload.RedactionMarker(examplePasswordMarker)

// exampleRegistry is the registry variable of a session with the example
// file variable.
var exampleRegistry = payload.CredentialFilesEnvName + "=" + examplePasswordEnv + "=" + examplePasswordMarker

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "example_password")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return p
}

// credentialFilesCasesPath is the registry case file that both suites run.
const credentialFilesCasesPath = "testdata/credential-files-cases.json"

type credentialFilesCase struct {
	Name  string            `json:"name"`
	Value string            `json:"value"`
	Want  map[string]string `json:"want"`
}

func loadCredentialFilesCases(t *testing.T) []credentialFilesCase {
	t.Helper()
	b, err := os.ReadFile(credentialFilesCasesPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []credentialFilesCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("parse %s: %v", credentialFilesCasesPath, err)
	}
	if len(file.Cases) == 0 {
		t.Fatalf("%s holds no cases", credentialFilesCasesPath)
	}
	return file.Cases
}

// TestParseCredentialFiles_SharedCases runs the shared registry case file.
// prism.credential-file-redaction.test.ts runs the same file against the
// extension, so a pass on both sides means that the two parse each value in
// the same way.
func TestParseCredentialFiles_SharedCases(t *testing.T) {
	for _, tc := range loadCredentialFilesCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := payload.ParseCredentialFiles(tc.Value); !maps.Equal(got, tc.Want) {
				t.Errorf("ParseCredentialFiles(%q) = %v, want %v", tc.Value, got, tc.Want)
			}
		})
	}
}

func TestEncodeCredentialFiles(t *testing.T) {
	registry := map[string]string{"B_TOKEN_FILE": "B_TOKEN", "A_PASSWORD_FILE": "A_PASSWORD", "bad name": "X", "C_FILE": "bad marker"}
	got := payload.EncodeCredentialFiles(registry)
	if want := "A_PASSWORD_FILE=A_PASSWORD,B_TOKEN_FILE=B_TOKEN"; got != want {
		t.Errorf("EncodeCredentialFiles = %q, want %q", got, want)
	}
	if back := payload.ParseCredentialFiles(got); !maps.Equal(back, map[string]string{"A_PASSWORD_FILE": "A_PASSWORD", "B_TOKEN_FILE": "B_TOKEN"}) {
		t.Errorf("the encoded registry parses to %v", back)
	}
	if got := payload.EncodeCredentialFiles(nil); got != "" {
		t.Errorf("EncodeCredentialFiles(nil) = %q, want empty", got)
	}
}

// Neither the registry variable nor a path variable is a credential name:
// they hold names and paths, not secrets.
func TestCredentialFiles_RegistryAndPathAreNotSecrets(t *testing.T) {
	for _, name := range []string{payload.CredentialFilesEnvName, examplePasswordEnv} {
		if payload.IsCredentialEnvName(name) {
			t.Errorf("IsCredentialEnvName(%s) = true, want false", name)
		}
	}
}

// The tool result of `cat "$EXAMPLE_PASSWORD_FILE"`, of `env` after an export
// of the value, of a login body, and the JSON form of a frame all show the
// marker.
func TestRedactor_ReadsTheFileThatTheRegistryNames(t *testing.T) {
	path := writeSecretFile(t, syntheticFilePassword+"\n")
	r := payload.NewRedactorFromEnviron([]string{exampleRegistry, examplePasswordEnv + "=" + path})
	if r.ValueCount() != 1 {
		t.Fatalf("ValueCount = %d, want 1", r.ValueCount())
	}
	for _, in := range []string{
		syntheticFilePassword + "\n",
		"HOME=/home/x\nPW=" + syntheticFilePassword + "\n" + examplePasswordEnv + "=" + path + "\n",
		`{"username":"agent","password":"` + syntheticFilePassword + `","remember":false}`,
	} {
		got := r.Redact(in)
		if strings.Contains(got, syntheticFilePassword) {
			t.Errorf("the value survived redaction: %q", got)
		}
		if !strings.Contains(got, exampleMarker) {
			t.Errorf("Redact(%q) = %q, want the marker %s", in, got, exampleMarker)
		}
	}
	doc, _ := json.Marshal(map[string]string{"output": "password " + syntheticFilePassword})
	if got := r.RedactJSON(string(doc)); strings.Contains(got, syntheticFilePassword) || !strings.Contains(got, exampleMarker) {
		t.Errorf("RedactJSON = %q, want the marker in place of the value", got)
	}
	// The path is not a secret, so the output keeps it.
	if got := r.Redact(examplePasswordEnv + "=" + path); got != examplePasswordEnv+"="+path {
		t.Errorf("the path was changed: %q", got)
	}
}

// The redactor reads only the files of the variables that the registry
// lists. With no registry, a path variable is a plain variable.
func TestRedactor_ReadsNoFileThatTheRegistryDoesNotName(t *testing.T) {
	path := writeSecretFile(t, syntheticFilePassword)
	for name, environ := range map[string][]string{
		"no registry":                  {examplePasswordEnv + "=" + path},
		"registry names another":       {payload.CredentialFilesEnvName + "=OTHER_FILE=OTHER", examplePasswordEnv + "=" + path},
		"registry entry is not a name": {payload.CredentialFilesEnvName + "=" + examplePasswordEnv + "=bad marker", examplePasswordEnv + "=" + path},
		"listed variable not set":      {exampleRegistry},
	} {
		t.Run(name, func(t *testing.T) {
			r := payload.NewRedactorFromEnviron(environ)
			if r.ValueCount() != 0 || r.Redact(syntheticFilePassword) != syntheticFilePassword {
				t.Errorf("ValueCount = %d; the redactor read a file that the registry does not name", r.ValueCount())
			}
		})
	}
}

// Each listed file has its own marker.
func TestRedactor_TwoCredentialFiles(t *testing.T) {
	const second = "synthetic-second-credential-token-fedcba9876"
	first := writeSecretFile(t, syntheticFilePassword)
	other := writeSecretFile(t, second)
	r := payload.NewRedactorFromEnviron([]string{
		payload.CredentialFilesEnvName + "=" + examplePasswordEnv + "=" + examplePasswordMarker + ",SECOND_TOKEN_FILE=SECOND_TOKEN",
		examplePasswordEnv + "=" + first,
		"SECOND_TOKEN_FILE=" + other,
	})
	if got, want := r.Redact(syntheticFilePassword+" "+second), exampleMarker+" [redacted:SECOND_TOKEN]"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestRedactor_ReadsTheFilesOfTheHostConfig(t *testing.T) {
	path := writeSecretFile(t, syntheticFilePassword)
	r := payload.NewRedactorFromEnvironAndFiles(nil, []payload.CredentialFile{{Marker: examplePasswordMarker, Path: path}})
	if got := r.Redact("k=" + syntheticFilePassword); got != "k="+exampleMarker {
		t.Errorf("Redact = %q, want k=%s", got, exampleMarker)
	}
}

// Two different values under one marker (the file of the environment and the
// file of the host config, across a rotation) are both masked.
func TestRedactor_OneMarkerWithTwoFileValues(t *testing.T) {
	const rotated = "synthetic-credential-file-rotated-fedcba9876"
	envPath := writeSecretFile(t, syntheticFilePassword)
	hostPath := writeSecretFile(t, rotated)
	files := []payload.CredentialFile{{Marker: examplePasswordMarker, Path: hostPath}}
	r := payload.NewRedactorFromEnvironAndFiles([]string{exampleRegistry, examplePasswordEnv + "=" + envPath}, files)
	if r.ValueCount() != 2 {
		t.Fatalf("ValueCount = %d, want 2", r.ValueCount())
	}
	got := r.Redact(syntheticFilePassword + " " + rotated)
	if want := exampleMarker + " " + exampleMarker; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
	if len(files) != 1 {
		t.Errorf("NewRedactorFromEnvironAndFiles changed the slice of the caller: %d entries", len(files))
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
			r := payload.NewRedactorFromEnviron([]string{exampleRegistry, examplePasswordEnv + "=" + path})
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
	if err := os.WriteFile(big, []byte(syntheticFilePassword+strings.Repeat("k", payload.MaxCredentialFileSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{big, dir, filepath.Join(dir, "missing"), ""} {
		v, err := payload.ReadSecretFile(p)
		if err == nil {
			t.Errorf("ReadSecretFile(%q) = %d bytes, nil; want an error", p, len(v))
			continue
		}
		if strings.Contains(err.Error(), syntheticFilePassword) {
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
		syntheticFilePassword: true,
		"12345678":            true,
		"1234567":             false,
		"":                    false,
		"   12345   ":         false,
		"$(cat /x/y/z)":       false,
	} {
		if got := payload.UsableSecretValue(v); got != want {
			t.Errorf("UsableSecretValue(%q) = %v, want %v", v, got, want)
		}
	}
}
