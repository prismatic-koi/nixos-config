package prismcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// Audit decisions.
const (
	DecisionAllowed = "allowed"
	DecisionRefused = "refused"
	DecisionError   = "error"
)

// AuditEntry is one line of the audit log. The log never holds an --env
// or --build-arg value, because a value can be a secret. It holds the key
// only.
type AuditEntry struct {
	Time       string `json:"time"`
	Session    string `json:"session"`
	InstanceID string `json:"instance_id"`
	Command    string `json:"command"`
	// Image is the image of a run, or the name a build gives its image.
	Image          string   `json:"image"`
	Args           []string `json:"args,omitempty"`
	EnvKeys        []string `json:"env_keys,omitempty"`
	Context        string   `json:"context,omitempty"`
	File           string   `json:"file,omitempty"`
	BuildArgKeys   []string `json:"build_arg_keys,omitempty"`
	Mount          string   `json:"mount,omitempty"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
	Container      string   `json:"container,omitempty"`
	Decision       string   `json:"decision"`
	Reason         string   `json:"reason,omitempty"`
	// ExitCode is null when no container command ran to completion.
	ExitCode *int `json:"exit_code"`
}

// auditLog appends entries to the audit log of one session incarnation.
type auditLog struct{ f *os.File }

// openAuditLog opens the audit log in append mode and creates its directory.
// The directory is created before podman runs, so cleanup can use it as
// proof that the incarnation used `prism container`.
func openAuditLog(instanceID string) (*auditLog, error) {
	path, err := container.PrismContainerAuditLogPath(instanceID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &auditLog{f: f}, nil
}

// write appends e as one JSON line. A single write call keeps the line
// whole when two requests of one session append at the same time.
func (a *auditLog) write(e AuditEntry, now time.Time) error {
	e.Time = now.UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = a.f.Write(append(line, '\n'))
	return err
}

func (a *auditLog) close() { _ = a.f.Close() }

// AppendAuditLog appends the audit log of incarnation from to the audit log
// of incarnation to. Cleanup uses it to keep the record of an incarnation
// or a review agent that has no archive of its own: the lines go into a log
// that cleanup archives. Each line names its own session and instance ID.
// A missing source log is not an error. After a nil return the caller can
// remove the source directory.
func AppendAuditLog(from, to string) error {
	src, err := container.PrismContainerAuditLogPath(from)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(data) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	dst, err := openAuditLog(to)
	if err != nil {
		return err
	}
	defer dst.close()
	_, err = dst.f.Write(data)
	return err
}
