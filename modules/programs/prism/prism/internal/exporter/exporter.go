// Package exporter is the `prism exporter` host daemon: a long-running
// process that opens prism.db read-only and serves prism's own operational
// metrics on /metrics for Alloy to scrape (issue #2700, parent #2699).
//
// # The split that is the whole design
//
// Counters are produced by tailing agent_events forward by its monotonic
// rowid, accumulating in memory, and persisting the cursor and the values
// together — see internal/tailcursor for why, and for the mechanism.
//
// Gauges are recomputed at scrape time. They are point-in-time by
// definition, carry no monotonicity contract, and so the 90-day prune
// cannot hurt them.
//
// This issue ships exactly two metrics, one of each kind, to prove both
// halves. #2702 to #2706 add the rest.
//
// # Where it runs
//
// On the host. prism.db is not bind-mounted into worker sandboxes, so
// nothing here may assume in-sandbox database access.
package exporter

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/metrics"
	"github.com/prismatic-koi/prism/internal/tailcursor"
	"github.com/prismatic-koi/prism/internal/usage"
)

// Version is the version string reported by prism_exporter_build_info. It
// is a var so a release build can set it with -ldflags -X; the default is
// what a `go build` produces.
var Version = "dev"

// Fixed names. #2701 points its Alloy scrape at DefaultPort and
// MetricsPath; nothing else may move them without editing that config too.
const (
	// DefaultListenHost is the loopback address the daemon binds by
	// default. The exporter carries prism's operational history and has no
	// authentication of its own, so it is not exposed on the network:
	// Alloy runs on the same host and scrapes it over loopback.
	DefaultListenHost = "127.0.0.1"

	// DefaultPort is the TCP port /metrics is served on.
	//
	// It sits deliberately outside the 9100-9999 band that the Prometheus
	// project allocates to community exporters, so it cannot collide with
	// a future exporter default on navi or tui.
	DefaultPort = 19891

	// MetricsPath is the only route the daemon serves.
	MetricsPath = "/metrics"

	// DefaultPollInterval bounds how stale a counter can be when nothing
	// scrapes. The scrape path advances the tail synchronously as well, so
	// a scraped value is always current as of that scrape.
	DefaultPollInterval = 15 * time.Second

	// StateFileName is the tail-cursor state file, written next to
	// prism.db under $XDG_STATE_HOME/prism.
	StateFileName = "exporter-state.json"
)

// Metric and tailer names.
const (
	MetricBuildInfo        = "prism_exporter_build_info"
	MetricAgentEventsTotal = "prism_agent_events_total"

	// TailerAgentEvents is the key the agent_events cursor is stored under
	// in the state file. Changing it makes a running daemon lose its place
	// and re-initialise at the head of the table.
	TailerAgentEvents = "agent_events"
)

// scrapeAdvanceTimeout bounds the synchronous tail advance on the scrape
// path so a wedged database cannot hold a scrape open indefinitely.
const scrapeAdvanceTimeout = 10 * time.Second

// shutdownTimeout is how long Run gives in-flight scrapes to drain.
const shutdownTimeout = 5 * time.Second

// Config configures an Exporter.
type Config struct {
	// DBPath is the prism.db to read. Required.
	DBPath string
	// StatePath is the tail-cursor state file. Required.
	StatePath string
	// UsageDir is the prism usage-snapshot directory
	// (~/.local/state/prism/usage), read at scrape time to map an account
	// name to its org ID for the #2704 account dimension. Optional: when
	// empty, New resolves it from usage.DefaultDir(), and if that also fails
	// every account folds to account_org_id="unknown" rather than the scrape
	// failing.
	UsageDir string
	// ListenAddr is the "host:port" the HTTP server binds. Required.
	// A port of 0 binds an ephemeral port — Addr reports the real one.
	ListenAddr string
	// PollInterval is the background tail interval. Zero uses
	// DefaultPollInterval.
	PollInterval time.Duration
	// Logger receives operational messages. Zero uses the standard logger.
	Logger *log.Logger
	// Version overrides the version label of prism_exporter_build_info.
	// Zero uses the package-level Version.
	Version string
}

