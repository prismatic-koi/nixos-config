package db

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MaxSchemaVersion returns the highest database schema version that this
// prism binary supports.
func MaxSchemaVersion() int { return currentSchemaVersion }

// MaxSchemaVersionArgs returns the prism arguments that print
// MaxSchemaVersion and nothing else. A prism binary runs another prism binary
// with these arguments to learn which schema version it supports. Keep the
// verb stable: an older binary that runs a newer one depends on it.
func MaxSchemaVersionArgs() []string { return []string{"db", "max-schema-version"} }

// SkewCause is the cause of a SchemaTooNewError that the process detected.
type SkewCause int

const (
	// SkewCauseUnknown means that the process cannot tell which cause
	// applies. The error then names every cause.
	SkewCauseUnknown SkewCause = iota
	// SkewCauseUpdatedWhileRunning means that the prism on PATH supports the
	// database version and this process runs an older binary.
	SkewCauseUpdatedWhileRunning
	// SkewCauseInstalledTooOld means that the prism on PATH does not support
	// the database version either.
	SkewCauseInstalledTooOld
)

// SchemaTooNewError reports that a writable open found a database schema
// version that is newer than this binary supports.
//
// RunningPath, InstalledPath, and InstalledMax are set only when Cause is
// not SkewCauseUnknown.
type SchemaTooNewError struct {
	DBVersion     int
	BinaryMax     int
	Cause         SkewCause
	RunningPath   string
	InstalledPath string
	InstalledMax  int
}

func (e *SchemaTooNewError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "db schema version %d is newer than this prism binary (max %d).",
		e.DBVersion, e.BinaryMax)
	switch e.Cause {
	case SkewCauseUpdatedWhileRunning:
		b.WriteString("\nCause: prism was updated while this session ran. " +
			"The session still runs the old prism binary.")
		fmt.Fprintf(&b, "\nThis process runs %s (max schema version %d).", e.RunningPath, e.BinaryMax)
		fmt.Fprintf(&b, "\nThe installed prism is %s (max schema version %d).", e.InstalledPath, e.InstalledMax)
		b.WriteString("\nFix: restart the session to load the installed prism. " +
			"On the host, run `prism restart`. This command restarts all sessions.")
	case SkewCauseInstalledTooOld:
		b.WriteString("\nCause: the installed prism is older than the database. " +
			"An older prism was installed, or a newer database was restored.")
		if e.RunningPath == e.InstalledPath {
			fmt.Fprintf(&b, "\nThis process runs the installed prism, %s.", e.RunningPath)
		} else {
			fmt.Fprintf(&b, "\nThis process runs %s (max schema version %d).", e.RunningPath, e.BinaryMax)
			fmt.Fprintf(&b, "\nThe installed prism is %s (max schema version %d).", e.InstalledPath, e.InstalledMax)
		}
		fmt.Fprintf(&b, "\nFix: install a prism that supports schema version %d, "+
			"or restore a database copy that the installed prism supports. "+
			"A session restart does not correct this.", e.DBVersion)
	default:
		b.WriteString("\nPrism cannot tell which of these two causes applies:")
		b.WriteString("\n1. Prism was updated while this session ran. " +
			"The session still runs the old prism binary. " +
			"Fix: restart the session. On the host, run `prism restart`.")
		fmt.Fprintf(&b, "\n2. An older prism is installed, or a newer database was restored. "+
			"Fix: install a prism that supports schema version %d, "+
			"or restore a database copy that the installed prism supports.", e.DBVersion)
	}
	return b.String()
}

// prismBinaries holds the resolved path of the prism binary that this process
// runs, the resolved path of the prism binary on PATH, and the highest schema
// version that the prism on PATH supports.
type prismBinaries struct {
	running      string
	installed    string
	installedMax int
}

// diagnose sets the cause of e from bins. When ok is false, the cause stays
// SkewCauseUnknown.
func (e *SchemaTooNewError) diagnose(bins prismBinaries, ok bool) {
	if !ok {
		return
	}
	e.RunningPath = bins.running
	e.InstalledPath = bins.installed
	e.InstalledMax = bins.installedMax
	if bins.installedMax >= e.DBVersion {
		e.Cause = SkewCauseUpdatedWhileRunning
	} else {
		e.Cause = SkewCauseInstalledTooOld
	}
}

