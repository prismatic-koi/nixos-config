package container

// pi_resume_cwd.go — the cwd check on a pi conversation resume (#3284).
//
// On resume, prism starts pi with `--session <id>`. pi then opens a session
// file and runs in the cwd from the header of that file, not in the cwd that
// prism gave it. A sandboxed session can write the session files in
// sessions/<encoded-cwd>/ of its own worktree, so it can write a header with
// any cwd. Without this check, a resume can start pi in a directory that the
// worker chose, and pi then loads the project resources of that directory.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/prismatic-koi/prism/internal/rundir"
)

// piHeaderLineLimit is the longest first line that the check accepts. pi
// finds a header by id only inside the first 1 MiB of a file
// (MAX_SESSION_HEADER_SCAN_BYTES in pi 1.4 dist/core/session-manager.js),
// but its list path reads the whole file. A real header line is less than
// 1 kB.
const piHeaderLineLimit = 64 << 10

// piSessionIDPattern is the set of session ids that prism resumes. It is
// the pi id rule (assertValidSessionId in pi 1.4) without '.'. pi reads a
// `--session` value that contains '/' or '\' or ends in ".jsonl" as a file
// path, so such a value can name any file.
var piSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]*[A-Za-z0-9])?$`)

// piSessionHeader holds the header fields that the cwd check uses.
type piSessionHeader struct {
	ID  string
	Cwd any
}

// CheckPIResumeCwd returns nil when `pi --session <cfg.HarnessSessionID>`,
// started in cfg.Worktree, opens a session file whose header cwd is
// cfg.Worktree. It returns an error that states the reason otherwise.
//
// pi 1.4 resolves the id in the sessions dir of its cwd: first a file whose
// header id is the id (findById), then a file whose header id starts with
// the id (list). Each lookup reads the header with its own parser:
// findById and open scan up to 1 MiB and skip blank lines, lines that are
// not JSON, and JSON null; list splits lines on "\r" too. So the check
// fails closed. It reads every *.jsonl entry in the dir, and it refuses the
// resume unless each entry is a regular file whose first line is one JSON
// object with no "\r", inside piHeaderLineLimit, that Go decodes with no
// error. pi writes every session file this way. For such a file, each pi
// parser takes the first line as the header.
//
// Then each header with type "session" must have a string id. Each header
// whose id starts with the id must have the worktree as its cwd. At least
// one header must have the exact id, so that pi does not search the
// sessions dirs of other projects. An empty file has no header for pi, so
// the check skips it.
func CheckPIResumeCwd(cfg Config) error {
	id := cfg.HarnessSessionID
	if !piSessionIDPattern.MatchString(id) {
		return fmt.Errorf("session id %q is not a pi session id", id)
	}
	if cfg.Worktree == "" {
		return errors.New("no worktree recorded")
	}
	root, ok := piResumeSessionsRoot(cfg)
	if !ok {
		return errors.New("cannot find the pi sessions dir")
	}
	worktree := filepath.Clean(cfg.Worktree)
	dir := filepath.Join(root, encodePiCWD(worktree))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	exact := false
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		h, isSession, err := readPISessionHeader(path)
		if err != nil {
			// %v, not %w: only a missing sessions dir is a not-exist error
			// for the caller (restoreResumeSessionID).
			return fmt.Errorf("session file %q: %v", path, err)
		}
		if !isSession || !strings.HasPrefix(h.ID, id) {
			continue
		}
		cwd, isString := h.Cwd.(string)
		if !isString || filepath.Clean(cwd) != worktree || !filepath.IsAbs(cwd) {
			return fmt.Errorf("session file %q has cwd %s in its header, not the worktree %q",
				path, quoteHeaderValue(h.Cwd), worktree)
		}
		if h.ID == id {
			exact = true
		}
	}
	if !exact {
		return fmt.Errorf("no session file in %q has the id %s in its header", dir, id)
	}
	return nil
}

// readPISessionHeader reads the first line of the session file at path.
// isSession is true when the line is a pi session header. err is not nil
// when the file does not have the shape that pi writes, so that the pi
// parsers can read a header that differs from the first line: see
// CheckPIResumeCwd. An empty file is not a session and is not an error.
func readPISessionHeader(path string) (h piSessionHeader, isSession bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return h, false, err
	}
	defer f.Close()
	if fi, statErr := f.Stat(); statErr != nil || !fi.Mode().IsRegular() {
		return h, false, errors.New("not a regular file")
	}
	r := bufio.NewReaderSize(io.LimitReader(f, piHeaderLineLimit+1), piHeaderLineLimit+1)
	line, readErr := r.ReadSlice('\n')
	switch {
	case readErr == nil:
		line = line[:len(line)-1]
	case errors.Is(readErr, bufio.ErrBufferFull), len(line) > piHeaderLineLimit:
		return h, false, errors.New("the first line is too long")
	case readErr != io.EOF:
		return h, false, readErr
	case len(line) == 0:
		return h, false, nil
	}
	return parsePISessionHeaderLine(line)
}

// parsePISessionHeaderLine parses the first line of a session file with the
// rules of readPISessionHeader.
func parsePISessionHeaderLine(line []byte) (h piSessionHeader, isSession bool, err error) {
	if bytes.IndexByte(line, '\r') >= 0 {
		return h, false, errors.New("the first line holds a carriage return")
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return h, false, errors.New("the first line is blank")
	}
	var entry map[string]any
	if jsonErr := json.Unmarshal(line, &entry); jsonErr != nil || entry == nil {
		return h, false, errors.New("the first line is not a JSON object that prism can check")
	}
	if typ, _ := entry["type"].(string); typ != "session" {
		return h, false, nil
	}
	id, idIsString := entry["id"].(string)
	if !idIsString {
		return h, false, errors.New("the header has no string id")
	}
	return piSessionHeader{ID: id, Cwd: entry["cwd"]}, true, nil
}

// piLogResumeRefusal appends a warning line for a refused resume to the
// agent-run log of the session. Like piLogResumeWarning, it is best-effort.
func piLogResumeRefusal(cfg Config, reason error) {
	if cfg.SessionName == "" {
		return
	}
	logPath, err := piAgentRunLogPath(cfg.SessionName)
	if err != nil {
		return
	}
	f, err := rundir.OpenAppend(logPath)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[agent-run] warning: not resuming pi session %s: %v — starting fresh conversation\n",
		cfg.HarnessSessionID, reason)
}

// quoteHeaderValue renders a header value for an error message.
func quoteHeaderValue(v any) string {
	if v == nil {
		return "(none)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
