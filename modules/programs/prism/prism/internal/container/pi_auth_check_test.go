package container

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noAuthRetryWait makes the second read of CheckPIAuthFile immediate.
func noAuthRetryWait(t *testing.T) {
	t.Helper()
	prev := piAuthRetryWait
	piAuthRetryWait = func() {}
	t.Cleanup(func() { piAuthRetryWait = prev })
}

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return path
}

func TestCheckPIAuthFile_AcceptsRealShapes(t *testing.T) {
	for name, content := range map[string]string{
		"empty":      "",
		"empty obj":  "{}",
		"oauth":      `{"anthropic":{"type":"oauth","access":"sk-ant-a","refresh":"sk-ant-r","expires":1793088927000}}`,
		"api key":    `{"openrouter":{"type":"api_key","key":"sk-or-v1-abc"}}`,
		"env ref":    `{"openai":{"type":"api_key","key":"$OPENAI_API_KEY","env":{"A":"b"}}}`,
		"bom":        "\xef\xbb\xbf{}",
		"big number": `{"x":{"type":"oauth","access":"a","refresh":"r","expires":1e400}}`,
		"inner bang": `{"openrouter":{"type":"api_key","key":"sk-or-!-abc"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckPIAuthFile(writeAuthFile(t, content)); err != nil {
				t.Fatalf("CheckPIAuthFile = %v, want nil", err)
			}
		})
	}
	if err := CheckPIAuthFile(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Errorf("CheckPIAuthFile(missing) = %v, want nil", err)
	}
	if err := CheckPIAuthFile(""); err != nil {
		t.Errorf("CheckPIAuthFile(\"\") = %v, want nil", err)
	}
}

// pi runs an api_key value that starts with "!". The error names the file
// and the provider, and holds no value from the file.
func TestCheckPIAuthFile_RefusesCommandValue(t *testing.T) {
	const secret = "touch /tmp/planted-3284"
	for name, content := range map[string]string{
		"api key":       `{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1},"openai":{"type":"api_key","key":"!` + secret + `"}}`,
		"escaped bang":  `{"openai":{"type":"api_key","key":"\u0021` + secret + `"}}`,
		"nested":        `{"openai":{"type":"api_key","env":{"X":"!` + secret + `"}}}`,
		"duplicate key": `{"openai":{"type":"api_key","key":"safe","key":"!` + secret + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeAuthFile(t, content)
			err := CheckPIAuthFile(path)
			if err == nil {
				t.Fatal("CheckPIAuthFile = nil, want an error")
			}
			for _, want := range []string{path, `"openai"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "sk-") {
				t.Errorf("error %q holds a value from the file", err)
			}
		})
	}
}

// JSON.parse in pi can accept a file that Go cannot decode, so the check
// refuses such a file.
func TestCheckPIAuthFile_FailsClosed(t *testing.T) {
	noAuthRetryWait(t)
	deep := strings.Repeat("[", 10001) + strings.Repeat("]", 10001)
	for name, content := range map[string]string{
		"not json":        "not json",
		"null":            "null",
		"array":           "[]",
		"trailing data":   `{} {"openai":{"type":"api_key","key":"!x"}}`,
		"deep nesting":    `{"openai":{"type":"api_key","key":"!x","n":` + deep + `}}`,
		"only whitespace": "  \n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckPIAuthFile(writeAuthFile(t, content)); err == nil {
				t.Fatal("CheckPIAuthFile = nil, want an error")
			}
		})
	}
	big := writeAuthFile(t, `{"x":"`+strings.Repeat("a", piAuthFileLimit)+`"}`)
	if err := CheckPIAuthFile(big); err == nil {
		t.Error("CheckPIAuthFile = nil for a file over the limit, want an error")
	}
}

func TestCheckPIAuthFile_RefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- CheckPIAuthFile(path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("CheckPIAuthFile = nil for a FIFO, want an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CheckPIAuthFile blocked on a FIFO")
	}
}

func TestPIHostAuthPath(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/run/prism/pi-agent")
	if got := PIHostAuthPath(); got != "/run/prism/pi-agent/auth.json" {
		t.Errorf("PIHostAuthPath = %q with PI_CODING_AGENT_DIR set", got)
	}
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("HOME", home)
	if got, want := PIHostAuthPath(), filepath.Join(home, ".pi", "agent", "auth.json"); got != want {
		t.Errorf("PIHostAuthPath = %q, want %q", got, want)
	}
}

// pi writes auth.json in place, so a read can see a partial file. The check
// reads once more before it refuses the start.
func TestCheckPIAuthFile_RetriesAnUndecodableRead(t *testing.T) {
	path := writeAuthFile(t, `{"anthropic":{"type":"oauth","acc`)
	prev := piAuthRetryWait
	waits := 0
	piAuthRetryWait = func() {
		waits++
		if err := os.WriteFile(path, []byte(`{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { piAuthRetryWait = prev })

	if err := CheckPIAuthFile(path); err != nil {
		t.Fatalf("CheckPIAuthFile = %v after the write finished, want nil", err)
	}
	if waits != 1 {
		t.Errorf("retry waits = %d, want 1", waits)
	}

	// A command value is refused at once, with no retry.
	waits = 0
	if err := os.WriteFile(path, []byte(`{"openai":{"type":"api_key","key":"!x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPIAuthFile(path); err == nil {
		t.Fatal("CheckPIAuthFile = nil for a command value")
	}
	if waits != 0 {
		t.Errorf("retry waits = %d for a command value, want 0", waits)
	}
}
