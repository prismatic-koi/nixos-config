package tailcursor

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// DefaultBatchSize is the number of records a single Advance step reads
// before applying them and moving the cursor on. Advance loops until the
// source is drained, so this bounds memory per step, not total throughput.
const DefaultBatchSize = 1000

// Record is one row of a Source, carrying the monotonic id the cursor
// advances over and the projected value the accumulator consumes.
//
// The value is deliberately a type parameter, not a row struct: a tailer
// must read only the columns its metric needs. The exporter is a
// "/stats"-class surface and must never read a raw TEXT body column.
type Record[T any] struct {
	ID    int64
	Value T
}

// Source reads a table forward by a monotonic id.
//
// Implementations must return records in ascending id order, and must not
// write to the database.
type Source[T any] interface {
	// MaxID returns the current maximum id in the source, or 0 when the
	// source is empty.
	MaxID(ctx context.Context) (int64, error)
	// Records returns up to limit records with id > afterID, ascending.
	Records(ctx context.Context, afterID int64, limit int) ([]Record[T], error)
}

// Advancer is the non-generic view of a Tailer. A daemon that runs several
// tailers over different row types holds them in one []Advancer.
type Advancer interface {
	Name() string
	Cursor() int64
	// InitAtHead sets the cursor to the current maximum id of the source,
	// so a first run counts only what happens from now on.
	InitAtHead(ctx context.Context) error
	// Resume sets the cursor to a value read from a state file.
	Resume(ctx context.Context, cursor int64) error
	// Advance consumes every record after the cursor and returns how many
	// it applied.
	Advance(ctx context.Context) (int, error)
}

// Tailer advances a cursor over a Source and hands each new record to an
// accumulator function.
//
// A Tailer is NOT safe for concurrent use. The owning daemon serialises
// Advance calls — see internal/exporter, which holds one mutex across the
// scrape path and the poll ticker.
type Tailer[T any] struct {
	name   string
	source Source[T]
	apply  func(T) error
	batch  int
	logger *log.Logger

	cursor int64
}

// Option customises a Tailer.
type Option func(*options)

type options struct {
	batch  int
	logger *log.Logger
}

// WithBatchSize sets the per-step read size. Values below 1 are ignored.
func WithBatchSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.batch = n
		}
	}
}

// WithLogger sets the logger used for the cursor-regression warning. A nil
// logger silences it.
func WithLogger(l *log.Logger) Option {
	return func(o *options) { o.logger = l }
}

// New returns a Tailer named name over src, applying each record's value
// with apply.
//
// name is the key the cursor is stored under in the state file. It must be
// stable across releases: renaming it makes the daemon lose its place and
// re-initialise at the head of the source.
func New[T any](name string, src Source[T], apply func(T) error, opts ...Option) (*Tailer[T], error) {
	if name == "" {
		return nil, errors.New("tailcursor: New: empty name")
	}
	if src == nil {
		return nil, fmt.Errorf("tailcursor: New(%q): nil source", name)
	}
	if apply == nil {
		return nil, fmt.Errorf("tailcursor: New(%q): nil apply function", name)
	}
	o := options{batch: DefaultBatchSize}
	for _, opt := range opts {
		opt(&o)
	}
	return &Tailer[T]{
		name:   name,
		source: src,
		apply:  apply,
		batch:  o.batch,
		logger: o.logger,
	}, nil
}

// Name implements Advancer.
func (t *Tailer[T]) Name() string { return t.name }

// Cursor implements Advancer.
func (t *Tailer[T]) Cursor() int64 { return t.cursor }

// InitAtHead implements Advancer. It is the no-state-file path: the cursor
// jumps to the current maximum id, so history before this moment is never
// counted.
//
// Backfilling instead would be wrong twice over. It would make the first
// scrape after a fresh install report a large jump that never happened in
// that window, and — because prune has already removed everything older than
// 90 days — the "history" it counted would be an arbitrary, shrinking
// fraction of the real one.
func (t *Tailer[T]) InitAtHead(ctx context.Context) error {
	maxID, err := t.source.MaxID(ctx)
	if err != nil {
		return fmt.Errorf("tailcursor: %s: read head: %w", t.name, err)
	}
	t.cursor = maxID
	return nil
}

// Resume implements Advancer. A negative cursor is rejected; the caller
// treats that as a corrupt state file.
func (t *Tailer[T]) Resume(ctx context.Context, cursor int64) error {
	if cursor < 0 {
		return fmt.Errorf("tailcursor: %s: negative cursor %d", t.name, cursor)
	}
	t.cursor = cursor
	return t.clampToHead(ctx)
}

// Advance consumes every record after the cursor, applies it, and moves the
// cursor to the last applied id. It returns the number of records applied.
//
// On error, the cursor has still moved over whatever was applied
// successfully, so a retry does not re-count.
func (t *Tailer[T]) Advance(ctx context.Context) (int, error) {
	if err := t.clampToHead(ctx); err != nil {
		return 0, err
	}
	applied := 0
	for {
		if err := ctx.Err(); err != nil {
			return applied, err
		}
		records, err := t.source.Records(ctx, t.cursor, t.batch)
		if err != nil {
			return applied, fmt.Errorf("tailcursor: %s: read records after %d: %w", t.name, t.cursor, err)
		}
		if len(records) == 0 {
			return applied, nil
		}
		for _, r := range records {
			if r.ID <= t.cursor {
				return applied, fmt.Errorf(
					"tailcursor: %s: source returned id %d at or behind cursor %d (source must return ascending ids > afterID)",
					t.name, r.ID, t.cursor)
			}
			if err := t.apply(r.Value); err != nil {
				return applied, fmt.Errorf("tailcursor: %s: apply record %d: %w", t.name, r.ID, err)
			}
			t.cursor = r.ID
			applied++
		}
		if len(records) < t.batch {
			return applied, nil
		}
	}
}

// clampToHead handles the one case where the source id is not monotonic in
// practice.
//
// prism.db is SQLite and agent_events has no AUTOINCREMENT, so its implicit
// rowid is allocated as max(rowid)+1. Delete the highest-rowid row — which
// the 90-day prune does when it empties the whole table on a machine that
// was idle for a quarter — and SQLite reuses the freed rowids for the next
// inserts. The cursor would then sit permanently ahead of every new row and
// the counter would stop advancing, quietly.
//
// The correction is the minimal one: when the head is behind the cursor,
// move the cursor DOWN to the head. Nothing is re-counted (every id at or
// below the head has already been consumed or deleted), no future row is
// missed (the next insert lands above the head), and no counter value is
// touched — so the counter still never decreases. The rows that were pruned
// before they were tailed are lost, which is an undercount, and an
// undercount is the only direction that keeps rate() honest.
func (t *Tailer[T]) clampToHead(ctx context.Context) error {
	maxID, err := t.source.MaxID(ctx)
	if err != nil {
		return fmt.Errorf("tailcursor: %s: read head: %w", t.name, err)
	}
	if maxID >= t.cursor {
		return nil
	}
	if t.logger != nil {
		t.logger.Printf(
			"tailcursor: %s: source head %d is behind cursor %d (rowid reuse after a full-table delete); "+
				"clamping the cursor to the head, counter values are unchanged",
			t.name, maxID, t.cursor)
	}
	t.cursor = maxID
	return nil
}
