// Package spawnbudget counts the worker spawns of the last 7 days and
// compares the count with the configured limit (issue #3177).
//
// The count is the higher of two values: the fleet-wide count from
// Prometheus (prism_spawns_total, all hosts) and the count in the local
// prism database (this host only). At 100% of the limit, `prism spawn`
// refuses to start a worker unless the caller passes --over-budget
// (issue #3178). A failed lookup never delays a spawn by more than
// LookupTimeout, and an unknown count never refuses a spawn.
package spawnbudget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

const (
	// Window is the rolling window of the budget: 7 days, not a calendar
	// week.
	Window = 168 * time.Hour

	// LookupTimeout is a hard limit on the whole lookup: the Prometheus
	// query and the local count together. At the deadline, Lookup returns
	// with what it has.
	LookupTimeout = 2 * time.Second

	// WorkerRole is the only agent_role the budget counts.
	WorkerRole = "worker"

	// FleetQuery is the PromQL for the fleet-wide count. It must select the
	// same role as WorkerRole and the same range as Window.
	FleetQuery = `sum(increase(prism_spawns_total{agent_role="worker"}[7d]))`

	// NearPercent is the share of the limit at which the budget tells the
	// user to finish the current work and stop for the day.
	NearPercent = 95

	// LinePrefix starts every budget line. The host-API /spawn handler finds
	// the line in the output of the host-side `prism spawn` by this prefix
	// and returns it to a sandboxed caller.
	LinePrefix = "spawn budget:"

	// EventSpawnOverride is the agent_events.type that `prism spawn
	// --over-budget` writes for each override. The exporter counts it as
	// prism_spawn_overrides_total, by repo.
	EventSpawnOverride = "spawn.over_budget_override"
)

// Level is the position of a count relative to the limit.
type Level int

const (
	// LevelUnder is below NearPercent of the limit.
	LevelUnder Level = iota
	// LevelNear is NearPercent of the limit or more, but below the limit.
	LevelNear
	// LevelOver is the limit or more.
	LevelOver
)

// LevelFor classifies count against limit. A limit below 1 has no
// threshold, so every count is LevelUnder.
func LevelFor(count, limit int) Level {
	switch {
	case limit < 1:
		return LevelUnder
	case count >= limit:
		return LevelOver
	case count*100 >= limit*NearPercent:
		return LevelNear
	default:
		return LevelUnder
	}
}

// Result is the outcome of one Lookup.
type Result struct {
	// Count is the higher of FleetCount and LocalCount, from the sources
	// that answered. It is valid only when Known is true.
	Count int
	Limit int
	// Known is false when neither source answered.
	Known bool
	// Fleet is true when Count includes the fleet-wide count from
	// Prometheus, so it covers all hosts.
	Fleet bool
	// LocalOnlyReason says why the Prometheus count is missing. It is empty
	// when Fleet is true.
	LocalOnlyReason string
	FleetCount      int
	LocalCount      int
	// LocalErr is the error of the local count, or nil.
	LocalErr error
}

// Level returns the level of the result. An unknown count is LevelUnder.
func (r Result) Level() Level {
	if !r.Known {
		return LevelUnder
	}
	return LevelFor(r.Count, r.Limit)
}

// Percent is Count as a whole percentage of Limit, rounded down.
func (r Result) Percent() int {
	if r.Limit < 1 {
		return 0
	}
	return r.Count * 100 / r.Limit
}

// Scope is a short text that says which hosts Count covers.
func (r Result) Scope() string {
	if r.Fleet {
		return "all hosts"
	}
	return "this host only: " + r.LocalOnlyReason
}

// Line is the one-line budget text that `prism spawn` prints for a worker
// spawn. It always starts with LinePrefix.
func (r Result) Line() string {
	if !r.Known {
		reason := r.LocalOnlyReason
		if r.LocalErr != nil {
			reason += ", and the local count failed"
		}
		return fmt.Sprintf("%s the count is not available (%s). The limit is %d worker spawns in 7 days.",
			LinePrefix, reason, r.Limit)
	}
	base := fmt.Sprintf("%s %d of %d worker spawns in the last 7 days (%d%%, %s).",
		LinePrefix, r.Count, r.Limit, r.Percent(), r.Scope())
	switch r.Level() {
	case LevelOver:
		return base + " OVER THE LIMIT. Finish the current work and stop for the day."
	case LevelNear:
		return fmt.Sprintf("%s WARNING: %d%% of the limit or more. Finish the current work and stop for the day.",
			base, NearPercent)
	default:
		return base
	}
}

// LocalCountFunc returns the number of worker spawns on this host at or
// after since.
type LocalCountFunc func(ctx context.Context, since time.Time) (int, error)

// LocalCounter returns the LocalCountFunc over d. It counts the same event
// that the exporter counts for prism_spawns_total (session.EventSpawnIntent),
// with the role from the same sessions join, so the local count and the
// fleet count agree on this host.
func LocalCounter(d *db.DB) LocalCountFunc {
	return func(ctx context.Context, since time.Time) (int, error) {
		return d.CountRoleEventsSince(ctx, session.EventSpawnIntent, WorkerRole, since)
	}
}

