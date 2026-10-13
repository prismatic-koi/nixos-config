package session

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestReadPIDArgv_ProcCmdline(t *testing.T) {
	cases := []struct {
		desc    string
		cmdline string
		want    []string
	}{
		{"sidecar", "/bin/prism\x00sidecar\x00--session\x00a@b\x00", []string{"/bin/prism", "sidecar", "--session", "a@b"}},
		{"empty command line (kernel thread)", "", []string{""}},
	}
	for i, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			root := t.TempDir()
			pid := 4_000_000 + i
			dir := filepath.Join(root, strconv.Itoa(pid))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir fake proc: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(c.cmdline), 0o644); err != nil {
				t.Fatalf("write fake cmdline: %v", err)
			}
			prev := procRoot
			procRoot = root
			t.Cleanup(func() { procRoot = prev })

			got, err := readPIDArgv(pid)
			if err != nil {
				t.Fatalf("readPIDArgv: %v", err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("readPIDArgv = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReadPIDArgv_MissingProcEntry(t *testing.T) {
	prev := procRoot
	procRoot = t.TempDir()
	t.Cleanup(func() { procRoot = prev })

	if _, err := readPIDArgv(4_000_100); err == nil {
		t.Error("readPIDArgv of a PID with no /proc entry: err = nil, want an error")
	}
}
