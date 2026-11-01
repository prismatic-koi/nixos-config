//go:build darwin

package sandboxcheck

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"unsafe"
)

//go:cgo_import_dynamic libc_proc_listpids proc_listpids "/usr/lib/libSystem.B.dylib"

var libc_proc_listpids_trampoline_addr uintptr

// procRUIDOnly is PROC_RUID_ONLY of <sys/proc_info.h>: the processes whose
// real user ID is the type info.
const procRUIDOnly = 5

// ListPIDs returns the PID of each process whose real user ID is ruid,
// zombies included. It calls proc_listpids(3):
//
//	int proc_listpids(uint32_t type, uint32_t typeinfo, void *buffer, int buffersize);
//
// The kernel makes the list in one pass under its process list lock. So a
// process that runs for the whole call is in the list, also when it exits
// just after. The kern.proc sysctl is not usable here: it reads each
// process after the lock, and it leaves out a process that exits between.
//
// proc_listpids returns the bytes that it wrote, or 0 on an error. This
// process is always in a correct list, so ListPIDs returns an error for a
// list that does not hold it.
func ListPIDs(ruid int) ([]int, error) {
	n := 1024
	for {
		buf := make([]int32, n)
		r1, _, _ := syscall_syscall6(libc_proc_listpids_trampoline_addr,
			procRUIDOnly, uintptr(ruid), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*4), 0, 0)
		runtime.KeepAlive(buf)
		count := int(int32(r1)) / 4
		if count < n {
			if count <= 0 {
				return nil, errors.New("proc_listpids returned no process")
			}
			pids := make([]int, count)
			for i, p := range buf[:count] {
				pids[i] = int(p)
			}
			if ruid == os.Getuid() && !slices.Contains(pids, os.Getpid()) {
				return nil, errors.New("proc_listpids returned a list without this process")
			}
			return pids, nil
		}
		// A full buffer can hold only part of the list.
		n *= 2
	}
}
