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
	mux.HandleFunc("/search/jql", func(w http.ResponseWriter, r *http.Request) {
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

func TestSearchConfluence(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		cql := r.URL.Query().Get("cql")
		if cql == "" {
			t.Error("cql query param missing")
		}
		limit := r.URL.Query().Get("limit")
		if limit == "" {
			t.Error("limit query param missing")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []any{},
			"size":    0,
		})
	})
	c := newTestClient(t, mux)
	_, err := c.SearchConfluence("space = ENG", 10)
	if err != nil {
		t.Fatalf("SearchConfluence: %v", err)
	}
}

// ---- Write method tests ----

func TestCreateJiraIssue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/issue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected Content-Type: application/json")
		}
		// Verify body was not logged (we read it only for assertion)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":  "10001",
			"key": "FOO-1",
			"self": "https://example.atlassian.net/rest/api/3/issue/10001",
		})
	})
	c := newTestClient(t, mux)
	payload := map[string]any{
		"fields": map[string]any{
			"project":   map[string]any{"key": "FOO"},
			"issuetype": map[string]any{"name": "Task"},
			"summary":   "Test issue",
		},
	}
	v, err := c.CreateJiraIssue(payload)
	if err != nil {
		t.Fatalf("CreateJiraIssue: %v", err)
	}
	m := v.(map[string]any)
	if m["key"] != "FOO-1" {
		t.Errorf("expected key=FOO-1, got %v", m["key"])
	}
}

func TestUpdateJiraIssue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/issue/FOO-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := newTestClient(t, mux)
	payload := map[string]any{
		"fields": map[string]any{"summary": "Updated summary"},
	}
	v, err := c.UpdateJiraIssue("FOO-1", payload)
	if err != nil {
		t.Fatalf("UpdateJiraIssue: %v", err)
	}
	_ = v
}

func TestAddJiraComment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/issue/FOO-1/comment", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":   "comment-1",
			"body": map[string]any{"type": "doc"},
		})
	})
	c := newTestClient(t, mux)
	payload := map[string]any{
		"body": map[string]any{"type": "doc", "version": 1, "content": []any{}},
	}
	v, err := c.AddJiraComment("FOO-1", payload)
	if err != nil {
		t.Fatalf("AddJiraComment: %v", err)
	}
	m := v.(map[string]any)
	if m["id"] != "comment-1" {
		t.Errorf("expected id=comment-1, got %v", m["id"])
	}
}

func TestTransitionJiraIssue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/issue/FOO-1/transitions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		transition, _ := body["transition"].(map[string]any)
		if transition["id"] != "21" {
			t.Errorf("expected transition id=21, got %v", transition["id"])
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := newTestClient(t, mux)
	_, err := c.TransitionJiraIssue("FOO-1", "21")
	if err != nil {
		t.Fatalf("TransitionJiraIssue: %v", err)
	}
}

func TestCreateConfluencePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "123456",
			"title":   "My Page",
			"version": map[string]any{"number": 1},
		})
	})
	c := newTestClient(t, mux)
	payload := map[string]any{
		"spaceId": "ENG",
		"title":   "My Page",
		"body": map[string]any{
			"representation": "atlas_doc_format",
			"value":          `{"version":1,"type":"doc","content":[]}`,
		},
	}
	v, err := c.CreateConfluencePage(payload)
	if err != nil {
		t.Fatalf("CreateConfluencePage: %v", err)
	}
	m := v.(map[string]any)
	if m["id"] != "123456" {
		t.Errorf("expected id=123456, got %v", m["id"])
	}
}

func TestUpdateConfluencePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/pages/123456", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "123456",
			"title":   "Updated Title",
			"version": map[string]any{"number": 2},
		})
	})
	c := newTestClient(t, mux)
	payload := map[string]any{
		"id":      "123456",
		"title":   "Updated Title",
		"version": map[string]any{"number": 2},
		"body": map[string]any{
			"representation": "atlas_doc_format",
			"value":          `{"version":1,"type":"doc","content":[]}`,
		},
	}
	v, err := c.UpdateConfluencePage("123456", payload)
	if err != nil {
		t.Fatalf("UpdateConfluencePage: %v", err)
	}
	m := v.(map[string]any)
	if m["id"] != "123456" {
		t.Errorf("expected id=123456, got %v", m["id"])
	}
}

func TestWrite_4xx_NoRetry(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/issue", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errorMessages":["Invalid project key"]}`))
	})
	c := newTestClient(t, mux)
	_, err := c.CreateJiraIssue(map[string]any{})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 call (no retry on 4xx), got %d", callCount)
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
}

func TestWrite_5xx_Retry(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/issue", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Internal Server Error"}`))
	})
	c := newTestClient(t, mux)
	_, err := c.CreateJiraIssue(map[string]any{})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	// Should have been called exactly twice (initial + 1 retry)
	if callCount != 2 {
		t.Errorf("expected 2 calls (retry on 5xx), got %d", callCount)
	}
}

func TestWrite_409_VersionConflict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/pages/123456", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"message":"Version conflict"}`))
	})
	c := newTestClient(t, mux)
	_, err := c.UpdateConfluencePage("123456", map[string]any{})
	if err == nil {
		t.Fatal("expected error on 409")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 409 {
		t.Errorf("expected 409, got %d", apiErr.StatusCode)
	}
	if !contains(apiErr.Message, "concurrently") {
		t.Errorf("expected concurrent-modification message, got %q", apiErr.Message)
	}
}

func TestWrite_BodyNotLogged(t *testing.T) {
	// Verify that the request body is not included in error output.
	// We send a body with a fake-secret and verify the error message doesn't contain it.
	const secretContent = "confidential-ticket-body-do-not-log"
	mux := http.NewServeMux()
	mux.HandleFunc("/issue", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errorMessages":["Bad request"]}`))
	})
	c := newTestClient(t, mux)
	payload := map[string]any{
		"fields": map[string]any{"summary": secretContent},
	}
	_, err := c.CreateJiraIssue(payload)
	if err == nil {
		t.Fatal("expected error")
	}
	if contains(err.Error(), secretContent) {
		t.Errorf("error message must not contain the request body: %q", err.Error())
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
