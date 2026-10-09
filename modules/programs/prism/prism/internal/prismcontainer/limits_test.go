package prismcontainer

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostLock_Exclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-limit.lock")
	first, err := acquireHostLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := acquireHostLock(context.Background(), path, 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "stayed busy") {
		t.Fatalf("second acquire while held: err = %v, want busy", err)
	}
	first.release()
	second, err := acquireHostLock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.release()
}

func TestTailBuffer(t *testing.T) {
	b := newTailBuffer(8)
	for _, s := range []string{"abc", "def", "ghij"} {
		_, _ = b.Write([]byte(s))
	}
	got, dropped := b.snapshot()
	if string(got) != "cdefghij" || dropped != 2 {
		t.Errorf("snapshot = %q, %d; want cdefghij, 2", got, dropped)
	}
	_, _ = b.Write(bytes.Repeat([]byte("z"), 20))
	got, dropped = b.snapshot()
	if string(got) != "zzzzzzzz" || dropped != 22 {
		t.Errorf("after a large write: %q, %d; want 8 z and 22", got, dropped)
	}
}
