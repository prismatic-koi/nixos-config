package cmd

// Active refresh for `prism account usage` (issue #2541, parent #2537).
//
// `prism account usage` reads a passively captured snapshot (#2538, #2539).
// A snapshot only exists once a session has actually talked to Anthropic, so
// an account nobody has used recently shows nothing at all. This file fills
// that gap: when the active account's snapshot is missing or stale, the
// command makes ONE live request and prints what comes back.
//
// Three constraints shape everything below.
//
// # 1. The refresh can only run host-side
//
// The request needs a bearer token, and the token lives in
// ~/.config/prism/accounts/. That directory is deliberately NOT bound into an
// agent sandbox (internal/container/mounts.go scopes the neighbouring
// profiles.json mount to a single file precisely to keep it out), so a
// sandboxed invocation cannot read it. Most sessions are sandboxed, so this
// is the common path, not the exotic one — refreshUnavailable reports it and
// the command falls back to stored data.
//
// # 2. The sidecar owns the write
//
// A successful refresh is persisted by POSTing to /usage/snapshot, the
// endpoint #2538 defines. Nothing here writes a snapshot file. The sidecar
// resolves the account host-side at write time, which is what keeps
// attribution correct when the user switches accounts.
//
// # 3. A refresh failure must never lose data
//
// Every failure branch degrades to stored-data-plus-a-warning and exits 0.
// A user reading a usage display that shows nothing cannot tell "no quota
// used" from "the tool broke", so stale-with-a-warning always wins.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/prismatic-koi/prism/internal/account"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/usage"
)

// usageSnapshotEndpoint is the sidecar host-API path #2538 defines. Shared
// with the pi extension's ratelimit.ts::USAGE_SNAPSHOT_PATH.
const usageSnapshotEndpoint = "/usage/snapshot"

// sidecarDialTimeout bounds each probe of a candidate sidecar socket. Kept
// short — matching internal/session's own liveness probe — so a wedged socket
// cannot stall an interactive command.
const sidecarDialTimeout = 250 * time.Millisecond

// refreshOutcome is what a refresh attempt produced. Exactly one of Snapshot
// and Warning is meaningful:
//
//   - Snapshot non-nil: the request succeeded and returned usable headers.
//   - Warning non-empty: nothing was refreshed and this text explains why.
//
// Both can be set at once in one case: the request succeeded but the sidecar
// POST did not, so the numbers are displayable but were not persisted.
type refreshOutcome struct {
	// Account is the account the refresh targeted, "" when none was resolved.
	Account string
	// Snapshot is the freshly fetched snapshot, ready to display.
	Snapshot *usage.Snapshot
	// Warning is a single line for stderr. Never carries token material.
	Warning string
}

// maybeRefresh decides whether to refresh, does so at most once, and reports
// the result.
//
// rows is what `usage.ReadAll` returned (possibly empty, when the usage
// directory does not exist). now is the clock.
//
// It returns a zero refreshOutcome when no refresh was needed — a snapshot
// newer than usage.StaleAfter exists for the active account, so no request is
// made at all.
//
// AT MOST ONE REQUEST. There is exactly one target (the active account) and
// exactly one call to Refresh, with no retry. The command's whole purpose is
// to report quota consumption, so spending more of it than necessary to do so
// would be self-defeating.
//
// Order of operations is deliberate: the freshness question is answered
// BEFORE any "cannot refresh" reason is reported. A sandboxed session with a
// fresh snapshot needs no refresh, so telling it that a refresh is impossible
// would be noise on every single invocation.
func maybeRefresh(ctx context.Context, rows []usage.AccountSnapshot, now time.Time) refreshOutcome {
	unavailable := refreshUnavailable()

	var name string
	var nameErr error
	if unavailable == "" {
		name, nameErr = activeAccountName()
	}
	if name == "" {
		// Fall back to the account named in current.json. That file lives in
		// the usage directory, which IS readable inside a sandbox, so this is
		// how a sandboxed invocation still knows which row to judge for
		// staleness.
		name = activeRowName(rows)
	}

	if name != "" && !needsRefresh(rows, name, now) {
		return refreshOutcome{}
	}

	if unavailable != "" {
		return refreshOutcome{Warning: "usage refresh skipped: " + unavailable}
	}
	if nameErr != nil {
		return refreshOutcome{Warning: "usage refresh skipped: " + nameErr.Error()}
	}

	creds, err := readActiveCredentials(name)
	if err != nil {
		return refreshOutcome{Account: name, Warning: "usage refresh skipped: " + err.Error()}
	}

	// Pre-flight expiry check. Catching this here rather than letting the API
	// answer with a 401 spends no quota and gives the user the same
	// instruction one round-trip sooner.
	if creds.Expired(now) {
		return refreshOutcome{
			Account: name,
			Warning: fmt.Sprintf(
				"usage refresh failed: the access token for account %q has expired — run `prism account login %s`",
				name, name),
		}
	}

	refresher := &usage.Refresher{BaseURL: usage.BaseURLFromEnv()}
	payload, err := refresher.Refresh(ctx, creds.Access)
	if err != nil {
		return refreshOutcome{
			Account: name,
			Warning: describeRefreshError(name, err),
		}
	}

	snap := payload.ToSnapshot(name, now)
	out := refreshOutcome{Account: name, Snapshot: &snap}

	// Persist through the sidecar. A delivery failure is NOT a refresh
	// failure: the numbers in hand are good, so they are still displayed —
	// they just did not reach disk, and the warning says so.
	if err := persistSnapshot(payload); err != nil {
		out.Warning = "usage refresh: snapshot not persisted: " + err.Error()
	}
	return out
}