// Exporter is the daemon. Construct with New and run with Run.
type Exporter struct {
	cfg      Config
	logger   *log.Logger
	conn     *sql.DB
	registry *metrics.Registry
	store    *tailcursor.Store

	eventsTotal *metrics.CounterVec
	lifecycle   *lifecycleCounters
	cost        *costCounters
	tailers     []tailcursor.Advancer

	// mu serialises the tail advance and the state write. Both the scrape
	// handler and the poll ticker call refresh, and a Tailer is not safe
	// for concurrent use.
	mu sync.Mutex
	// started records that Start has positioned the tailers. Refresh
	// refuses to run before that: a tailer at its zero cursor would read
	// agent_events from the very beginning and backfill history, which is
	// exactly what the no-state-file rule forbids.
	started bool

	// addr is the resolved listen address, set once Run has bound.
	addrMu sync.RWMutex
	addr   string
}

// DefaultStatePath returns the state-file path alongside dbPath.
func DefaultStatePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), StateFileName)
}

// New validates cfg, opens prism.db read-only, and registers the metrics.
// It does not bind a listener and does not read the state file — Run does
// both.
func New(cfg Config) (*Exporter, error) {
	if cfg.DBPath == "" {
		return nil, errors.New("exporter: Config.DBPath is required")
	}
	if cfg.StatePath == "" {
		return nil, errors.New("exporter: Config.StatePath is required")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New("exporter: Config.ListenAddr is required")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.PollInterval < 0 {
		return nil, fmt.Errorf("exporter: Config.PollInterval must be >= 0, got %v", cfg.PollInterval)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "prism-exporter: ", log.LstdFlags)
	}
	version := cfg.Version
	if version == "" {
		version = Version
	}

	// Read-only handle. db.OpenReadOnly builds a "?mode=ro" DSN, which
	// makes SQLite itself reject any write with "attempt to write a
	// readonly database" — the enforcement is in the engine, not in a
	// convention this package has to keep. prism.db is SQLite in WAL mode,
	// so this handle is safe alongside the live sidecars.
	conn, err := db.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("exporter: open %s read-only: %w", cfg.DBPath, err)
	}

	e := &Exporter{
		cfg:      cfg,
		logger:   logger,
		conn:     conn,
		registry: metrics.NewRegistry(),
		store:    tailcursor.NewStore(cfg.StatePath),
	}

	// Gauge — recomputed on every scrape. Its value is a constant 1, which
	// is the whole point of a build-info metric: the information is in the
	// labels, and both labels are closed sets.
	e.registry.MustRegister(metrics.NewGaugeFunc(
		MetricBuildInfo,
		"Build information for the running prism exporter. Always 1; the labels carry the value.",
		[]string{"version", "go_version"},
		[]string{version, runtime.Version()},
		func() float64 { return 1 },
	))

	// Counter — produced by the tail cursor, never by an aggregate.
	//
	// The `type` label is folded through the closed set in eventtypes.go. The
	// column is writable from inside a worker sandbox, so the label name being
	// a closed set is not enough — the VALUE has to be bounded too. Putting
	// the fold on the CounterVec covers the restore path as well as the tail
	// path, so a poisoned state file cannot reintroduce a series either.
	e.eventsTotal = metrics.NewCounterVec(
		MetricAgentEventsTotal,
		"Total prism agent lifecycle events observed by the exporter, by event type. "+
			"Types outside the exporter's known set are counted as type=\"other\".",
		[]string{"type"},
		metrics.WithLabelValueNormaliser(eventTypeNormaliser),
	)
	e.registry.MustRegister(e.eventsTotal)

	tailer, err := tailcursor.New[string](
		TailerAgentEvents,
		agentEventSource{conn: conn},
		func(eventType string) error { return e.eventsTotal.Inc(eventType) },
		tailcursor.WithLogger(logger),
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("exporter: build agent_events tailer: %w", err)
	}

	// The six #2703 lifecycle and outcome counters. A second, independent
	// tailer over the same agent_events table, with its own cursor — see
	// lifecycle.go and LifecycleEventsTailSQL for why one more tailer is
	// simpler and safer here than teaching the #2700 tailer a second Value
	// shape.
	e.lifecycle = newLifecycleCounters(e.registry)
	lifecycleTailer, err := tailcursor.New[lifecycleEvent](
		TailerLifecycleEvents,
		lifecycleEventSource{conn: conn},
		e.lifecycle.apply,
		tailcursor.WithLogger(logger),
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("exporter: build lifecycle events tailer: %w", err)
	}

	// The three #2704 cost and token counters, plus the prism_account_info
	// join gauge. A third tailer over agent_events, with its own cursor,
	// reading only the msg_assistant rows that carry token usage. The account
	// resolver reads the usage snapshots at scrape time (cost.go).
	usageDir := cfg.UsageDir
	if usageDir == "" {
		// Best effort: a failure here is not fatal. An empty usageDir makes
		// every account resolve to "unknown" rather than failing the daemon.
		if dir, dirErr := usage.DefaultDir(); dirErr == nil {
			usageDir = dir
		} else {
			logger.Printf("cannot resolve the usage-snapshot directory (%v); "+
				"account attribution will report account_org_id=\"unknown\" until --usage-dir is set", dirErr)
		}
	}
	e.cost = newCostCounters(e.registry, &accountResolver{usageDir: usageDir})
	costTailer, err := tailcursor.New[costEvent](
		TailerCostEvents,
		costEventSource{conn: conn},
		e.cost.apply,
		tailcursor.WithLogger(logger),
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("exporter: build cost events tailer: %w", err)
	}

	e.tailers = []tailcursor.Advancer{tailer, lifecycleTailer, costTailer}

	return e, nil
}

