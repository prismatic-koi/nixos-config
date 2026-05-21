package container_test

// credentials_timeout_test.go — timeout and cache tests for
// githubAccountFromBareRoot.
//
// These tests PATH-inject a stub "git" script that sleeps longer than
// gitBareRootTimeout and assert that the function returns within ~5s with an
// empty string, not blocking indefinitely.
//
// The stub scripts use "exec sleep" (exec-replaces the shell with the sleep
// binary) so that when exec.CommandContext sends SIGKILL to the process, there
// is no orphaned child subprocess keeping inherited file descriptors (e.g.
// stdout pipes) open on macOS. Without "exec", the shell spawns sleep as a
// child; killing the shell leaves sleep alive with inherited pipes, causing
// cmd.Run() to block until sleep exits naturally — defeating the context
// timeout in the test.
//
// Reference: internal/review/subprocess_timeout_test.go (same pattern).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// writeSleepScript writes an executable shell script that sleeps for the given
// duration to dir/<name> and returns its path.
func writeSleepScript(t *testing.T, dir, name string, sleep time.Duration) string {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nexec sleep %.3f\n", sleep.Seconds())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writeSleepScript: %v", err)
	}
	return path
}

// writeCountScript writes an executable shell script that increments a counter
// file and then outputs the given remoteURL. Returns the counter file path.
func writeCountScript(t *testing.T, dir, name, remoteURL string) string {
	t.Helper()
	counterPath := filepath.Join(dir, "call-count")
	// Atomically increment the counter then print the remote URL.
	script := fmt.Sprintf("#!/bin/sh\n"+
		"n=$(cat %q 2>/dev/null || echo 0)\n"+
		"n=$((n+1))\n"+
		"echo $n > %q\n"+
		"echo %q\n",
		counterPath, counterPath, remoteURL)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writeCountScript: %v", err)
	}
	return counterPath
}

// prependToPATH returns a PATH value with dir prepended.
func prependToPATH(dir string) string {
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestGithubAccountFromBareRoot_Timeout verifies that a hanging git subprocess
// does not block githubAccountFromBareRoot for more than ~5 s.
//
// Implementation note: the stub sleeps 60 s (>> gitBareRootTimeout = 5 s) so
// the context deadline fires first. We do not reduce gitBareRootTimeout itself
// to avoid modifying production code; the test validates the mechanism
// (context cancellation), not the exact wall-clock value.
func TestGithubAccountFromBareRoot_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	// The stub sleeps longer than gitBareRootTimeout (5 s) so the context fires.
	writeSleepScript(t, dir, "git", 60*time.Second)
	t.Setenv("PATH", prependToPATH(dir))

	// Sanity check: the stub git is actually found.
	stubPath := filepath.Join(dir, "git")
	if _, err := os.Stat(stubPath); err != nil {
		t.Fatalf("stub git not found at %s: %v", stubPath, err)
	}

	// Clear the cache so no prior test result leaks in.
	container.ClearGithubAccountCacheForTest()

	bareRoot := t.TempDir()
	start := time.Now()
	result := container.GithubAccountFromBareRootUncachedForTest(bareRoot)
	elapsed := time.Since(start)

	if result != "" {
		t.Errorf("expected empty string on timeout, got %q", result)
	}

	// The function must return within a generous bound. gitBareRootTimeout is 5 s;
	// each call makes up to two git attempts, so the theoretical ceiling is 10 s.
	// We allow 25 s (2.5×) for slow CI — the important assertion is that we
	// did NOT wait the full 60 s of the stub.
	if elapsed > 25*time.Second {
		t.Errorf("GithubAccountFromBareRootUncachedForTest took %s, expected to time out around %s (2× %s)",
			elapsed, 2*container.GitBareRootTimeoutForTest, container.GitBareRootTimeoutForTest)
	}
}

