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
	"regexp"
	"slices"
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
	held := make(map[string]bool, len(builds))
	for _, b := range builds {
		held[b.name()] = true
		entries = append(entries, psEntry{State: "running", Labels: map[string]string{LabelInstanceID: b.instanceID}})
	}
	// The anchor container of a macOS build and its marker are one build.
	// The anchor counts only when no prism process holds the marker: then
	// a build step can still run in the VM until the anchor stops.
	counted := entries[:0]
	for _, e := range entries {
		if name := anchorBuildName(e); name == "" || !held[name] {
			counted = append(counted, e)
		}
	}
	return checkLimits(counted, instanceID, hostLimit), nil
}

// buildSuffixPattern matches a suffix that newBuildSuffix makes.
var buildSuffixPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// anchorBuildName returns the build name (buildName) of an anchor
// container, or "" when e is not an anchor.
//
// Prism sets the prism.build label of each container that it creates
// (identityLabels), so an image LABEL cannot set it there. The label alone
// still does not make an anchor, because a container that prism did not
// label in this way can carry it, for example a container that an earlier
// prism version started. Prism sets the name of every container, and it
// gives an anchor its unit name, so the name must equal the label. The
// suffix must have the form of newBuildSuffix, because the build name
// becomes the path of a build marker.
func anchorBuildName(e psEntry) string {
	unit := e.Labels[LabelBuild]
	id := e.Labels[LabelInstanceID]
	suffix, ok := strings.CutPrefix(unit, buildUnitPrefix(id))
	if !ok || !buildSuffixPattern.MatchString(suffix) || container.InstanceTokenForID(id) == "" || !slices.Contains(e.Names, unit) {
		return ""
	}
	return buildName(id, suffix)
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
		return fmt.Sprintf("this session already has a running container or build, and the per-session limit is %d at a time. "+
			"A run or a build stops by itself: wait until it stops, then try again. "+
			"A service container of prism container start runs until you stop it: run prism container list to find its handle, then prism container stop HANDLE",
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

// buildName returns the name of the marker and of the context copy of one
// build, with no file suffix.
func buildName(instanceID, suffix string) string {
	return instanceID + "." + suffix
}

// buildRef names one build that holds its marker.
type buildRef struct{ instanceID, suffix string }

func (b buildRef) name() string { return buildName(b.instanceID, b.suffix) }

// buildMarkerPath returns the path of the marker of the build name.
func buildMarkerPath(name string) (string, error) {
	dir, err := container.PrismContainerBuildLockDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+buildMarkerSuffix), nil
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
	final := filepath.Join(dir, buildName(instanceID, suffix)+buildMarkerSuffix)
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

// markerHeld reports whether a build holds the marker at path. A missing
// marker is not held. Any lock error other than success counts as held,
// so that an error cannot open a place over the limit.
func markerHeld(path string) bool {
	// O_NONBLOCK: an open of a FIFO does not wait for a writer. A file that
	// is not a regular file is not a marker that prism made, so it counts
	// as held.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return true
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
}

// removeStaleBuildCopies removes each context copy whose build marker is
// not held. A build creates its marker before its copy and releases the
// marker after it removes the copy, so a copy with no held marker belongs
// to a build whose prism process has ended. The copies are listed before
// the markers are read, so a build that starts during the scan is safe.
func removeStaleBuildCopies() {
	stageDir, err := container.PrismContainerBuildStageDirPath()
	if err != nil {
		return
	}
	lockDir, err := container.PrismContainerBuildLockDirPath()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !markerHeld(filepath.Join(lockDir, e.Name()+buildMarkerSuffix)) {
			removeBuildCopy(filepath.Join(stageDir, e.Name()))
		}
	}
}

// runningBuilds returns each build that runs now. It removes the markers
// and the context copies of builds whose prism process has ended.
func runningBuilds() ([]buildRef, error) {
	removeStaleBuildCopies()
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
	var builds []buildRef
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, buildMarkerSuffix) {
			continue
		}
		id, suffix, _ := strings.Cut(strings.TrimSuffix(name, buildMarkerSuffix), ".")
		path := filepath.Join(dir, name)
		if markerHeld(path) {
			builds = append(builds, buildRef{instanceID: id, suffix: suffix})
		} else {
			_ = os.Remove(path)
		}
	}
	return builds, nil
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
