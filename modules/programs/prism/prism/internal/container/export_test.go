package container

// export_test.go — test-only export shims for unexported functions.
//
// These wrappers make unexported symbols accessible to external test packages
// (package container_test) without expanding the production API surface. This
// is the standard Go pattern for white-box testing via black-box test files.
//
// All declarations here are compiled only when running tests (the _test.go
// suffix ensures they are excluded from production builds).

// GithubAccountFromBareRootUncachedForTest is an exported wrapper around
// githubAccountFromBareRootUncached for use in timeout tests.
func GithubAccountFromBareRootUncachedForTest(bareRoot string) string {
	return githubAccountFromBareRootUncached(bareRoot)
}

// GithubAccountFromBareRootForTest is an exported wrapper around
// githubAccountFromBareRoot (cache-aware) for use in cache tests.
func GithubAccountFromBareRootForTest(bareRoot string) string {
	return githubAccountFromBareRoot(bareRoot)
}

// ClearGithubAccountCacheForTest deletes all entries from githubAccountCache.
// Call at the start of cache tests to ensure a clean slate.
func ClearGithubAccountCacheForTest() {
	githubAccountCache.Range(func(k, _ any) bool {
		githubAccountCache.Delete(k)
		return true
	})
}

// GitBareRootTimeoutForTest exposes gitBareRootTimeout for assertions.
const GitBareRootTimeoutForTest = gitBareRootTimeout

// RedactedArgsForTest is an exported wrapper around redactedArgs, for the
// external test files (package container_test). Every dump of a whole argv or
// env slice must go through it: a built argv carries live host credentials
// (issue #2581). See argv_redact_test.go for the full rationale.
func RedactedArgsForTest(args []string) []string {
	return redactedArgs(args)
}
