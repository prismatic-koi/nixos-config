// Package sandboxcheck asks the macOS kernel about the sandbox of a
// process: whether the process runs in a sandbox, and whether its sandbox
// allows an operation on a path. It calls sandbox_check(3) of
// libsystem_sandbox, with no cgo. It also lists the processes of a user in
// one atomic call (proc_listpids).
//
// The package has code on Darwin only. prism uses it to find the processes
// of a sandbox-exec session that left the process group of the agent
// (#3375, internal/session/sandbox_procs_darwin.go).
package sandboxcheck
