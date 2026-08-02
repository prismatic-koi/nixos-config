package sidecar

// Tests for the host-API request-body size cap (issue #1848).
//
// Every POST handler in host_api.go now wraps r.Body in
// http.MaxBytesReader via the package-local decodeRequestJSON helper. The
// default cap is 1 MiB; /prompt is bumped to 16 MiB because worker spawn
// prompts may legitimately carry file attachments and large context.
//
// AC #3 picks the more informative status code: when the cap is exceeded the
// handler returns 413 Request Entity Too Large. (We can distinguish a cap
// overflow from any other decode error because the runtime wraps the
// overflow in *http.MaxBytesError before the json.Decoder ever sees it.)
//
// AC #5 requires that the test post a >cap body to each affected endpoint
// and assert the documented status. We exercise every route covered by the
// helper change.

import (
	"net/http"
	"strings"
	"testing"
)

// oversizeJSONPayload returns a JSON object whose serialised size is strictly
// greater than capBytes — the body MUST exceed the supplied cap.
//
// The payload shape is {"junk":"<padding>"}. The field name `junk` is
// deliberately unknown to every host-API request schema, so this body cannot
// reach business logic regardless of the cap outcome — it either trips
// http.MaxBytesReader first (status 413, which is what we assert) or, if it
// were small enough to fit, would trip DisallowUnknownFields (status 400).
//
// With ≥128 bytes of headroom past the cap the MaxBytesReader trips first:
// the decoder must stream the entire quoted-string value to learn what is
// inside `"junk"`, and that streaming exceeds the cap well before the
// closing quote arrives. Empirically verified against encoding/json +
// net/http on Go 1.26.
//
// The padding string is built once, in memory, so a 17 MiB payload takes
// ~17 MiB of heap during the test — bounded and well inside the test
// runner's budget.
func oversizeJSONPayload(t *testing.T, capBytes int) string {
	t.Helper()
	// The JSON wrapper {"junk":""} is 11 bytes; pad past the cap with 128
	// bytes of headroom so the test is insensitive to small framing
	// off-by-ones and so the MaxBytesReader trips reliably ahead of the
	// decoder's unknown-field check.
	const (
		wrapperBytes = 11
		headroom     = 128
	)
	pad := strings.Repeat("x", capBytes-wrapperBytes+headroom)
	return `{"junk":"` + pad + `"}`
}

// TestHostAPI_BodyCap_Default verifies that every POST handler bound to the
// 1 MiB default body cap rejects a >1 MiB body with HTTP 413. The case table
// covers every route except /prompt (which has its own 16 MiB cap and is
// tested in TestHostAPI_BodyCap_Prompt below).
func TestHostAPI_BodyCap_Default(t *testing.T) {
	d := openTestDB(t)
	// Use a coordinator session so the handlers that gate on
	// requireCoordinator (e.g. /spawn, /merge, /escalate, /investigate)
	// reach the decode step before the role check fires. The body-cap
	// behaviour we are asserting lives in decodeRequestJSON, which runs
	// before the role check inside each handler.
	sc := newSidecarCoordinatorWithInstance(t, "test-repo@main", "test-repo",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", d)

	// Slightly over the 1 MiB cap (oversizeJSONPayload adds 128 bytes of
	// headroom to ensure MaxBytesReader trips ahead of the decoder's
	// unknown-field check).
	body := oversizeJSONPayload(t, 1<<20)

	// Every route bound to defaultMaxBodyBytes. /prompt is tested separately
	// because it uses promptMaxBodyBytes (16 MiB).
	routes := []string{
		"/spawn",
		"/review",
		"/cleanup",
		"/switch",
		"/merge",
		"/merges/cancel",
		"/event",
		"/feedback",
		"/usage/snapshot",
		"/escalate",
		"/investigate",
		"/set-model",
		"/apply-profile",
		"/register-provider",
		"/register-provider-direct",
		"/set-active-tools",
		"/abort",
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			rr := doHostAPI(t, sc, http.MethodPost, route, body)
			if rr.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("POST %s with >1 MiB body: status = %d, body = %q, want 413",
					route, rr.Code, truncateForLog(rr.Body.String(), 200))
			}
		})
	}
}

// TestHostAPI_BodyCap_Prompt verifies the elevated /prompt cap. The endpoint
// must:
//
//  1. Reject a body just over 16 MiB with HTTP 413 (cap enforcement).
//  2. Accept a body well over the 1 MiB default (proving the cap really is
//     higher for /prompt; we use 2 MiB which is in the middle of the legal
//     window).
//
// The "accept" assertion here is "decode succeeds" — i.e. the handler reaches
// the next layer of validation (which then 400s on missing fields, etc.).
// What we explicitly do NOT want to see for the 2 MiB body is a 413.
func TestHostAPI_BodyCap_Prompt(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "test-repo@main", "test-repo",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", d)

	// Slightly over the 16 MiB /prompt cap → must 413.
	overBody := oversizeJSONPayload(t, 16<<20)
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", overBody)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /prompt with >16 MiB body: status = %d, body = %q, want 413",
			rr.Code, truncateForLog(rr.Body.String(), 200))
	}

	// 2 MiB body — well above the 1 MiB default, well below the 16 MiB cap.
	// We deliberately use a valid schema-shaped body so the cap is the only
	// gate we exercise: any unknown field would also be rejected with 400
	// (DisallowUnknownFields), which would muddle the signal.
	pad := strings.Repeat("x", (2<<20)-64)
	okBody := `{"session":"test-repo@main","prompt":"` + pad + `"}`
	rr = doHostAPI(t, sc, http.MethodPost, "/prompt", okBody)
	if rr.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("POST /prompt with 2 MiB body: got 413, want decode to succeed "+
			"(any non-413 status; body = %q)", truncateForLog(rr.Body.String(), 200))
	}
}

// TestHostAPI_BodyCap_DisallowUnknownFields verifies that every POST handler
// has DisallowUnknownFields enabled (AC #4). A small valid-shape body with one
// stray field must return 400 across the board.
func TestHostAPI_BodyCap_DisallowUnknownFields(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "test-repo@main", "test-repo",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", d)

	// The strayField is unknown to every host-API request schema.
	const body = `{"strayUnknownField_issue1848":"reject me"}`

	routes := []string{
		"/prompt",
		"/spawn",
		"/review",
		"/cleanup",
		"/switch",
		"/merge",
		"/merges/cancel",
		"/event",
		"/feedback",
		"/usage/snapshot",
		"/escalate",
		"/investigate",
		"/set-model",
		"/apply-profile",
		"/register-provider",
		"/register-provider-direct",
		"/set-active-tools",
		"/abort",
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			rr := doHostAPI(t, sc, http.MethodPost, route, body)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("POST %s with stray unknown field: status = %d, body = %q, want 400 (DisallowUnknownFields)",
					route, rr.Code, truncateForLog(rr.Body.String(), 200))
			}
			if !strings.Contains(rr.Body.String(), "unknown field") {
				t.Errorf("POST %s with stray unknown field: body = %q, want error mentioning \"unknown field\"",
					route, truncateForLog(rr.Body.String(), 200))
			}
		})
	}
}

// truncateForLog clips long bodies so test failure messages stay readable.
// Used only for diagnostic output; not part of any assertion.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}
