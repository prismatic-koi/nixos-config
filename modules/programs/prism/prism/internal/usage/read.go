// This file implements the read path behind `prism account usage`. It reads
// what usage.go already writes and adds no new on-disk format of its own.
//
// Sandbox constraint: this package must never depend on
// ~/.config/prism/accounts/ (invisible inside an agent sandbox — see
// internal/container/mounts.go). The active account is identified from
// current.json alone, which lives in the same usage directory this package
// already reads.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StaleAfter is the age past which a snapshot's percentage is considered
// stale ("a snapshot older than 15 minutes is stale"). The
// countdown to reset is unaffected — Reset is an absolute timestamp and stays
// correct regardless of snapshot age.
const StaleAfter = 15 * time.Minute

// AccountSnapshot pairs one account's snapshot with its filename-derived
// account name and whether it is the active account. The account name here
// always comes from the filename (via SanitizeAccountName's inverse — the
// file basename minus ".json"), not from the "account" field inside the
// JSON, so a corrupt or stale "account" field inside a file can never
// misattribute a row.
type AccountSnapshot struct {
	// Name is the account name, taken from the <account>.json filename.
	Name string
	// Active reports whether this account is named in current.json.
	Active bool
	// Snapshot is the parsed snapshot, or nil when the file could not be
	// parsed (see ReadErr).
	Snapshot *Snapshot
	// ReadErr is set when the file exists but failed to parse. Name and
	// Active are still populated; Snapshot is nil.
	ReadErr error
}

// Stale reports whether a's snapshot's CapturedAt is older than StaleAfter,
// as of now. A snapshot with no parseable CapturedAt, or no snapshot at all,
// is treated as not-stale by this helper — callers with a ReadErr should not
// call this.
func (a AccountSnapshot) Stale(now time.Time) bool {
	if a.Snapshot == nil {
		return false
	}
	t, err := time.Parse(time.RFC3339, a.Snapshot.CapturedAt)
	if err != nil {
		return false
	}
	return now.Sub(t) > StaleAfter
}

// ErrUsageDirMissing is returned by ReadAll when the usage directory does not
// exist. Callers should treat this as a non-fatal, exit-0 condition,
// printing a message naming the missing directory.
type ErrUsageDirMissing struct {
	Dir string
}

func (e *ErrUsageDirMissing) Error() string {
	return fmt.Sprintf("usage directory %s does not exist", e.Dir)
}

// ReadAll reads every per-account snapshot file in dir, plus current.json to
// determine the active account, and returns one AccountSnapshot per account
// file found, sorted by account name.
//
// A malformed snapshot file is reported via that row's ReadErr rather than
// failing the whole call, so the remaining accounts' rows are still
// returned.
//
// Returns *ErrUsageDirMissing when dir does not exist. Any other error is a
// genuine I/O failure reading the directory itself.
func ReadAll(dir string) ([]AccountSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &ErrUsageDirMissing{Dir: dir}
		}
		return nil, fmt.Errorf("usage: read dir %s: %w", dir, err)
	}

	activeAccount := activeAccountName(dir)

	rows := make([]AccountSnapshot, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == CurrentFileName {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		accountName := strings.TrimSuffix(name, ".json")

		row := AccountSnapshot{
			Name:   accountName,
			Active: accountName == activeAccount && activeAccount != "",
		}

		raw, rErr := os.ReadFile(filepath.Join(dir, name))
		if rErr != nil {
			row.ReadErr = fmt.Errorf("read %s: %w", name, rErr)
			rows = append(rows, row)
			continue
		}
		var snap Snapshot
		if uErr := json.Unmarshal(raw, &snap); uErr != nil {
			row.ReadErr = fmt.Errorf("parse %s: %w", name, uErr)
			rows = append(rows, row)
			continue
		}
		row.Snapshot = &snap
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// activeAccountName reads <dir>/current.json and returns its "account"
// field, or "" when the file is absent or malformed. A malformed
// current.json must not fail ReadAll — it just means no row is marked
// active.
func activeAccountName(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, CurrentFileName))
	if err != nil {
		return ""
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return ""
	}
	return snap.Account
}
