package promptdelivery

// HasPathPrefix exposes the unexported hasPathPrefix helper for table-driven
// testing from the external _test package. Not part of the public API.
var HasPathPrefix = hasPathPrefix

// SetSidecarHostAPIPathFn swaps the package-level socket-path resolver for the
// duration of a test, returning a function that restores the original. Used
// only by tests that need to inject a sockPath outside $XDG_STATE_HOME so they
// can exercise the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard (issue #1883).
func SetSidecarHostAPIPathFn(fn func(string) (string, error)) (restore func()) {
	prev := sidecarHostAPIPathFn
	sidecarHostAPIPathFn = fn
	return func() { sidecarHostAPIPathFn = prev }
}