// activeRowName returns the name of the row marked active by current.json, or
// "" when no row is marked.
func activeRowName(rows []usage.AccountSnapshot) string {
	for _, row := range rows {
		if row.Active {
			return row.Name
		}
	}
	return ""
}

// describeRefreshError turns a Refresher error into one user-facing line.
//
// Every branch names the specific failure. None of them carries the token,
// the request headers, or the response body.
func describeRefreshError(accountName string, err error) string {
	switch {
	case errors.Is(err, usage.ErrTokenRejected):
		// The stored token is expired or revoked. Same instruction as the
		// pre-flight branch, reached when the local expiry said the token was
		// still good but the server disagreed.
		return fmt.Sprintf(
			"usage refresh failed: the access token for account %q was rejected (HTTP 401) — run `prism account login %s`",
			accountName, accountName)

	case errors.Is(err, usage.ErrNoRateLimitHeaders):
		// A 200 that told us nothing. Persisting an empty snapshot would
		// clobber a good one, so the stored copy is left byte-identical.
		return "usage refresh: the response carried no rate-limit headers — the stored snapshot is unchanged"
	}

	var statusErr *usage.StatusError
	if errors.As(err, &statusErr) {
		msg := fmt.Sprintf(
			"usage refresh failed: HTTP %d from the Anthropic API — the stored snapshot is unchanged",
			statusErr.StatusCode)
		if statusErr.StatusCode == 429 {
			// #2537: a malformed OAuth request is rejected by Anthropic's WAF
			// with a 429 that carries no rate-limit headers, which reads
			// exactly like quota exhaustion and is not. Say so, so the next
			// reader does not chase the wrong cause.
			msg += " (a 429 here can also mean the request shape was rejected, not that quota ran out)"
		}
		return msg
	}

	return "usage refresh failed: " + err.Error() + " — showing stored data"
}

// refreshUnavailable returns a reason string when a refresh cannot run at
// all, or "" when it can.
//
// Two conditions, checked in order:
//
//  1. Inside a sandbox. PRISM_HOST_API is the sentinel, and it is set only by
//     the sidecar when it launches a sandboxed session. The accounts
//     directory is not bound into that sandbox, so the token is out of reach
//     by construction.
//  2. The accounts directory is not readable for any other reason — it does
//     not exist yet, or the permissions deny it.
//
// The sandbox check comes first so the common case gets the message that
// explains it, rather than a generic "not readable".
func refreshUnavailable() string {
	if sandboxenv.IsInsideSandbox() {
		return "~/.config/prism/accounts/ is not visible inside an agent sandbox — run `prism account usage` on the host to refresh"
	}
	paths, err := account.ResolvePaths()
	if err != nil {
		return "the accounts directory could not be resolved"
	}
	// A read of the directory, not a stat: a directory can be statable and
	// still unreadable, and the refresh needs to read a file inside it.
	f, err := os.Open(paths.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("%s does not exist — run `prism account login <name>` first", paths.Dir)
		}
		return fmt.Sprintf("%s is not readable", paths.Dir)
	}
	_ = f.Close()
	return ""
}

// activeAccountName returns the name in accounts/current.
//
// Only the ACTIVE account is ever refreshed. The sidecar endpoint takes no
// account parameter — it resolves `accounts/current` host-side at write time
// (#2538) — so a snapshot fetched for any other account would be persisted
// under the active account's name and misattribute the numbers.
func activeAccountName() (string, error) {
	paths, err := account.ResolvePaths()
	if err != nil {
		return "", errors.New("the accounts directory could not be resolved")
	}
	name, ok, err := account.Current(paths)
	if err != nil {
		return "", errors.New("the active account could not be read")
	}
	if !ok {
		return "", errors.New("no active account — run `prism account use <name>` first")
	}
	return name, nil
}

// readActiveCredentials loads the stored access token for accountName.
//
// The returned error never carries token material: account.ReadCredentials
// guarantees that, and the wrapping here adds only the account name.
func readActiveCredentials(accountName string) (account.Credentials, error) {
	paths, err := account.ResolvePaths()
	if err != nil {
		return account.Credentials{}, errors.New("the accounts directory could not be resolved")
	}
	creds, err := account.ReadCredentials(paths, accountName)
	if err != nil {
		if errors.Is(err, account.ErrNoCredentials) || errors.Is(err, account.ErrNoAccessToken) {
			return account.Credentials{}, fmt.Errorf(
				"account %q has no stored access token — run `prism account login %s`", accountName, accountName)
		}
		return account.Credentials{}, fmt.Errorf("account %q: %v", accountName, err)
	}
	return creds, nil
}

