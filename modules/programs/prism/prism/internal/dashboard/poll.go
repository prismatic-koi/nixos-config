package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
)

// FetchSessionsFromDB queries agent_status for all active sessions and
// enriches them with git diff stats. Used by the popup dashboard (which fetches
// fresh data on every open) and by the persistent dashboard's RefreshMsg path.
func FetchSessionsFromDB() tea.Msg {
	d, err := openDB()
	if err != nil {
		// DB unavailable — return empty list rather than crashing.
		return SessionsMsg{}
	}
	defer d.Close()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return SessionsMsg{}
	}

	// Fetch group parents (group_id → parent_session) in a single batch query
	// so that StatusToAgentSession can populate ParentSession for every
	// post-migration session without N individual DB round-trips.
	groupParents, _ := d.AllGroupParents() // non-fatal; nil map falls back to name heuristic

	// Get client counts from tmux for the attachment dot indicator.
	clientCounts := TmuxClientCounts()

	sessions := make([]AgentSession, 0, len(statuses))
	for _, s := range statuses {
		sessions = append(sessions, StatusToAgentSession(s, clientCounts, groupParents))
	}

	// Filter out internal sessions (scratchpad, prism-dashboard).
	sessions = FilterAgentSessions(sessions)

	// Collect unique agent paths that need git stat computation.
	seen := map[string]bool{}
	var paths []string
	for _, s := range sessions {
		if s.AgentPath != "" && !seen[s.AgentPath] {
			seen[s.AgentPath] = true
			paths = append(paths, s.AgentPath)
		}
	}

	// Run git.Stat for each unique path concurrently.
	stats := make(map[string]GitStatResult, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			diffStat, err := git.Stat(path)
			result := GitStatResult{Stat: diffStat, Ok: err == nil}
			mu.Lock()
			stats[path] = result
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	return SessionsMsg{Sessions: sessions, GitStats: stats}
}

// FetchGitStatsOnly queries the current set of active sessions from the DB
// solely to discover their worktree paths, then runs git.Stat on each unique
// path concurrently and returns a GitStatsOnlyMsg. It does NOT update the
// session list or agent states — those are managed by FetchSessionsFromDB and
// push events. This is what the persistent dashboard's 5-second git stat ticker
// calls so that diff-counter updates never overwrite push-event state changes.
//
// Internal sessions (scratchpad, prism-dashboard) are filtered out before
// collecting paths, consistent with FetchSessionsFromDB.
func FetchGitStatsOnly() tea.Msg {
	d, err := openDB()
	if err != nil {
		return GitStatsOnlyMsg{}
	}
	defer d.Close()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return GitStatsOnlyMsg{}
	}

	// Collect unique worktree paths, skipping internal sessions
	// (scratchpad, prism-dashboard) by name — the same filter applied by
	// FilterAgentSessions. We operate directly on db.Status here to avoid
	// the TmuxClientCounts() subprocess call that StatusToAgentSession
	// requires but FetchGitStatsOnly does not use.
	seen := map[string]bool{}
	var paths []string
	for _, s := range statuses {
		if session.IsMetaSession(s.SessionName) {
			continue
		}
		if s.Worktree != "" && !seen[s.Worktree] {
			seen[s.Worktree] = true
			paths = append(paths, s.Worktree)
		}
	}

	// Run git.Stat for each unique path concurrently.
	stats := make(map[string]GitStatResult, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			diffStat, err := git.Stat(path)
			result := GitStatResult{Stat: diffStat, Ok: err == nil}
			mu.Lock()
			stats[path] = result
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	return GitStatsOnlyMsg{GitStats: stats}
}

// FetchGitHubStats calls gh api via GraphQL to get the viewer's open PR count.
// Runs as a tea.Cmd so it never blocks the render loop.
func FetchGitHubStats() tea.Msg {
	const query = `{ viewer { pullRequests(states: OPEN, first: 1) { totalCount } } }`
	out, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		return GithubStatsMsg{Err: true}
	}
	// Parse: {"data":{"viewer":{"pullRequests":{"totalCount":N}}}}
	s := string(out)
	idx := strings.Index(s, `"totalCount":`)
	if idx < 0 {
		return GithubStatsMsg{Err: true}
	}
	s = s[idx+len(`"totalCount":`):]
	end := strings.IndexAny(s, ",}")
	if end < 0 {
		return GithubStatsMsg{Err: true}
	}
	var n int
	parseGHCount(strings.TrimSpace(s[:end]), &n)
	return GithubStatsMsg{OpenPRs: n}
}

// parseGHCount extracts a non-negative decimal integer from s into out.
func parseGHCount(s string, out *int) {
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			*out = *out*10 + int(ch-'0')
		}
	}
}

// GhTick returns a tea.Cmd that fires a GhTickMsg after 60 seconds.
func GhTick() tea.Cmd {
	return tea.Tick(60*time.Second, func(t time.Time) tea.Msg {
		return GhTickMsg(t)
	})
}

