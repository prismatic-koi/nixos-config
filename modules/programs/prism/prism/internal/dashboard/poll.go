package dashboard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/git"
)

// FetchSessionsFromDB queries agent_status for all active sessions and
// enriches them with git diff stats.
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

	// Get client counts from tmux for the attachment dot indicator.
	clientCounts := TmuxClientCounts()

	sessions := make([]AgentSession, 0, len(statuses))
	for _, s := range statuses {
		sessions = append(sessions, StatusToAgentSession(s, clientCounts))
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
	_, _ = strings.NewReader(strings.TrimSpace(s[:end])), &n
	// Use fmt.Sscanf for the parse (mirrors original).
	parseGHCount(strings.TrimSpace(s[:end]), &n)
	return GithubStatsMsg{OpenPRs: n}
}

// parseGHCount extracts an integer from a string into out.
// Separated from FetchGitHubStats so fmt can be imported once.
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

// DashSentinelPath returns the path to the dashboard sentinel file.
func DashSentinelPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "bus", ".dashboard.signal")
}

// WatchDashboardSentinel starts a goroutine that polls the dashboard sentinel
// file for changes and sends a RefreshMsg to the Bubble Tea program when a
// change is detected. The goroutine exits when ctx is cancelled (call the
// returned cancel function after p.Run() returns to stop it cleanly).
//
// Uses a stat-poll rather than inotify/fsnotify to avoid adding a dependency.
// The poll interval is 200ms — well under the 1-second target from the spec.
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
