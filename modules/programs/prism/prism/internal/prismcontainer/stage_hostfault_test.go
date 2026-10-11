package prismcontainer

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestIsHostFaultENOSPC(t *testing.T) {
	err := &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}
	if !isHostFault(err) {
		t.Fatal("ENOSPC must be a host fault")
	}
	he := &hostError{fmt.Errorf("copy the build context: %w", err)}
	if !strings.Contains(he.Error(), "no space left on device") {
		t.Fatalf("error does not name ENOSPC: %v", he)
	}
	if isHostFault(fs.ErrNotExist) {
		t.Fatal("a missing file is a change, not a host fault")
	}
}
