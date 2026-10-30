package hostapiauth

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestHostAPIAuth_ExtensionUsesSameValues: the pi extension sends the
// credential with the header name and reads the file name of this package
// (pi/extensions/anthropic-oauth/host-api-auth.ts). If one side changes
// alone, the sidecar refuses each request of the extension.
func TestHostAPIAuth_ExtensionUsesSameValues(t *testing.T) {
	const ext = "../../../pi/extensions/anthropic-oauth/host-api-auth.ts"
	src, err := os.ReadFile(ext)
	if os.IsNotExist(err) {
		t.Skipf("%s not present (expected in the nix build sandbox)", ext)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`export const HOST_API_SECRET_HEADER = ` + strconv.Quote(strings.ToLower(Header)),
		`export const HOST_API_SECRET_FILE = ` + strconv.Quote(SecretFileName),
	} {
		if !strings.Contains(string(src), line) {
			t.Errorf("%s does not hold %q", ext, line)
		}
	}
}
