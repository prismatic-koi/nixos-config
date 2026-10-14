package prismcontainer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
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

// TestAnchorBuildName_ForgedLabels: a container that prism did not label
// with identityLabels can carry any prism.build label, so only a container
// that prism named after the label, with a suffix of newBuildSuffix, is an
// anchor. A forged label
// must not reach the path of a build marker.
func TestAnchorBuildName_ForgedLabels(t *testing.T) {
	const id = "0f0e0d0c-0b0a-4908-8706-050403020100"
	unit := buildUnitPrefix(id) + "0000beef"
	entry := func(name, label string) psEntry {
		return psEntry{Names: []string{name}, Labels: map[string]string{LabelInstanceID: id, LabelBuild: label}}
	}
	if got := anchorBuildName(entry(unit, unit)); got != id+".0000beef" {
		t.Errorf("anchorBuildName of a real anchor = %q, want %q", got, id+".0000beef")
	}
	traversal := buildUnitPrefix(id) + "x/../../../../../../tmp/p"
	for name, e := range map[string]psEntry{
		"service container with the label of a real unit": entry("prism-"+container.InstanceTokenForID(id)+"-s-01020304", unit),
		"traversal in the suffix":                         entry(traversal, traversal),
		"suffix that is not 8 hex":                        entry(buildUnitPrefix(id)+"0000BEEF", buildUnitPrefix(id)+"0000BEEF"),
		"empty suffix":                                    entry(buildUnitPrefix(id), buildUnitPrefix(id)),
		"no label":                                        entry(unit, ""),
	} {
		if got := anchorBuildName(e); got != "" {
			t.Errorf("%s: anchorBuildName = %q, want \"\"", name, got)
		}
	}
}

// TestMarkerHeld_FIFODoesNotBlock: a FIFO at a marker path does not make
// the check wait for a writer, and it counts as held.
func TestMarkerHeld_FIFODoesNotBlock(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "x.lock")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// If the open blocks, a writer releases it after the test.
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	done := make(chan bool, 1)
	go func() { done <- markerHeld(fifo) }()
	select {
	case held := <-done:
		if !held {
			t.Errorf("markerHeld of a FIFO = false, want true (fail closed)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("markerHeld of a FIFO did not return: the open waits for a writer")
	}
}

func TestBuildCPUQuotaDerivedFromCPULimit(t *testing.T) {
	if buildCPUQuota != "200000" {
		t.Fatalf("buildCPUQuota = %q for CPULimit %q", buildCPUQuota, CPULimit)
	}
}
