//go:build darwin

package sandboxcheck

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sandbox_check has no public header. WebKit and Chromium declare it so:
//
//	int sandbox_check(pid_t pid, const char *operation, enum sandbox_filter_type type, ...);
//
// It returns 0 when the sandbox of pid allows the operation, 1 when the
// sandbox denies it, and -1 with errno on an error. A process with no
// sandbox is allowed each operation. With SANDBOX_FILTER_NONE and no
// operation, it returns 1 when pid runs in a sandbox.
//
// libSystem re-exports libsystem_sandbox, as golang.org/x/sys/unix uses it
// for the other libc functions. The trampoline is in sandboxcheck_darwin.s.

//go:cgo_import_dynamic libc_sandbox_check sandbox_check "/usr/lib/libSystem.B.dylib"

var libc_sandbox_check_trampoline_addr uintptr

// syscall6 is the libc call of package syscall, which golang.org/x/sys/unix
// also uses. With six arguments, it also puts the fourth argument on the
// stack: on darwin/arm64 a variadic argument is on the stack, and the path
// is the first variadic argument of sandbox_check.
//
//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// The values of enum sandbox_filter_type.
const (
	filterNone = 0
	filterPath = 1
)

// checkNoReport is the value of SANDBOX_CHECK_NO_REPORT. With it, a check
// that the sandbox denies writes no violation report to the system log.
// libsystem_sandbox exports the value as a constant, which a call without
// cgo cannot read, so the value is written here. A check that fails with
// the flag runs again without it (see Allows).
const checkNoReport = 0x40000000

// noReportFails is set when a check fails with checkNoReport and passes
// without it. Later checks then do not send the flag.
var noReportFails atomic.Bool

func sandboxCheck(pid int, op *byte, filter uintptr, path *byte) (int32, error) {
	r1, _, errno := syscall_syscall6(libc_sandbox_check_trampoline_addr,
		uintptr(pid), uintptr(unsafe.Pointer(op)), filter, uintptr(unsafe.Pointer(path)), 0, 0)
	runtime.KeepAlive(op)
	runtime.KeepAlive(path)
	if r := int32(r1); r != -1 {
		return r, nil
	}
	return -1, fmt.Errorf("sandbox_check of process %d: %w", pid, errno)
}

// Sandboxed reports whether process pid runs in a sandbox. The error wraps
// syscall.ESRCH when the process does not exist.
func Sandboxed(pid int) (bool, error) {
	r, err := sandboxCheck(pid, nil, filterNone, nil)
	if err != nil {
		return false, err
	}
	return r != 0, nil
}

// Allows reports whether the sandbox of process pid allows the operation
// op (for example "file-write-data") on path. It reports true for a
// process with no sandbox. The error wraps syscall.ESRCH when the process
// does not exist.
//
// Give a path that exists. sandbox_check denies each operation on a path
// that does not exist, also for a process with no sandbox.
func Allows(pid int, op, path string) (bool, error) {
	opPtr, err := unix.BytePtrFromString(op)
	if err != nil {
		return false, err
	}
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return false, err
	}
	if !noReportFails.Load() {
		r, err := sandboxCheck(pid, opPtr, filterPath|checkNoReport, pathPtr)
		if err == nil {
			return r == 0, nil
		}
		if errors.Is(err, syscall.ESRCH) {
			return false, err
		}
	}
	r, err := sandboxCheck(pid, opPtr, filterPath, pathPtr)
	if err != nil {
		return false, err
	}
	noReportFails.Store(true)
	return r == 0, nil
}
