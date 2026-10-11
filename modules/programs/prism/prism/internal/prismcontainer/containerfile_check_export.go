package prismcontainer

// CheckContainerfile runs the Containerfile transport check on the content
// of a Containerfile. The differential fuzz test in parserdiff, a separate
// Go module, is the only caller outside this package.
func CheckContainerfile(data []byte) error {
	return checkContainerfile(data)
}
