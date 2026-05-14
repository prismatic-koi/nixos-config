// Package investigate provides shared helpers for the investigate command and
// the host-API /investigate endpoint.
package investigate

import (
	"fmt"
	"strings"
)

// ValidateName returns an error if the user-supplied --name value violates
// the slug constraints:
//   - Only lowercase alphanumerics and dashes ([a-z0-9-]).
//   - Must not start or end with a dash.
//   - Maximum 40 characters.
func ValidateName(name string) error {
	if len(name) > 40 {
		return fmt.Errorf("prism investigate: --name %q is %d characters; maximum is 40", name, len(name))
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("prism investigate: --name %q must not start or end with a dash", name)
	}
	var bad []rune
	seen := map[rune]bool{}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			if !seen[r] {
				bad = append(bad, r)
				seen[r] = true
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("prism investigate: --name %q contains disallowed characters: %q (only [a-z0-9-] is allowed)", name, string(bad))
	}
	return nil
}
