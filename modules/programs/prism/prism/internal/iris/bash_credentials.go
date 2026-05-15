package iris

// bash_credentials.go — thin shim over the D-7 CredentialBroker.
//
// Before D-7, this file owned bash credential resolution directly. D-7 moved
// the resolution logic into credential_broker.go so credential decisions live
// in a single place that also produces audit metadata.
//
// The bashEnv free function is preserved as a shim so the platform sandbox
// builders (bash_sandbox_{linux,darwin}.go) and the existing test suite
// continue to work unchanged when they only need the env list. New code that
// also needs the audit names should call CredentialBroker.ResolveBash
// directly instead — see harness_socket.go for the canonical caller.

// bashEnv returns the env-var list for a bash subprocess via the default
// CredentialBroker. It discards the audit names; use ResolveBash directly
// when those are needed.
//
// Kept as a free function so the platform sandbox builders can call it
// without plumbing a broker through every dispatcher field.
func bashEnv(role, bareRoot string) []string {
	return NewCredentialBroker().ResolveBash(role, bareRoot).Env
}