// Close releases the database handle. Run closes it on return, so an
// explicit Close is only needed when New succeeded but Run was never
// called.
func (e *Exporter) Close() error {
	if e.conn == nil {
		return nil
	}
	return e.conn.Close()
}

// Registry exposes the metric registry.
func (e *Exporter) Registry() *metrics.Registry { return e.registry }

// Addr returns the bound listen address once Run has bound its listener,
// or "" before that. Tests bind port 0 and read the real port here.
func (e *Exporter) Addr() string {
	e.addrMu.RLock()
	defer e.addrMu.RUnlock()
	return e.addr
}

// Start reads the state file and positions every tailer.
//
// Three inputs, three outcomes, none of them fatal:
//
//   - No state file — the first run. Every tailer initialises at the
//     current head of its source and history is NOT backfilled.
//   - A corrupt or truncated state file — logged, then handled exactly
//     like the no-state-file case. Accumulated history is lost; the daemon
//     stays up.
//   - A valid state file — counter values are restored, then each tailer
//     resumes from its saved cursor.
//
// Start is called by Run. It is exported so a caller can position the
// daemon without serving.
//
// Start is idempotent: a second call is a no-op. That is not defensive
// tidiness, it protects the central property of this package. A state file
// that loads cleanly but whose counter values cannot be applied sends Start
// through resetPersistedMetricsLocked, and on a second call that would zero
// a counter that is already serving — a counter decrease, which is the one
// thing the whole design exists to prevent. Making the guarantee structural
// beats relying on call discipline.
func (e *Exporter) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}

	state, err := e.store.Load()
	switch {
	case err == nil:
		if restoreErr := e.registry.Restore(state.Counters); restoreErr != nil {
			e.logger.Printf("state file %s holds unusable counter values (%v); "+
				"discarding accumulated history and re-initialising at the head of the source",
				e.store.Path(), restoreErr)
			e.resetPersistedMetricsLocked()
			state = nil
		}
	case errors.Is(err, tailcursor.ErrNoState):
		// First run. Nothing to log at warning level.
		state = nil
	default:
		var corrupt *tailcursor.CorruptError
		if errors.As(err, &corrupt) {
			e.logger.Printf("%v; discarding accumulated history and re-initialising at the head of the source", corrupt)
			state = nil
		} else {
			// An unreadable file (permissions, I/O) is still not a reason
			// to refuse to serve — the daemon degrades to a fresh start.
			e.logger.Printf("cannot read state file %s (%v); re-initialising at the head of the source",
				e.store.Path(), err)
			state = nil
		}
	}

	for _, t := range e.tailers {
		cursor, ok := int64(0), false
		if state != nil {
			cursor, ok = state.Cursor(t.Name())
		}
		if !ok {
			if initErr := t.InitAtHead(ctx); initErr != nil {
				return initErr
			}
			continue
		}
		if resumeErr := t.Resume(ctx, cursor); resumeErr != nil {
			e.logger.Printf("cannot resume tailer %q from cursor %d (%v); re-initialising at the head of the source",
				t.Name(), cursor, resumeErr)
			if initErr := t.InitAtHead(ctx); initErr != nil {
				return initErr
			}
		}
	}

	e.started = true

	// Persist immediately so a first run leaves a usable state file even
	// if nothing ever scrapes it.
	if err := e.persistLocked(); err != nil {
		e.logger.Printf("cannot write state file %s: %v", e.store.Path(), err)
	}
	return nil
}

