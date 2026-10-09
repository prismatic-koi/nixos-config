package prismcontainer

import (
	"fmt"
	"io"
	"sync"
)

// OutputLimit is the number of bytes prism keeps of each output stream.
const OutputLimit = 1 << 20

// tailBuffer keeps the last limit bytes written to it and counts the bytes
// it dropped.
type tailBuffer struct {
	mu      sync.Mutex
	limit   int
	buf     []byte
	dropped int64
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.limit {
		t.dropped += int64(len(t.buf) + n - t.limit)
		t.buf = append(t.buf[:0], p[n-t.limit:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.limit; over > 0 {
		t.dropped += int64(over)
		kept := copy(t.buf, t.buf[over:])
		t.buf = t.buf[:kept]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) snapshot() ([]byte, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...), t.dropped
}

// WriteResult writes a RunResult to the streams of the `prism container run`
// process. Both routes use it, so the output does not depend on the route.
// The truncation notes go to stderr first, so that stdout holds container
// output only.
func WriteResult(stdout, stderr io.Writer, r RunResult) {
	if r.StdoutDropped > 0 {
		fmt.Fprintf(stderr, "prism container run: stdout was truncated: it was longer than 1 MiB, so prism kept the last 1 MiB and dropped the first %d bytes\n", r.StdoutDropped)
	}
	if r.StderrDropped > 0 {
		fmt.Fprintf(stderr, "prism container run: stderr was truncated: it was longer than 1 MiB, so prism kept the last 1 MiB and dropped the first %d bytes\n", r.StderrDropped)
	}
	_, _ = stdout.Write(r.Stdout)
	_, _ = stderr.Write(r.Stderr)
	if r.Message != "" {
		fmt.Fprintf(stderr, "prism container run: %s\n", r.Message)
	}
}
