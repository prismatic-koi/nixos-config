package prismcontainer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// PerSessionLimit is the number of containers and builds one session
// incarnation can run at a time.
const PerSessionLimit = 1

// DefaultHostLimit is the default number of prism containers that may run
// at a time on one host.
const DefaultHostLimit = 4

// HostLimitNixOption is the Nix option that sets the host-wide limit. The
// refusal message names it.
const HostLimitNixOption = "nx.programs.prism.containerHostLimit"

// limitRefusal lists the running containers and builds, and returns a
// refusal message when a new one goes over a limit, or "" when the request
// can continue. A *podmanError means that podman did not answer.
func limitRefusal(ctx context.Context, r Runner, instanceID string, hostLimit int) (string, error) {
	entries, err := listLabelled(ctx, r)
	if err != nil {
		return "", err
	}
	builds, err := runningBuilds()
	if err != nil {
		return "", fmt.Errorf("read the build markers: %w", err)
	}
	for _, id := range builds {
		entries = append(entries, psEntry{State: "running", Labels: map[string]string{LabelInstanceID: id}})
	}
	return checkLimits(entries, instanceID, hostLimit), nil
}

// checkLimits returns a refusal message when a new container or build goes
// over a limit, or "" when the request can continue.
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
		return fmt.Sprintf("this session already has a running container or build, and the per-session limit is %d at a time. Wait until it stops, then try again",
			PerSessionLimit)
	}
	if host >= hostLimit {
		return fmt.Sprintf("%d prism containers and builds are running on this host, and the host-wide limit is %d. The Nix option %s sets this limit. Try again later",
			host, hostLimit, HostLimitNixOption)
	}
	return ""
}

const buildMarkerSuffix = ".lock"

// buildMarker is a file that a build holds an exclusive flock on while it
// runs. A build container of podman carries no prism label and podman ps
// does not list it, so the limit check counts the held markers. The kernel
// releases the flock when the prism process ends, so a marker of a dead
// process does not count.
type buildMarker struct {
	f    *os.File
	path string
}

// newBuildSuffix returns the random part of the names of one build: its
// marker file, its image when the agent gives no tag, and its scope unit.
func newBuildSuffix() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("build name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// createBuildMarker creates and locks the marker of one build. The caller
// holds the host lock, so that the next limit check counts the marker.
//
// The file is locked before it gets its final name. A scan that finds a
// marker under its final name and gets the lock can then remove it as
// stale. The scan cannot catch a new marker in the gap between create and
// lock.
func createBuildMarker(instanceID, suffix string) (*buildMarker, error) {
	dir, err := container.PrismContainerBuildLockDirPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create build marker dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".new-")
	if err != nil {
		return nil, fmt.Errorf("create build marker: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("lock build marker: %w", err)
	}
	final := filepath.Join(dir, instanceID+"."+suffix+buildMarkerSuffix)
	if err := os.Rename(f.Name(), final); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("name build marker: %w", err)
	}
	return &buildMarker{f: f, path: final}, nil
}

// release removes the marker before it unlocks it, so that no scan sees
// an unlocked marker of a build that is still in this function.
func (m *buildMarker) release() {
	if m == nil || m.f == nil {
		return
	}
	_ = os.Remove(m.path)
	_ = m.f.Close()
	m.f = nil
}

// runningBuilds returns the instance ID of each build that runs now, one
// entry for each build. It removes the markers of builds whose prism
// process has ended.
func runningBuilds() ([]string, error) {
	dir, err := container.PrismContainerBuildLockDirPath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, buildMarkerSuffix) {
			continue
		}
		id, _, _ := strings.Cut(name, ".")
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		// Any lock error other than success counts as a running build, so
		// that an error cannot open a place over the limit.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
			_ = os.Remove(path)
		} else {
			ids = append(ids, id)
		}
		_ = f.Close()
	}
	return ids, nil
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
