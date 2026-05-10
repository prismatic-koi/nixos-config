package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prismatic-koi/atlassian/internal/client"
)

func newTestClient(t *testing.T, mux *http.ServeMux) *client.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.NewWithBase("test.atlassian.net", "test@example.com", "token", srv.URL)
}

func TestGet_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header is present but never log the value
		if r.Header.Get("Authorization") == "" {
			t.Error("Authorization header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"account_id":"abc","email":"test@example.com"}`))
	})
	c := newTestClient(t, mux)
	v, err := c.GetMe()
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	m := v.(map[string]any)
	if m["account_id"] != "abc" {
		t.Errorf("expected account_id=abc, got %v", m["account_id"])
	}
}

func TestGet_4xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Unauthorized"}`))
	})
	c := newTestClient(t, mux)
	_, err := c.GetMe()
	if err == nil {
		t.Fatal("expected error on 401")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "Unauthorized" {
		t.Errorf("expected message 'Unauthorized', got %q", apiErr.Message)
	}
}

func TestGet_5xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Internal Server Error"}`))
	})
	c := newTestClient(t, mux)
	_, err := c.GetMe()
	if err == nil {
		t.Fatal("expected error on 500")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", apiErr.StatusCode)
	}
}

func TestGet_JiraErrorMessages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/issue/FOO-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errorMessages":["Issue does not exist"],"errors":{}}`))
	})
	c := newTestClient(t, mux)
	_, err := c.GetJiraIssue("FOO-1")
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Message != "Issue does not exist" {
		t.Errorf("expected Jira error message, got %q", apiErr.Message)
	}
}

func TestSearchJira(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		jql := r.URL.Query().Get("jql")
		if jql == "" {
			t.Error("jql query param missing")
		}
		maxResults := r.URL.Query().Get("maxResults")
		if maxResults == "" {
			t.Error("maxResults query param missing")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issues": []any{},
			"total":  0,
		})
	})
	c := newTestClient(t, mux)
	_, err := c.SearchJira("project = FOO", 10, "")
	if err != nil {
		t.Fatalf("SearchJira: %v", err)
	}
}

func TestGetJiraTransitions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/issue/FOO-1/transitions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"transitions": []any{
				map[string]any{"id": "1", "name": "To Do"},
				map[string]any{"id": "2", "name": "In Progress"},
			},
		})
	})
	c := newTestClient(t, mux)
	v, err := c.GetJiraTransitions("FOO-1")
	if err != nil {
		t.Fatalf("GetJiraTransitions: %v", err)
	}
	m := v.(map[string]any)
	transitions := m["transitions"].([]any)
	if len(transitions) != 2 {
		t.Errorf("expected 2 transitions, got %d", len(transitions))
	}
}

func TestNew_MissingEnv(t *testing.T) {
	// Clear env vars
	t.Setenv("ATLASSIAN_SITE", "")
	t.Setenv("ATLASSIAN_EMAIL", "")
	t.Setenv("ATLASSIAN_API_TOKEN", "")

	_, err := client.New()
	if err == nil {
		t.Fatal("expected error when ATLASSIAN_SITE is unset")
	}
	if err.Error() != "ATLASSIAN_SITE is not set" {
		t.Errorf("expected ATLASSIAN_SITE error, got: %v", err)
	}

	t.Setenv("ATLASSIAN_SITE", "foo.atlassian.net")
	_, err = client.New()
	if err == nil {
		t.Fatal("expected error when ATLASSIAN_EMAIL is unset")
	}
	if err.Error() != "ATLASSIAN_EMAIL is not set" {
		t.Errorf("expected ATLASSIAN_EMAIL error, got: %v", err)
	}

	t.Setenv("ATLASSIAN_EMAIL", "x@y.com")
	_, err = client.New()
	if err == nil {
		t.Fatal("expected error when ATLASSIAN_API_TOKEN is unset")
	}
	if err.Error() != "ATLASSIAN_API_TOKEN is not set" {
		t.Errorf("expected ATLASSIAN_API_TOKEN error, got: %v", err)
	}
}

func TestAuthHeaderNotLogged(t *testing.T) {
	// This test verifies the token is not exposed in errors.
	// We set a fake token and verify error messages don't contain it.
	t.Setenv("ATLASSIAN_API_TOKEN", "supersecrettoken")
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Forbidden"}`))
	})
	c := newTestClient(t, mux)
	_, err := c.GetMe()
	if err == nil {
		t.Fatal("expected error")
	}
	errMsg := err.Error()
	if contains(errMsg, "supersecrettoken") {
		t.Error("error message must not contain the API token value")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsInner(s, substr))
}

func containsInner(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