// Refresh advances every tailer and persists the result. It is safe to call
// concurrently; calls serialise.
func (e *Exporter) Refresh(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshLocked(ctx)
}

func (e *Exporter) refreshLocked(ctx context.Context) error {
	if !e.started {
		return errors.New("exporter: Refresh before Start; the tailers are not positioned yet")
	}
	var (
		changed bool
		errs    []error
	)
	for _, t := range e.tailers {
		before := t.Cursor()
		applied, err := t.Advance(ctx)
		if applied > 0 || t.Cursor() != before {
			changed = true
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if changed {
		if err := e.persistLocked(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// persistLocked writes the cursors and the counter values as one atomic
// snapshot. Caller holds e.mu.
func (e *Exporter) persistLocked() error {
	state := tailcursor.NewState()
	for _, t := range e.tailers {
		state.SetCursor(t.Name(), t.Cursor())
	}
	state.Counters = e.registry.Snapshot()
	return e.store.Save(state)
}

// resetPersistedMetricsLocked zeroes every persistable collector. Caller
// holds e.mu. Used when a state file is readable but its values cannot be
// applied, so the daemon does not start from a half-restored set.
func (e *Exporter) resetPersistedMetricsLocked() {
	for _, p := range e.registry.PersistentCollectors() {
		if err := p.Restore(nil); err != nil {
			e.logger.Printf("cannot reset metric %q: %v", p.Name(), err)
		}
	}
}

// Handler returns the HTTP handler the daemon serves. Exposed so a test can
// drive /metrics without binding a port.
func (e *Exporter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(MetricsPath, e.serveMetrics)
	return mux
}

func (e *Exporter) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Advance the tail before gathering so a scraped counter is current as
	// of this scrape. A failure here is logged and the last known values
	// are served anyway: a transient database error must not blank a
	// scrape, which Prometheus would read as the target going down.
	ctx, cancel := context.WithTimeout(r.Context(), scrapeAdvanceTimeout)
	defer cancel()
	if err := e.Refresh(ctx); err != nil {
		e.logger.Printf("scrape: tail advance failed, serving last known values: %v", err)
	}

	var buf bytes.Buffer
	if err := e.registry.Gather(&buf); err != nil {
		e.logger.Printf("scrape: gather failed: %v", err)
		http.Error(w, "failed to gather metrics", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, &buf); err != nil {
		e.logger.Printf("scrape: write response: %v", err)
	}
}

// Run positions the daemon, binds the listener, and serves /metrics until
// ctx is cancelled. It closes the database handle before returning.
//
// On cancellation it drains in-flight scrapes, then writes one last state
// snapshot so a clean stop-and-start loses nothing.
func (e *Exporter) Run(ctx context.Context) error {
	defer e.conn.Close()

	if err := e.Start(ctx); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", e.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("exporter: listen on %s: %w", e.cfg.ListenAddr, err)
	}
	e.addrMu.Lock()
	e.addr = ln.Addr().String()
	e.addrMu.Unlock()

	srv := &http.Server{
		Handler:           e.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	e.logger.Printf("serving %s on http://%s%s (db %s, state %s)",
		MetricsPath, ln.Addr(), MetricsPath, e.cfg.DBPath, e.store.Path())

	serveErr := make(chan error, 1)
	go func() {
		serveErrLocal := srv.Serve(ln)
		if errors.Is(serveErrLocal, http.ErrServerClosed) {
			serveErr <- nil
			return
		}
		serveErr <- serveErrLocal
	}()

	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			_ = srv.Shutdown(shutdownCtx)
			cancel()
			<-serveErr
			e.finalPersist()
			return nil

		case err := <-serveErr:
			e.finalPersist()
			if err != nil {
				return fmt.Errorf("exporter: serve: %w", err)
			}
			return nil

		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(ctx, scrapeAdvanceTimeout)
			if err := e.Refresh(tickCtx); err != nil && ctx.Err() == nil {
				e.logger.Printf("poll: tail advance failed: %v", err)
			}
			cancel()
		}
	}
}

// finalPersist writes one last snapshot on shutdown. A failure is logged,
// not returned: the next start simply resumes from the previous snapshot,
// which is a correct (and, because the cursor and the values are one
// snapshot, exact) recovery.
func (e *Exporter) finalPersist() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.persistLocked(); err != nil {
		e.logger.Printf("cannot write final state file %s: %v", e.store.Path(), err)
	}
}