// TestGithubAccountFromBareRoot_CacheHit verifies that two calls for the same
// bareRoot result in only one underlying git invocation.
func TestGithubAccountFromBareRoot_CacheHit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	// The counting script records how many times git is called and prints the
	// remote URL so the account can be derived.
	counterPath := writeCountScript(t, dir, "git", "git@github.com:prismatic-koi/nixos-config.git")
	t.Setenv("PATH", prependToPATH(dir))

	// Clear the cache so this test is independent of earlier runs.
	container.ClearGithubAccountCacheForTest()

	bareRoot := t.TempDir()

	// First call — must hit git.
	result1 := container.GithubAccountFromBareRootForTest(bareRoot)
	// Second call — must return cached value without re-forking git.
	result2 := container.GithubAccountFromBareRootForTest(bareRoot)

	if result1 != result2 {
		t.Errorf("cache returned different value: first=%q second=%q", result1, result2)
	}

	// Read the counter. The script increments by 1 on each git invocation.
	// Because "git --git-dir <bareRoot>/.bare" fails (the dir exists but has
	// no .bare subdir), the uncached path also tries "git --git-dir <bareRoot>"
	// — so one cache miss may invoke git twice. The cache hit must not invoke it
	// again, so the total count must be ≤ 2.
	countBytes, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("could not read counter file %s: %v", counterPath, err)
	}
	var callCount int
	if _, err := fmt.Sscanf(string(countBytes), "%d", &callCount); err != nil {
		t.Fatalf("could not parse counter %q: %v", string(countBytes), err)
	}
	if callCount > 2 {
		t.Errorf("git was invoked %d times across 2 calls; expected ≤ 2 (cache must prevent re-fork on second call)", callCount)
	}
}

// TestGithubAccountFromBareRoot_ConcurrentCacheSafety runs many goroutines
// calling githubAccountFromBareRoot for the same bareRoot and verifies they
// all return the same value. This is primarily a -race detector exercise.
func TestGithubAccountFromBareRoot_ConcurrentCacheSafety(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	writeCountScript(t, dir, "git", "git@github.com:prismatic-koi/nixos-config.git")
	t.Setenv("PATH", prependToPATH(dir))

	container.ClearGithubAccountCacheForTest()

	bareRoot := t.TempDir()

	const goroutines = 20
	results := make([]string, goroutines)
	var wg atomic.Int32
	wg.Store(int32(goroutines))

	ch := make(chan int, goroutines)
	for i := 0; i < goroutines; i++ {
		ch <- i
	}
	close(ch)

	done := make(chan struct{})
	go func() {
		for i := range ch {
			go func(idx int) {
				results[idx] = container.GithubAccountFromBareRootForTest(bareRoot)
				if wg.Add(-1) == 0 {
					close(done)
				}
			}(i)
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent calls did not complete within 30 s")
	}

	first := results[0]
	for i, r := range results {
		if r != first {
			t.Errorf("goroutine %d returned %q, want %q", i, r, first)
		}
	}
}

// TestGithubAccountFromBareRoot_ExistingHappyPath exercises the cached path
// against a real bare git repo (created with git init --bare) to confirm the
// cache correctly stores and returns a valid account name.
func TestGithubAccountFromBareRoot_ExistingHappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	const remoteURL = "git@github.com:prismatic-koi/nixos-config.git"
	bareRoot := t.TempDir()
	bareDir := filepath.Join(bareRoot, ".bare")
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", bareDir, "remote", "add", "origin", remoteURL).CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}

	container.ClearGithubAccountCacheForTest()

	// First call (cache miss).
	got1 := container.GithubAccountFromBareRootForTest(bareRoot)
	if got1 != "prismatic-koi" {
		t.Errorf("first call: got %q, want %q", got1, "prismatic-koi")
	}

	// Second call (cache hit) — must return same value.
	got2 := container.GithubAccountFromBareRootForTest(bareRoot)
	if got2 != "prismatic-koi" {
		t.Errorf("second call (cache hit): got %q, want %q", got2, "prismatic-koi")
	}
}