// GitStatTick returns a tea.Cmd that fires a GitStatTickMsg after 5 seconds.
func GitStatTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return GitStatTickMsg(t)
	})
}

// SessionSyncTick returns a tea.Cmd that fires a SessionSyncTickMsg after 10
// seconds. The persistent dashboard uses this to periodically re-fetch the full
// session list from the DB, ensuring that sessions spawned or cleaned up since
// the last full refresh become visible (or disappear) within one tick interval.
// The interval is intentionally coarser than GitStatTick (10s vs 5s) to avoid
// unnecessary DB load; push events remain the primary mechanism for sub-second
// state-change updates.
func SessionSyncTick() tea.Cmd {
	return tea.Tick(10*time.Second, func(t time.Time) tea.Msg {
		return SessionSyncTickMsg(t)
	})
}

// DashSentinelPath returns the path to the dashboard sentinel file.
func DashSentinelPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "bus", ".dashboard.signal")
}

// DashSocketPath returns the path to the persistent dashboard Unix socket.
func DashSocketPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "bus", "dashboard.sock")
}

// WatchDashboardSentinel starts a goroutine that polls the dashboard sentinel
// file for changes and sends a RefreshMsg to the Bubble Tea program when a
// change is detected. The goroutine exits when ctx is cancelled (call the
// returned cancel function after p.Run() returns to stop it cleanly).
//
// Uses a stat-poll rather than inotify/fsnotify to avoid adding a dependency.
// The poll interval is 200ms — well under the 1-second target from the spec.
//
// This function is used by the popup dashboard only. The persistent dashboard
// uses StartSocketListener instead.
func WatchDashboardSentinel(ctx context.Context, p *tea.Program) {
	sentinelPath := DashSentinelPath()
	var lastMod time.Time
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			info, err := os.Stat(sentinelPath)
			if err == nil {
				mod := info.ModTime()
				if !mod.Equal(lastMod) {
					lastMod = mod
					p.Send(RefreshMsg{})
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// pushEvent is the JSON payload sent by the sidecar to the dashboard socket.
type pushEvent struct {
	Session string `json:"session"`
	State   string `json:"state"`
	Title   string `json:"title"`
}

// StartSocketListener creates the dashboard Unix socket, listens for push events
// from sidecars, and sends PushEventMsg to the Bubble Tea program on each event.
// The socket is created at DashSocketPath() with mode 0600. The goroutine is
// stopped and the socket removed when ctx is cancelled.
//
// Returns the net.Listener (for explicit teardown by the caller) and any error
// from socket creation. On error the caller should log and continue without
// push events — the dashboard still renders correctly via the periodic git stat
// ticker and on-demand DB refreshes; it just won't receive instant state updates.
func StartSocketListener(ctx context.Context, p *tea.Program) (net.Listener, error) {
	sockPath := DashSocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		return nil, err
	}
	// Remove any stale socket from a previous run.
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	// Restrict to owner only.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// Accept fails when the listener is closed (clean shutdown).
				return
			}
			go handlePushConn(conn, p)
		}
	}()

	// Close the listener and remove the socket when ctx is cancelled.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()

	return ln, nil
}

// handlePushConn reads a single push event from conn and sends a PushEventMsg
// to p. Fire-and-forget: errors are silently discarded.
func handlePushConn(conn net.Conn, p *tea.Program) {
	defer conn.Close()
	// Set a short read deadline so a stuck or misbehaving sidecar cannot
	// block an Accept slot indefinitely.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	// Read one line (the JSON event).
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	line := scanner.Bytes()
	var evt pushEvent
	if err := json.Unmarshal(line, &evt); err != nil {
		log.Printf("dashboard: socket: invalid push event: %v", err)
		return
	}
	if evt.Session == "" || evt.State == "" {
		return
	}
	p.Send(PushEventMsg{Session: evt.Session, State: evt.State, Title: evt.Title})
}

// RemoveStaleSocket removes the dashboard socket at DashSocketPath() if it
// exists but has no listening process. Used by prism restore to clean up
// sockets left behind by a crashed dashboard.
//
// Liveness detection: a short dial is attempted. If the dial fails (connection
// refused or timeout), the socket is stale and is removed. If the dial succeeds
// the socket is live — the connection is immediately closed; the dashboard's
// handlePushConn goroutine will receive an empty read and return silently after
// its 2-second read deadline. This is a known and accepted side-effect
// (restore runs once at login; the cost is one benign no-op goroutine).
func RemoveStaleSocket() {
	sockPath := DashSocketPath()
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		return
	}
	// Attempt a dial; if connection is refused (or any error), remove the socket.
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		_ = os.Remove(sockPath)
		return
	}
	// Socket is live — close immediately and leave the socket in place.
	_ = conn.Close()
}