// Options configures Lookup.
type Options struct {
	// PrometheusURL is the base URL of the Prometheus API. Empty skips the
	// fleet query.
	PrometheusURL string
	// Limit is the configured limit of worker spawns per 7 days.
	Limit int
	// Local counts the worker spawns of this host. Nil skips the local
	// count.
	Local LocalCountFunc
	// Client sends the Prometheus query. Nil uses a new http.Client.
	Client *http.Client
	// Now returns the current time. Nil uses time.Now.
	Now func() time.Time
	// Timeout overrides LookupTimeout. Tests use it.
	Timeout time.Duration
}

type fleetAnswer struct {
	count int
	err   error
}

type localAnswer struct {
	count int
	err   error
}

// Lookup runs the Prometheus query and the local count at the same time and
// returns at the latest at the timeout. It never returns an error: a failed
// source only removes that source from the result.
func Lookup(ctx context.Context, o Options) Result {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = LookupTimeout
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Both channels have a buffer of one, so a source that ignores ctx and
	// answers after the deadline does not block its goroutine forever.
	fleetCh := make(chan fleetAnswer, 1)
	localCh := make(chan localAnswer, 1)
	if o.PrometheusURL != "" {
		go func() {
			n, err := QueryFleet(ctx, o.Client, o.PrometheusURL)
			fleetCh <- fleetAnswer{count: n, err: err}
		}()
	} else {
		fleetCh <- fleetAnswer{err: errNotConfigured}
	}
	if o.Local != nil {
		since := now().Add(-Window)
		go func() {
			n, err := o.Local(ctx, since)
			localCh <- localAnswer{count: n, err: err}
		}()
	} else {
		localCh <- localAnswer{err: errNoLocalDB}
	}

	var (
		fleet              fleetAnswer
		local              localAnswer
		gotFleet, gotLocal bool
	)
	for !gotFleet || !gotLocal {
		select {
		case fleet = <-fleetCh:
			gotFleet = true
		case local = <-localCh:
			gotLocal = true
		case <-ctx.Done():
			if !gotFleet {
				fleet = fleetAnswer{err: ctx.Err()}
				gotFleet = true
			}
			if !gotLocal {
				local = localAnswer{err: ctx.Err()}
				gotLocal = true
			}
		}
	}

	r := Result{Limit: o.Limit}
	if fleet.err == nil {
		r.Fleet = true
		r.Known = true
		r.FleetCount = fleet.count
		r.Count = fleet.count
	} else {
		r.LocalOnlyReason = fleetFailureReason(fleet.err, timeout)
	}
	if local.err == nil {
		r.LocalCount = local.count
		if !r.Known || local.count > r.Count {
			r.Count = local.count
		}
		r.Known = true
	} else {
		r.LocalErr = local.err
	}
	return r
}

var (
	errNotConfigured = errors.New("spawnbudget: no Prometheus URL configured")
	errNoLocalDB     = errors.New("spawnbudget: no local database")
	// ErrNoData means Prometheus answered, but the query returned no
	// sample. That is the answer when no host has a worker series in the
	// window.
	ErrNoData = errors.New("spawnbudget: Prometheus returned no data")
)

// fleetFailureReason is the short text after "this host only:" in a budget
// line.
func fleetFailureReason(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, errNotConfigured):
		return "no Prometheus URL is configured"
	case errors.Is(err, ErrNoData):
		return "Prometheus returned no data"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("Prometheus did not answer in %s", timeout)
	default:
		return "cannot reach Prometheus"
	}
}

// promResponse is the part of the Prometheus /api/v1/query response that
// QueryFleet reads.
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// QueryFleet sends FleetQuery to the Prometheus API at baseURL and returns
// the fleet-wide worker spawn count.
//
// increase() extrapolates, so the raw value is fractional (for example
// 196.04). QueryFleet rounds it to the nearest whole number. It returns
// ErrNoData when the result vector is empty or the value is not a number.
func QueryFleet(ctx context.Context, client *http.Client, baseURL string) (int, error) {
	if client == nil {
		client = &http.Client{}
	}
	u := strings.TrimRight(baseURL, "/") + "/api/v1/query?" + url.Values{"query": {FleetQuery}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("spawnbudget: build Prometheus request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("spawnbudget: Prometheus query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("spawnbudget: read Prometheus response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("spawnbudget: Prometheus query: HTTP %d", resp.StatusCode)
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("spawnbudget: decode Prometheus response: %w", err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("spawnbudget: Prometheus query status %q: %s", pr.Status, pr.Error)
	}
	if pr.Data.ResultType != "vector" {
		return 0, fmt.Errorf("spawnbudget: Prometheus result type %q, want vector", pr.Data.ResultType)
	}
	if len(pr.Data.Result) == 0 || len(pr.Data.Result[0].Value) != 2 {
		return 0, ErrNoData
	}
	var raw string
	if err := json.Unmarshal(pr.Data.Result[0].Value[1], &raw); err != nil {
		return 0, fmt.Errorf("spawnbudget: decode Prometheus sample value: %w", err)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, ErrNoData
	}
	if v < 0 {
		v = 0
	}
	return int(math.Round(v)), nil
}