// needsRefresh reports whether accountName's snapshot is missing or stale.
//
// A row whose file failed to parse counts as missing: an unreadable snapshot
// carries no numbers, so refreshing it is exactly what the user wants.
func needsRefresh(rows []usage.AccountSnapshot, accountName string, now time.Time) bool {
	for _, row := range rows {
		if row.Name != accountName {
			continue
		}
		if row.ReadErr != nil || row.Snapshot == nil {
			return true
		}
		return row.Stale(now)
	}
	return true
}

// persistSnapshot POSTs the payload to a live sidecar's /usage/snapshot.
//
// This is the ONLY write path. The refresh never touches the snapshot files
// itself, so the sidecar keeps sole ownership of account resolution and of
// the atomic write, exactly as it does for the passive capture hook.
func persistSnapshot(payload *usage.SnapshotPayload) error {
	apiURL, err := discoverSidecarAPI()
	if err != nil {
		return err
	}
	// proxyToHostAPI is the same helper every other container-aware command
	// uses; it handles both the unix:// and http:// forms of the URL and
	// surfaces the endpoint's JSON "error" field.
	var resp struct {
		Account string `json:"account"`
	}
	return proxyToHostAPI(apiURL, usageSnapshotEndpoint, payload, &resp)
}

// discoverSidecarAPI finds a live sidecar host-API socket to POST through.
//
// The refresh only runs host-side, where PRISM_HOST_API is unset by
// definition (the sidecar injects it into sandboxed sessions only), so there
// is no environment variable to read. Instead the per-session run directories
// are scanned and each candidate socket is dialled.
//
// ANY live sidecar will do. /usage/snapshot ignores the calling session
// entirely: it resolves `accounts/current` itself and writes the one shared
// state directory, so every sidecar produces an identical result. There is
// therefore no "correct" sidecar to prefer, only a reachable one.
//
// The PRISM_HOST_API branch below is unreachable from the refresh path TODAY,
// and that is deliberate rather than an oversight. The refresh runs only when
// refreshUnavailable() passed, and that function rejects a sandbox using the
// same variable as its sentinel, so the variable is always empty by the time
// control arrives here. It is retained because "the sandbox sentinel" and
// "the sidecar URL" are two different facts that merely share one variable
// today: if the accounts directory is ever bound into sandboxes, or the token
// is ever fetched through the host API, this becomes the correct branch and
// its absence becomes a bug.
func discoverSidecarAPI() (string, error) {
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		return apiURL, nil
	}

	runDir, err := sidecarRunDir()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(runDir, "*", "hostapi.sock"))
	if err != nil {
		return "", fmt.Errorf("scan %s for a sidecar socket: %w", runDir, err)
	}
	// Deterministic order so a host with several live sessions always picks
	// the same socket, which keeps the failure mode reproducible.
	sort.Strings(matches)

	for _, sock := range matches {
		conn, dialErr := net.DialTimeout("unix", sock, sidecarDialTimeout)
		if dialErr != nil {
			// An absent listener on an existing socket file is a tombstone
			// from a sidecar that exited without cleanup. Skip it.
			continue
		}
		_ = conn.Close()
		return "unix://" + sock, nil
	}
	return "", errors.New("no running prism sidecar found to persist through — start a prism session and retry")
}

// sidecarRunDir returns $XDG_STATE_HOME/prism/run, the directory holding one
// subdirectory per session. Mirrors internal/session's own resolution, which
// is not exported.
func sidecarRunDir() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errors.New("cannot resolve the prism state directory — neither XDG_STATE_HOME nor HOME is set")
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "run"), nil
}

// mergeRefreshed returns rows with the refreshed snapshot substituted in,
// appending a new row when the account had none.
//
// The freshly fetched object is displayed directly rather than re-read from
// disk. Two reasons: the content is identical to what the sidecar persists,
// and re-reading would show nothing at all on the branch where the POST could
// not be delivered.
func mergeRefreshed(rows []usage.AccountSnapshot, snap *usage.Snapshot, accountName string) []usage.AccountSnapshot {
	if snap == nil || accountName == "" {
		return rows
	}
	for i := range rows {
		if rows[i].Name == accountName {
			rows[i].Snapshot = snap
			rows[i].ReadErr = nil
			rows[i].Active = true
			return rows
		}
	}
	rows = append(rows, usage.AccountSnapshot{
		Name:     accountName,
		Active:   true,
		Snapshot: snap,
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// writeRefreshWarning emits w's warning line to stderr, if any.
//
// stderr, not stdout: `--json` promises a parseable array on stdout and a
// warning mixed into it would break every consumer.
func writeRefreshWarning(errW io.Writer, out refreshOutcome) {
	if out.Warning == "" {
		return
	}
	fmt.Fprintln(errW, "warning: "+out.Warning)
}
