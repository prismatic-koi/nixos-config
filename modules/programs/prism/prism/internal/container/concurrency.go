package container

// concurrency.go — soft concurrency cap for prism-managed agent containers.
//
// "In-flight" is defined as the union of:
//   - Sessions with an agent_status row where ended_at IS NULL (DB source)
//   - Live podman ps containers whose name matches the "prism-" prefix (podman source)
//
// The two sources are deduplicated by container name so that a session tracked
// in both the DB and podman is counted exactly once.
//
// When podman ps fails the check falls back to DB-only with a warning.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

const (
	// DefaultConcurrencyCap is the maximum number of in-flight agent containers
	// before new spawns are refused. Chosen for a 32 GB host where
	// nixos-config@main (coordinator) and obsidian@main are typically always
	// running, leaving 4 slots for real workers.
	DefaultConcurrencyCap = 6
)

// InFlightSession describes a single in-flight agent container.
type InFlightSession struct {
	// Name is the prism session name (e.g. "nixos-config@feature").
	Name string
	// Role is the inferred role ("coordinator", "worker", or "unknown").
	// Derived from root_agent_name in the DB when available, or inferred
	// from the session name heuristic ("@main" → coordinator).
	Role string
}

// roleFor infers the role label for display. It uses rootAgentName from the DB
// when available; otherwise it falls back to a session-name heuristic.
func roleFor(sessionName string, rootAgentName *string) string {
	if rootAgentName != nil && *rootAgentName != "" {
		return *rootAgentName
	}
	// Heuristic: sessions on the main branch are coordinators.
	if strings.HasSuffix(sessionName, "@main") {
		return "coordinator"
	}
	return "unknown"
}

// ListInFlight returns the deduplicated set of in-flight prism agent containers,
// merging the DB view (ended_at IS NULL rows) with podman ps output.
//
// dbPath is the path to prism.db. podmanFallbackWarning is set to true when
// podman ps failed and the list is DB-only (potentially imprecise).
//
// The podmanPS parameter, when non-nil, overrides the real podman ps call —
// used exclusively by tests to inject fake output without executing podman.
func ListInFlight(dbPath string, podmanPS func() ([]string, bool)) ([]InFlightSession, bool) {
	// Step 1: collect sessions from the DB (ended_at IS NULL).
	dbSessions := map[string]InFlightSession{}
	if d, err := db.Open(dbPath); err == nil {
		if statuses, err := d.AllActiveStatus(); err == nil {
			for _, s := range statuses {
				name := s.SessionName
				dbSessions[NameForSession(name)] = InFlightSession{
					Name: name,
					Role: roleFor(name, s.RootAgentName),
				}
			}
		}
		d.Close()
	}

	// Step 2: collect live containers from podman ps.
	// containerName → sessionName (for display)
	podmanSessions := map[string]InFlightSession{}
	podmanFailed := false

	var podmanNames []string
	if podmanPS != nil {
		// Test injection.
		names, ok := podmanPS()
		podmanNames = names
		podmanFailed = !ok
	} else {
		var ok bool
		podmanNames, ok = runPodmanPS()
		podmanFailed = !ok
	}

	for _, cname := range podmanNames {
		if !strings.HasPrefix(cname, "prism-") {
			continue
		}
		// Only add if not already in DB map (DB is authoritative for name/role).
		if _, found := dbSessions[cname]; !found {
			podmanSessions[cname] = InFlightSession{
				Name: cname, // can't reverse the name; use container name as display
				Role: "unknown",
			}
		}
	}

	// Step 3: merge, deduplicating by container name.
	seen := map[string]bool{}
	var result []InFlightSession

	for cname, s := range dbSessions {
		if seen[cname] {
			continue
		}
		seen[cname] = true
		result = append(result, s)
	}
	for cname, s := range podmanSessions {
		if seen[cname] {
			continue
		}
		seen[cname] = true
		result = append(result, s)
	}

	return result, podmanFailed
}

// runPodmanPS executes `podman ps --format {{.Names}}` and returns the
// container name list. Returns (nil, false) when podman ps fails.
func runPodmanPS() ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, false
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, true
}

// CheckResult is the outcome of a concurrency cap check.
type CheckResult struct {
	// Count is the number of in-flight containers at check time.
	Count int
	// Cap is the cap that was applied.
	Cap int
	// Exceeded is true when Count >= Cap.
	Exceeded bool
	// InFlight is the full list of in-flight sessions at check time.
	InFlight []InFlightSession
	// PodmanFailed is true when podman ps failed and the count is DB-only.
	PodmanFailed bool
}

// CheckCap checks the current in-flight container count against cap.
// dbPath is the path to prism.db. podmanPS, when non-nil, overrides the real
// podman ps call (for tests).
func CheckCap(dbPath string, cap int, podmanPS func() ([]string, bool)) CheckResult {
	inFlight, podmanFailed := ListInFlight(dbPath, podmanPS)
	return CheckResult{
		Count:        len(inFlight),
		Cap:          cap,
		Exceeded:     len(inFlight) >= cap,
		InFlight:     inFlight,
		PodmanFailed: podmanFailed,
	}
}

// FormatExceededError formats the error message shown when the cap is reached.
func FormatExceededError(res CheckResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "error: prism concurrency cap reached (%d agent containers already in flight)\n", res.Count)
	sb.WriteString("\nActive containers:\n")
	for _, s := range res.InFlight {
		fmt.Fprintf(&sb, "  %-40s (%s)\n", s.Name, s.Role)
	}
	sb.WriteString("\nHint: wait for a worker to finish and be cleaned up, or re-run with\n")
	sb.WriteString("      --ignore-concurrency-cap to bypass this guard.")
	return sb.String()
}

// FormatExceededWarning formats the warning written to stderr when
// --ignore-concurrency-cap is passed and the cap is exceeded.
func FormatExceededWarning(res CheckResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[prism] warning: concurrency cap exceeded (%d/%d in-flight containers) — proceeding because --ignore-concurrency-cap was passed\n", res.Count, res.Cap)
	sb.WriteString("[prism] in-flight containers:\n")
	for _, s := range res.InFlight {
		fmt.Fprintf(&sb, "[prism]   %-40s (%s)\n", s.Name, s.Role)
	}
	return sb.String()
}
