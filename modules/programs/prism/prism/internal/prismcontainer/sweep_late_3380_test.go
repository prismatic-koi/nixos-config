package prismcontainer_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// lateRegistration is a podman that registers a container of the session
// after a `podman ps`, as podman does for a container that a podman client
// in progress hands to conmon. Each sets how many lists it follows.
type lateRegistration struct {
	*prismcontainertest.Fake
	mu    sync.Mutex
	lists int
	each  int
}

func (l *lateRegistration) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	code, err := l.Fake.Run(ctx, stdout, stderr, args...)
	if len(args) > 0 && args[0] == "ps" {
		l.mu.Lock()
		l.lists++
		if l.lists <= l.each {
			l.Fake.Add(prismcontainertest.Container{ID: fmt.Sprintf("late%d", l.lists), State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
		}
		l.mu.Unlock()
	}
	return code, err
}

// AC (#3380): a container that podman registers after the first container
// list of the sweep is removed before the sweep returns.
func TestSweepSession_RemovesContainerRegisteredAfterTheFirstList_3380(t *testing.T) {
	f := &prismcontainertest.Fake{}
	f.Add(prismcontainertest.Container{ID: "early", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	r := &lateRegistration{Fake: f, each: 1}

	if err := prismcontainer.SweepSession(context.Background(), r, f, []string{testInstanceID}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("containers left = %+v, want none", left)
	}
}

// A container that appears after each list shows a process that still
// creates containers of the session. The sweep returns an error, so that
// a conversion does not go on.
func TestSweepSession_ContainerAfterEachListIsAnError_3380(t *testing.T) {
	f := &prismcontainertest.Fake{}
	r := &lateRegistration{Fake: f, each: 1 << 30}

	if err := prismcontainer.SweepSession(context.Background(), r, f, []string{testInstanceID}); err == nil {
		t.Fatal("SweepSession = nil, want an error while containers keep appearing")
	}
}
