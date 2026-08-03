package payload

// Test-only hooks for the redactor internals (issue #2589).
//
// The shape layer runs behind a literal prefilter for cost. The prefilter is
// only safe if every trigger is a NECESSARY substring of its pattern, so the
// tests need a way to run the shape layer WITHOUT the prefilter and compare.

// RedactShapesNoPrefilterForTest applies the shape layer directly, skipping
// the prefilter. A result that differs from Redact's shape layer means a
// trigger is not a necessary substring of its pattern.
func RedactShapesNoPrefilterForTest(s string) string {
	return combinedShapeRE.ReplaceAllStringFunc(s, func(m string) string {
		return RedactionMarker(shapeNameFor(m))
	})
}

// ShapeTriggerPresentForTest exposes the prefilter decision.
func ShapeTriggerPresentForTest(s string) bool { return shapeTriggerPresent(s) }
