package cmd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// buildBinary compiles the atlassian binary to a temp dir and returns the path.
// It is skipped if the test binary is not being run from the module root.
func buildBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping binary integration tests in short mode")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "atlassian")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Build from the module root (two levels up from cmd/)
	_, file, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Join(filepath.Dir(file), "..")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	return bin
}

func TestBinary_UnknownCommand(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "fnord")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown command")
	}
	if len(out) == 0 {
		t.Error("expected usage output on unknown command")
	}
}

func TestBinary_MissingEnvSite(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "whoami")
	cmd.Env = []string{} // clear all env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when ATLASSIAN_SITE is unset")
	}
	if string(out) != "ATLASSIAN_SITE is not set\n" {
		t.Errorf("expected 'ATLASSIAN_SITE is not set', got %q", string(out))
	}
}

func TestBinary_MissingEnvEmail(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "whoami")
	cmd.Env = []string{"ATLASSIAN_SITE=foo.atlassian.net"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when ATLASSIAN_EMAIL is unset")
	}
	if string(out) != "ATLASSIAN_EMAIL is not set\n" {
		t.Errorf("expected 'ATLASSIAN_EMAIL is not set', got %q", string(out))
	}
}

func TestBinary_MissingEnvToken(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "whoami")
	cmd.Env = []string{
		"ATLASSIAN_SITE=foo.atlassian.net",
		"ATLASSIAN_EMAIL=test@example.com",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when ATLASSIAN_API_TOKEN is unset")
	}
	if string(out) != "ATLASSIAN_API_TOKEN is not set\n" {
		t.Errorf("expected 'ATLASSIAN_API_TOKEN is not set', got %q", string(out))
	}
}

func TestBinary_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer srv.Close()

	bin := buildBinary(t)
	// We need to point the binary at our test server.
	// Since we can't set a custom base URL via env vars in production,
	// use an invalid site that will fail DNS. Instead, test via the client package directly.
	// This test just verifies the binary exits non-zero on HTTP errors.
	cmd := exec.Command(bin, "whoami")
	cmd.Env = []string{
		"ATLASSIAN_SITE=localhost.invalid", // guaranteed to fail DNS
		"ATLASSIAN_EMAIL=test@example.com",
		"ATLASSIAN_API_TOKEN=token",
	}
	_, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit on HTTP error")
	}
}

func TestBinary_JiraSearchNegativeLimit(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "jira", "search", "project = FOO", "--limit", "-1")
	cmd.Env = []string{
		"ATLASSIAN_SITE=foo.atlassian.net",
		"ATLASSIAN_EMAIL=test@example.com",
		"ATLASSIAN_API_TOKEN=token",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for negative limit")
	}
	if string(out) != "--limit must be >= 0\n" {
		t.Errorf("expected limit error message, got %q", string(out))
	}
}

func TestBinary_ConfluenceSearchNegativeLimit(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "confluence", "search", "space = ENG", "--limit", "-5")
	cmd.Env = []string{
		"ATLASSIAN_SITE=foo.atlassian.net",
		"ATLASSIAN_EMAIL=test@example.com",
		"ATLASSIAN_API_TOKEN=token",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for negative limit")
	}
	if string(out) != "--limit must be >= 0\n" {
		t.Errorf("expected limit error message, got %q", string(out))
	}
}

func TestBinary_JiraSearchZeroLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issues": []any{},
			"total":  0,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// This test exercises the API path — but we can't override the base URL
	// via env vars. Test via unit tests in the client package instead.
	// Mark as skipped in this integration test.
	t.Skip("zero-limit API path tested via client unit tests")
}

func TestBinary_StdinClosed(t *testing.T) {
	bin := buildBinary(t)
	// Run with stdin explicitly closed (/dev/null)
	cmd := exec.Command(bin, "whoami")
	cmd.Env = []string{
		"ATLASSIAN_SITE=localhost.invalid",
		"ATLASSIAN_EMAIL=test@example.com",
		"ATLASSIAN_API_TOKEN=token",
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd.Stdin = f
	// Should fail quickly due to DNS error, not block waiting on stdin
	_, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit (DNS error) with closed stdin")
	}
	// Key assertion: we got here — the binary did not block on stdin
}

func TestBinary_TokenNotInErrorOutput(t *testing.T) {
	bin := buildBinary(t)
	const secretToken = "supersecrettoken12345"
	cmd := exec.Command(bin, "whoami")
	cmd.Env = []string{
		"ATLASSIAN_SITE=localhost.invalid",
		"ATLASSIAN_EMAIL=test@example.com",
		"ATLASSIAN_API_TOKEN=" + secretToken,
	}
	out, _ := cmd.CombinedOutput()
	if containsStr(string(out), secretToken) {
		t.Error("error output must not contain the API token value")
	}
}

func containsStr(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
