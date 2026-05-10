// Package client provides an authenticated HTTP client for Atlassian Cloud APIs.
package client

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// Client is an authenticated Atlassian API client.
type Client struct {
	Site     string
	Email    string
	token    string
	hc       *http.Client
	baseURL  string // override for testing
}

// New creates a Client from the required environment variables.
// Returns an error naming the first missing variable.
func New() (*Client, error) {
	site := os.Getenv("ATLASSIAN_SITE")
	if site == "" {
		return nil, fmt.Errorf("ATLASSIAN_SITE is not set")
	}
	email := os.Getenv("ATLASSIAN_EMAIL")
	if email == "" {
		return nil, fmt.Errorf("ATLASSIAN_EMAIL is not set")
	}
	token := os.Getenv("ATLASSIAN_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("ATLASSIAN_API_TOKEN is not set")
	}
	return &Client{
		Site:  site,
		Email: email,
		token: token,
		hc:    &http.Client{},
	}, nil
}

// NewWithBase creates a Client with a custom base URL (for testing).
func NewWithBase(site, email, token, baseURL string) *Client {
	return &Client{
		Site:    site,
		Email:   email,
		token:   token,
		hc:      &http.Client{},
		baseURL: baseURL,
	}
}

func (c *Client) authHeader() string {
	creds := c.Email + ":" + c.token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

func (c *Client) jiraBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return "https://" + c.Site + "/rest/api/3"
}

func (c *Client) confluenceBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return "https://" + c.Site + "/wiki/api/v2"
}

func (c *Client) cloudBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return "https://api.atlassian.com"
}

// APIError represents an HTTP error from the Atlassian API.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d %s %s: %s", e.StatusCode, e.Method, e.Path, e.Message)
}

// Get performs an authenticated GET request and decodes the JSON response body.
// On 4xx/5xx it returns an *APIError.
func (c *Client) Get(url string) (any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		msg := extractErrorMessage(body, resp.StatusCode)
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodGet,
			Path:       resp.Request.URL.Path,
			Message:    msg,
		}
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return v, nil
}

// extractErrorMessage tries to extract a human-readable error message from an
// Atlassian error response body.
func extractErrorMessage(body []byte, statusCode int) string {
	// Try to parse as JSON
	var v map[string]any
	if err := json.Unmarshal(body, &v); err == nil {
		// Jira-style: {"errorMessages": [...], "errors": {...}}
		if msgs, ok := v["errorMessages"].([]any); ok && len(msgs) > 0 {
			if s, ok := msgs[0].(string); ok && s != "" {
				return s
			}
		}
		// Confluence / generic: {"message": "..."}
		if msg, ok := v["message"].(string); ok && msg != "" {
			return msg
		}
		// Jira search error: {"warningMessages": [...]}
		if msgs, ok := v["warningMessages"].([]any); ok && len(msgs) > 0 {
			if s, ok := msgs[0].(string); ok {
				return s
			}
		}
	}
	if len(body) > 0 && len(body) < 200 {
		return string(body)
	}
	return http.StatusText(statusCode)
}

// GetJiraIssue fetches a Jira issue by key.
func (c *Client) GetJiraIssue(key string) (any, error) {
	url := c.jiraBase() + "/issue/" + key
	return c.Get(url)
}

// SearchJira performs a Jira JQL search.
func (c *Client) SearchJira(jql string, maxResults int, fields string) (any, error) {
	url := fmt.Sprintf("%s/search/jql?jql=%s&maxResults=%d", c.jiraBase(), urlEncode(jql), maxResults)
	if fields != "" {
		url += "&fields=" + urlEncode(fields)
	}
	return c.Get(url)
}

// GetJiraTransitions fetches the available transitions for a Jira issue.
func (c *Client) GetJiraTransitions(key string) (any, error) {
	url := c.jiraBase() + "/issue/" + key + "/transitions"
	return c.Get(url)
}

// GetConfluencePage fetches a Confluence page by ID.
func (c *Client) GetConfluencePage(id string) (any, error) {
	url := c.confluenceBase() + "/pages/" + id + "?body-format=atlas_doc_format"
	return c.Get(url)
}

// SearchConfluence performs a Confluence CQL search.
func (c *Client) SearchConfluence(cql string, limit int) (any, error) {
	url := fmt.Sprintf("%s/search?cql=%s&limit=%d", c.confluenceBase(), urlEncode(cql), limit)
	return c.Get(url)
}

// GetMe fetches the current authenticated user.
func (c *Client) GetMe() (any, error) {
	url := c.cloudBase() + "/me"
	return c.Get(url)
}

// GetResources fetches accessible Atlassian cloud resources.
func (c *Client) GetResources() (any, error) {
	url := c.cloudBase() + "/oauth/token/accessible-resources"
	return c.Get(url)
}

// urlEncode percent-encodes a query parameter value.
func urlEncode(s string) string {
	return url.QueryEscape(s)
}
