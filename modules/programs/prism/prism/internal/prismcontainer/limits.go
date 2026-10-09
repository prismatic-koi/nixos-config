package prismcontainer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// PerSessionLimit is the number of containers one session incarnation may
// run at a time.
const PerSessionLimit = 1

// DefaultHostLimit is the default number of prism containers that may run
// at a time on one host.
const DefaultHostLimit = 4

// HostLimitNixOption is the Nix option that sets the host-wide limit. The
// refusal message names it.
const HostLimitNixOption = "nx.programs.prism.containerHostLimit"

// checkLimits returns a refusal message when a new container goes over a
// limit, or "" when the request can continue.
func checkLimits(entries []psEntry, instanceID string, hostLimit int) string {
	var session, host int
	for _, e := range entries {
		if !e.active() {
			continue
		}
		host++
		if e.Labels[LabelInstanceID] == instanceID {
			session++
		}
	}
	if session >= PerSessionLimit {
		return fmt.Sprintf("this session already has a running container, and the per-session limit is %d container at a time. Wait for that container to exit, then try again",
			PerSessionLimit)
	}
	if host >= hostLimit {
		return fmt.Sprintf("%d prism containers are running on this host, and the host-wide limit is %d. The Nix option %s sets this limit. Try again later",
			host, hostLimit, HostLimitNixOption)
	}
	return ""
}

// hostLock serialises the limit check and the container create across all
// prism processes on the host, so two sessions cannot both pass the check
// for the last free place.
type hostLock struct{ f *os.File }

// acquireHostLock takes an exclusive flock on path. It returns an error when
// ctx ends or wait passes before the lock is free.
func acquireHostLock(ctx context.Context, path string, wait time.Duration) (*hostLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &hostLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("the host-wide container lock %s stayed busy for %s", path, wait)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (l *hostLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
