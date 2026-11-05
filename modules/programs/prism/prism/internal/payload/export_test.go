package payload

// Test-only hooks for the redactor internals.
//
// The shape layer runs behind a literal prefilter for cost. The prefilter is
// only safe if every trigger is a NECESSARY substring of its pattern, so the
// tests need a way to run the shape layer WITHOUT the prefilter and compare.

// RedactShapesNoPrefilterForTest applies the shape layer directly, skipping
// the prefilter of both passes. A result that differs from Redact's shape
// layer means a trigger is not a necessary substring of its pattern.
func RedactShapesNoPrefilterForTest(s string) string {
	return combinedShapeRE.ReplaceAllStringFunc(redactPrivateKeyBlocks(s), func(m string) string {
		return RedactionMarker(shapeNameFor(m))
	})
}

// ShapeTriggerPresentForTest reports whether the prefilter of either pass
// fires.
func ShapeTriggerPresentForTest(s string) bool {
	return privateKeyTriggerPresent(s) || shapeTriggerPresent(s)
}

// RedactPrivateKeyBlocksForTest runs the private-key pass alone.
func RedactPrivateKeyBlocksForTest(s string) string { return redactPrivateKeyBlocks(s) }

// PrivateKeyShapeForTest returns the private-key shape rule.
func PrivateKeyShapeForTest() (name, begin, end string, triggers []string) {
	return privateKeyShape.name, privateKeyShape.begin, privateKeyShape.end,
		append([]string(nil), privateKeyShape.triggers...)
}
