package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// readBodyFile reads a body from the given file path.
// If path is "-", reads from stdin.
// Returns an error if the resulting content is empty.
func readBodyFile(path string) ([]byte, error) {
	var data []byte
	var err error

	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	} else {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read file %q: %w", path, err)
		}
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("empty body: no content to send")
	}
	return data, nil
}

// readBodyRaw reads raw bytes from stdin (used for --adf and --storage flags).
// Returns an error if the content is empty.
func readBodyRaw(source string) ([]byte, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", source, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("empty body: no content to send")
	}
	return data, nil
}
