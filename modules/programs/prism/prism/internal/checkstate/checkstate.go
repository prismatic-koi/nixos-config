// Package checkstate implements the shared discrimination between a required
// check that is still pending and one that has concluded in a failure state.
//
// It exists because two independent call sites need the exact same
// conservative classification:
//
//   - The merge-queue watcher's BLOCKED handling (internal/mergequeue), which
//     decides whether a polling row can terminate as failed (#2525).
//   - `prism merge`'s invocation-time initial-state probe (cmd/merge.go),
//     which decides whether the command should report CI failure immediately
//     instead of enqueuing a poller that can never resolve (#2527).
//
// Before this package existed, each call site carried its own copy of this
// logic. cmd/merge.go's copy (pendingRequiredCheckNames) classified any
// non-SUCCESS check — including a COMPLETED check with conclusion FAILURE —
// as merely "pending", so `prism merge` on a PR whose required check had
// already failed told the coordinator to keep waiting for something that
// could never happen. Lifting the logic here, rather than duplicating the
// fix, is deliberate: the conservative bias below (never call a check
// "failed" until the required set is fully accounted for) is the entire
// value of this code, and two copies WILL drift.
package checkstate

import "strings"

// CheckEntry is one entry from `gh pr view --json statusCheckRollup`.
//
// The rollup mixes two shapes. A modern check-run carries Status plus
// Conclusion; a legacy commit status carries only State.
type CheckEntry struct {
	// Name is the check-run name (e.g. "pr-gate") or the commit-status context.
	// GitHub's statusCheckRollup uses "name" for check-runs and "context" for
	// legacy commit statuses; callers populate both and classification falls
	// back to Context when Name is empty.
	Name       string `json:"name"`
	Context    string `json:"context"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	// State is the legacy commit-status field (e.g. "SUCCESS", "FAILURE").
	// Modern check-runs use Conclusion instead.
	State string `json:"state"`
}

// checkState is the aggregate verdict for one required check name, derived
// from every rollup entry carrying that name (#2525).
type checkState int

const (
	// checkStatePending — the check has not reached a verdict we can read:
	// queued, in progress, or an entry shape we do not recognise. A required
	// check in this state means the required set is NOT fully accounted for,
	// so no failure transition may fire.
	checkStatePending checkState = iota

	// checkStateConcluded — the check finished and did not conclude in a
	// recognised failure state. This covers SUCCESS and the neutral-ish
	// conclusions (NEUTRAL, SKIPPED, and any conclusion GitHub adds later).
	// It accounts for the check without failing it.
	checkStateConcluded

	// checkStateFailed — the check finished and concluded in a failure state.
	checkStateFailed
)

// RequiredCheckFailureConclusions is the check-run `conclusion` allowlist
// that counts as a real failure for the #2525/#2527 terminal transitions.
//
// The set is deliberately closed and deliberately small. Widening it risks
// the expensive direction of the trade-off: declaring a good PR dead.
// Conclusions NOT in this set (SUCCESS, NEUTRAL, SKIPPED, STALE,
// STARTUP_FAILURE, and anything GitHub adds later) classify as
// checkStateConcluded — they account for the check so a sibling check's
// genuine failure can still fire, but they never trigger the transition on
// their own. Add to this set only with a dedicated audit.
var RequiredCheckFailureConclusions = []string{
	"FAILURE",
	"TIMED_OUT",
	"CANCELLED",
	"ACTION_REQUIRED",
}

// LegacyStatusFailureStates is the commit-status `state` equivalent of
// RequiredCheckFailureConclusions. Legacy contexts use a separate, smaller
// enum (EXPECTED, PENDING, SUCCESS, FAILURE, ERROR) with no `status` field,
// so they need their own mapping. Without it a required legacy context that
// fails would classify as pending and reproduce the exact silent hang #2525
// and #2527 fix.
var LegacyStatusFailureStates = []string{
	"FAILURE",
	"ERROR",
}

// classifyCheckEntry maps one rollup entry to a checkState.
//
// The ordering of the cases below matters:
//
//   - A check-run whose Status is anything other than COMPLETED is pending,
//     even when a Conclusion is also present. A stale conclusion alongside a
//     re-running check must never count as a failure.
//   - Only then does a non-empty Conclusion decide the verdict.
//   - State is consulted last, for legacy contexts.
//   - An entry with none of the three populated is pending, not concluded. An
//     unreadable entry is not evidence that a check finished.
func classifyCheckEntry(c CheckEntry) checkState {
	switch {
	case c.Status != "" && !strings.EqualFold(c.Status, "COMPLETED"):
		return checkStatePending
	case c.Conclusion != "":
		if matchesAnyFold(c.Conclusion, RequiredCheckFailureConclusions) {
			return checkStateFailed
		}
		return checkStateConcluded
	case c.State != "":
		if matchesAnyFold(c.State, LegacyStatusFailureStates) {
			return checkStateFailed
		}
		if strings.EqualFold(c.State, "SUCCESS") {
			return checkStateConcluded
		}
		// PENDING, EXPECTED, or anything unrecognised: not a verdict.
		return checkStatePending
	default:
		return checkStatePending
	}
}

// matchesAnyFold reports whether s case-insensitively equals any candidate.
func matchesAnyFold(s string, candidates []string) bool {
	for _, c := range candidates {
		if strings.EqualFold(s, c) {
			return true
		}
	}
	return false
}

// combineCheckStates folds two verdicts for the same check name into one.
//
// GitHub can return more than one rollup entry per name — a re-run, or a
// check-run name that collides with a legacy status context. The precedence
// is pending > failed > concluded, which is the conservative order: a re-run
// in flight masks an older failure, so a worker who has already pushed a fix
// does not get their PR declared dead.
func combineCheckStates(a, b checkState) checkState {
	if a == checkStatePending || b == checkStatePending {
		return checkStatePending
	}
	if a == checkStateFailed || b == checkStateFailed {
		return checkStateFailed
	}
	return checkStateConcluded
}

// CheckName returns the canonical name for a rollup entry, preferring the
// check-run Name field and falling back to the legacy commit-status Context.
func CheckName(c CheckEntry) string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// FailedRequiredChecks returns the names of required checks that concluded
// in a failure state, but ONLY when the whole required set is accounted for
// (#2525, #2527). It returns nil in every other case, which callers read as
// "keep watching" / "not yet a terminal failure".
//
// nil is returned when:
//
//   - required is empty. No configured gate means no evidence of a dead end.
//   - Any required name is absent from the rollup. GitHub populates the rollup
//     for the PR's current head commit, so an absent name usually means the
//     check has not registered yet after a push.
//   - Any required name is still pending (queued, in progress, or re-running).
//   - Every required name concluded without a recognised failure.
//
// This ordering is what makes the helper safe inside the documented
// BLOCKED-to-CLEAN drift window between mergeStateStatus and
// statusCheckRollup: a stale BLOCKED reading paired with a green or
// still-filling rollup yields nil, never a failure.
//
// A residual window remains. A worker can push a fix in the gap between
// GitHub computing the rollup a caller reads and the read itself, in which
// case a failure is reported that the new push has already superseded. The
// caller should treat that as an accepted cost: a redundant but true report
// beats a silent hang.
//
// Returned names are deduplicated and follow the order of the required list,
// so notification text stays stable across repeated calls.
func FailedRequiredChecks(rollup []CheckEntry, required []string) []string {
	if len(required) == 0 {
		return nil
	}

	states := make(map[string]checkState, len(rollup))
	for _, c := range rollup {
		name := CheckName(c)
		if name == "" {
			continue
		}
		state := classifyCheckEntry(c)
		if prev, seen := states[name]; seen {
			state = combineCheckStates(prev, state)
		}
		states[name] = state
	}

	var failed []string
	emitted := make(map[string]bool, len(required))
	for _, req := range required {
		state, seen := states[req]
		if !seen || state == checkStatePending {
			// The required set is not fully accounted for. Stay silent.
			return nil
		}
		if state == checkStateFailed && !emitted[req] {
			emitted[req] = true
			failed = append(failed, req)
		}
	}
	return failed
}

// RequiredChecksAllPassed returns true iff every name in required has a
// corresponding entry in rollup with a successful conclusion.
//
// "Success" means:
//   - modern check-run: Conclusion == "SUCCESS" (case-insensitive)
//   - legacy commit-status: State == "SUCCESS" (case-insensitive)
//
// A required name that is missing from rollup entirely is treated as
// not-yet-passed and returns false.
func RequiredChecksAllPassed(rollup []CheckEntry, required []string) bool {
	if len(required) == 0 {
		// No required checks configured — treat as not-gated (conservative:
		// don't block forever, but callers should detect this upstream).
		return false
	}
	passed := make(map[string]bool, len(rollup))
	for _, c := range rollup {
		name := CheckName(c)
		if name == "" {
			continue
		}
		ok := strings.EqualFold(c.Conclusion, "SUCCESS") ||
			strings.EqualFold(c.State, "SUCCESS")
		passed[name] = ok
	}
	for _, req := range required {
		ok, found := passed[req]
		if !found || !ok {
			return false
		}
	}
	return true
}
