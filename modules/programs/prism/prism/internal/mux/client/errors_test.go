package client

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestClientError_Error verifies the Error() rendering covers both
// the "structured code present" and "fallback / no code" branches.
// A test like this guards against an inadvertent format-string
// change that breaks log parsers downstream.
func TestClientError_Error(t *testing.T) {
	cases := []struct {
		name string
		in   *ClientError
		want string
	}{
		{
			name: "with code",
			in: &ClientError{
				HTTPStatus: 404,
				Code:       CodeSessionNotFound,
				Message:    `session "x" not found`,
			},
			want: `mux: session_not_found (HTTP 404): session "x" not found`,
		},
		{
			name: "no code (non-JSON body)",
			in: &ClientError{
				HTTPStatus: 502,
				Message:    "non-JSON error body (HTTP 502): bad gateway",
			},
			want: "mux: HTTP 502: non-JSON error body (HTTP 502): bad gateway",
		},
		{
			name: "nil receiver",
			in:   nil,
			want: "<nil ClientError>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientError_Is exhaustively covers the codeToSentinel mapping
// from inside the package — every Code* constant must round-trip to
// its Err* sentinel via errors.Is. This is the unit-level partner
// to TestErrors_SentinelMapping in client_test.go, which exercises
// the same mapping end-to-end through a real server.
func TestClientError_Is(t *testing.T) {
	pairs := []struct {
		code     string
		sentinel error
	}{
		{CodeMethodNotAllowed, ErrMethodNotAllowed},
		{CodeBadRequest, ErrBadRequest},
		{CodeInternal, ErrInternal},
		{CodeSessionExists, ErrSessionExists},
		{CodeSessionNotFound, ErrSessionNotFound},
		{CodeParentNotFound, ErrParentNotFound},
		{CodeParentIsReview, ErrParentIsReview},
		{CodeInvalidSession, ErrInvalidSession},
		{CodePaneExists, ErrPaneExists},
		{CodePaneNotFound, ErrPaneNotFound},
		{CodeNoPanes, ErrNoPanes},
	}
	for _, p := range pairs {
		t.Run(p.code, func(t *testing.T) {
			ce := &ClientError{Code: p.code, HTTPStatus: 400, Message: "x"}
			if !errors.Is(ce, p.sentinel) {
				t.Errorf("errors.Is(ClientError{Code:%q}, %v) = false, want true",
					p.code, p.sentinel)
			}
		})
	}
}

// TestClientError_Is_OtherClientError asserts the
// "*ClientError-with-same-Code" branch of Is. This lets tests
// pattern-match against a ClientError target without needing one of
// the prebuilt sentinels:
//
//	want := &ClientError{Code: CodePaneNotFound}
//	if !errors.Is(err, want) { ... }
func TestClientError_Is_OtherClientError(t *testing.T) {
	got := &ClientError{Code: CodePaneNotFound, HTTPStatus: 404, Message: "x"}
	want := &ClientError{Code: CodePaneNotFound}
	if !errors.Is(got, want) {
		t.Errorf("errors.Is on matching Code = false, want true")
	}
	// Mismatched codes do NOT match.
	mismatch := &ClientError{Code: CodeSessionNotFound}
	if errors.Is(got, mismatch) {
		t.Errorf("errors.Is on mismatched Code = true, want false")
	}
	// A ClientError with empty Code never matches via this branch
	// (avoids "any ClientError matches any ClientError-target with no
	// Code" false positives).
	empty := &ClientError{}
	if errors.Is(got, empty) {
		t.Errorf("errors.Is on empty-Code target = true, want false")
	}
}

// TestClientError_Is_NilReceiver guards the documented "nil
// ClientError never matches anything" behaviour.
func TestClientError_Is_NilReceiver(t *testing.T) {
	var ce *ClientError
	if errors.Is(ce, ErrSessionNotFound) {
		t.Error("nil ClientError matched a sentinel")
	}
}

// TestDecodeErrorBody covers the three branches of decodeErrorBody:
// well-formed JSON, malformed body, and empty body. The function is
// the hot path for every 4xx/5xx response, so each branch must be
// pinned.
func TestDecodeErrorBody(t *testing.T) {
	t.Run("structured json", func(t *testing.T) {
		body := []byte(`{"code":"pane_not_found","message":"missing","data":{"x":1}}`)
		ce := decodeErrorBody(404, body)
		if ce.HTTPStatus != 404 {
			t.Errorf("HTTPStatus = %d, want 404", ce.HTTPStatus)
		}
		if ce.Code != CodePaneNotFound {
			t.Errorf("Code = %q, want %q", ce.Code, CodePaneNotFound)
		}
		if ce.Message != "missing" {
			t.Errorf("Message = %q, want %q", ce.Message, "missing")
		}
		// Data is held as RawMessage — the original JSON should
		// survive verbatim.
		var data map[string]any
		if err := json.Unmarshal(ce.Data, &data); err != nil {
			t.Fatalf("decode Data: %v", err)
		}
		if data["x"] != float64(1) {
			t.Errorf("Data[x] = %v, want 1", data["x"])
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		ce := decodeErrorBody(500, []byte("not json"))
		if ce.HTTPStatus != 500 {
			t.Errorf("HTTPStatus = %d, want 500", ce.HTTPStatus)
		}
		if ce.Code != "" {
			t.Errorf("Code = %q, want empty (non-JSON body)", ce.Code)
		}
		if ce.Message == "" {
			t.Errorf("Message empty, want explanatory fallback")
		}
	})

	t.Run("empty body", func(t *testing.T) {
		ce := decodeErrorBody(502, nil)
		if ce.HTTPStatus != 502 {
			t.Errorf("HTTPStatus = %d, want 502", ce.HTTPStatus)
		}
		if ce.Code != "" {
			t.Errorf("Code = %q, want empty (no body)", ce.Code)
		}
		if ce.Message == "" {
			t.Errorf("Message empty, want synthetic fallback")
		}
	})

	t.Run("missing data field", func(t *testing.T) {
		// Some endpoints (e.g. 400/bad_request without context)
		// omit the data field. Decoding must succeed and leave
		// Data nil.
		body := []byte(`{"code":"bad_request","message":"x"}`)
		ce := decodeErrorBody(400, body)
		if ce.Code != CodeBadRequest {
			t.Errorf("Code = %q, want %q", ce.Code, CodeBadRequest)
		}
		if len(ce.Data) != 0 {
			t.Errorf("Data = %s, want empty", ce.Data)
		}
	})
}