// probeInstalledPrism is a variable so that tests can replace it. A test that
// lets Open reach the version guard must replace it, or the probe runs the
// prism on the PATH of the test host.
var probeInstalledPrism = probeInstalledPrismOnPath

// probeInstalledPrismOnPath finds the prism on PATH and the highest schema
// version that it supports. It resolves both binaries the same way as the
// sidecar staleness check (internal/sidecar/prism_binary_staleness.go). It
// cannot import that check, because internal/sidecar imports this package.
//
// It returns ok=false when it cannot find out. Then the caller names no
// single cause.
func probeInstalledPrismOnPath() (prismBinaries, bool) {
	self, err := os.Executable()
	if err != nil {
		return prismBinaries{}, false
	}
	running, err := filepath.EvalSymlinks(self)
	if err != nil {
		return prismBinaries{}, false
	}
	looked, err := exec.LookPath("prism")
	if err != nil {
		return prismBinaries{}, false
	}
	installed, err := filepath.EvalSymlinks(looked)
	if err != nil {
		return prismBinaries{}, false
	}
	if running == installed {
		return prismBinaries{running: running, installed: installed, installedMax: currentSchemaVersion}, true
	}
	installedMax, err := askMaxSchemaVersion(installed)
	if err != nil {
		return prismBinaries{}, false
	}
	return prismBinaries{running: running, installed: installed, installedMax: installedMax}, true
}

// maxSchemaVersionTimeout bounds askMaxSchemaVersion. It is a variable so
// that a test can make it short.
var maxSchemaVersionTimeout = 5 * time.Second

// askMaxSchemaVersion runs bin with MaxSchemaVersionArgs and parses the
// version that it prints. A prism that is older than the verb prints its help
// text instead, and the parse fails.
func askMaxSchemaVersion(bin string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), maxSchemaVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, MaxSchemaVersionArgs()...)
	// A child process of bin can keep stdout open after the kill. WaitDelay
	// stops Output from a wait on that pipe with no limit.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("db: ask %s for its max schema version: %w", bin, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("db: %s printed %q, not a schema version", bin, out)
	}
	return n, nil
}

// migrationCautionOut is the destination of the live-session caution. nil
// means os.Stderr, read at write time so that a test that replaces os.Stderr
// captures the caution.
var migrationCautionOut io.Writer

// cautionLiveSessions writes a caution when the database lists one or more
// live sessions. The caller is about to migrate the database from version
// from to currentSchemaVersion.
//
// A live session is an agent_status row with ended_at IS NULL. prism restore
// uses the same set to find the sessions to recreate.
//
// The caution does not read input and does not stop the migration. A failed
// count writes no caution, because the caution must never block an open.
func cautionLiveSessions(e sqlExecutor, from int) {
	var live int
	if err := e.QueryRow(`SELECT COUNT(*) FROM agent_status WHERE ended_at IS NULL`).Scan(&live); err != nil {
		return
	}
	if live == 0 {
		return
	}
	w := migrationCautionOut
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprint(w, liveSessionMigrationCaution(from, currentSchemaVersion, live))
}

// liveSessionMigrationCaution returns the caution text for a migration from
// version from to version to while live sessions are open.
func liveSessionMigrationCaution(from, to, live int) string {
	noun := "sessions"
	if live == 1 {
		noun = "session"
	}
	return fmt.Sprintf(
		"prism: CAUTION: after this command, run `prism restart` to restart the live sessions.\n"+
			"This command migrates the prism database from schema version %d to %d. "+
			"The database lists %d live %s.\n"+
			"If a live session started before this prism was installed, its sidecar still runs the old prism binary.\n"+
			"After the migration, these commands fail in that session: "+
			"prism review, prism escalate, prism prompt, and the cleanup post-hook.\n"+
			"If this command is `prism restart`, it restarts the sessions now, and no other step is necessary.\n",
		from, to, live, noun,
	)
}
