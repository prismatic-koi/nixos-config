//go:build darwin

#include "textflag.h"

// The trampolines of sandbox_check and proc_listpids, in the form of
// golang.org/x/sys/unix.

TEXT libc_sandbox_check_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sandbox_check(SB)

GLOBL	·libc_sandbox_check_trampoline_addr(SB), RODATA, $8
DATA	·libc_sandbox_check_trampoline_addr(SB)/8, $libc_sandbox_check_trampoline<>(SB)

TEXT libc_proc_listpids_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_proc_listpids(SB)

GLOBL	·libc_proc_listpids_trampoline_addr(SB), RODATA, $8
DATA	·libc_proc_listpids_trampoline_addr(SB)/8, $libc_proc_listpids_trampoline<>(SB)
