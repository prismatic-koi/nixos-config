package payload

// Test-only hooks for the redactor internals.
//
// The shape layer runs behind a literal prefilter for cost. The prefilter is
// only safe if every trigger is a NECESSARY substring of its pattern, so the
// tests need a way to run the shape layer WITHOUT the prefilter and compare.

// RedactShapesNoPrefilterForTest applies the shape layer directly, skipping
// the prefilter of each pass. A result that differs from Redact's shape
// layer means a trigger is not a necessary substring of its pattern.
func RedactShapesNoPrefilterForTest(s string) string {
	s = combinedShapeRE.ReplaceAllStringFunc(redactPrivateKeyBlocks(s), func(m string) string {
		return RedactionMarker(shapeNameFor(m))
	})
	return redactContextShapes(s, false)
}

// ShapeTriggerPresentForTest reports whether the prefilter of any pass
// fires.
func ShapeTriggerPresentForTest(s string) bool {
	if privateKeyTriggerPresent(s) || shapeTriggerPresent(s) {
		return true
	}
	for i := range contextShapes {
		if contextShapes[i].triggerPresent(s) {
			return true
		}
	}
	return false
}

// RedactContextShapesForTest runs the context shapes alone, with the
// prefilter.
func RedactContextShapesForTest(s string) string { return redactContextShapes(s, true) }

// ContainsFoldASCIIForTest is containsFoldASCII.
func ContainsFoldASCIIForTest(s, t string) bool { return containsFoldASCII(s, t) }

// RedactPrivateKeyBlocksForTest runs the private-key pass alone.
func RedactPrivateKeyBlocksForTest(s string) string { return redactPrivateKeyBlocks(s) }

// PrivateKeyShapeForTest returns the private-key shape rule.
func PrivateKeyShapeForTest() (name, begin, end string, triggers []string) {
	return privateKeyShape.name, privateKeyShape.begin, privateKeyShape.end,
		append([]string(nil), privateKeyShape.triggers...)
}
