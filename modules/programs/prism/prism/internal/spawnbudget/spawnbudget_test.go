package spawnbudget_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

func TestLevelFor(t *testing.T) {
	cases := []struct {
		count, limit int
		want         spawnbudget.Level
	}{
		{0, 120, spawnbudget.LevelUnder},
		{113, 120, spawnbudget.LevelUnder},
		{114, 120, spawnbudget.LevelNear}, // 95% of 120 is 114
		{119, 120, spawnbudget.LevelNear},
		{120, 120, spawnbudget.LevelOver},
		{196, 120, spawnbudget.LevelOver},
		{18, 19, spawnbudget.LevelUnder}, // 94.7%: integer math must not round up
		{5, 0, spawnbudget.LevelUnder},
	}
	for _, tc := range cases {
		if got := spawnbudget.LevelFor(tc.count, tc.limit); got != tc.want {
			t.Errorf("LevelFor(%d, %d) = %v, want %v", tc.count, tc.limit, got, tc.want)
		}
	}
}

func TestLine_OneLinePerLevel(t *testing.T) {
	cases := []struct {
		name     string
		r        spawnbudget.Result
		want     []string
		mustNot  []string
		wantText string
	}{
		{
			name:     "under",
			r:        spawnbudget.Result{Count: 87, Limit: 120, Known: true, Fleet: true},
			want:     []string{"87 of 120 worker spawns in the last 7 days", "72%", "all hosts"},
			mustNot:  []string{"WARNING", "OVER", "stop for the day"},
			wantText: "spawn budget: 87 of 120 worker spawns in the last 7 days (72%, all hosts).",
		},
		{
			name:    "near",
			r:       spawnbudget.Result{Count: 115, Limit: 120, Known: true, Fleet: true},
			want:    []string{"115 of 120", "WARNING", "95%", "Finish the current work and stop for the day."},
			mustNot: []string{"OVER"},
		},
		{
			name: "over",
			r:    spawnbudget.Result{Count: 196, Limit: 120, Known: true, Fleet: true},
			want: []string{"196 of 120", "163%", "OVER THE LIMIT", "Prism starts this worker", "stop for the day"},
		},
		{
			name: "local only",
			r:    spawnbudget.Result{Count: 40, Limit: 120, Known: true, LocalOnlyReason: "Prometheus unreachable"},
			want: []string{"40 of 120", "this host only: Prometheus unreachable"},
		},
		{
			name: "unknown",
			r:    spawnbudget.Result{Limit: 120, LocalOnlyReason: "Prometheus unreachable", LocalErr: errors.New("boom")},
			want: []string{"count not available", "Prometheus unreachable", "local count failed", "120"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.r.Line()
			if !strings.HasPrefix(line, spawnbudget.LinePrefix) {
				t.Errorf("line %q does not start with %q", line, spawnbudget.LinePrefix)
			}
			if strings.Contains(line, "\n") {
				t.Errorf("line %q is more than one line", line)
			}
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("line %q does not contain %q", line, w)
				}
			}
			for _, w := range tc.mustNot {
				if strings.Contains(line, w) {
					t.Errorf("line %q contains %q", line, w)
				}
			}
			if tc.wantText != "" && line != tc.wantText {
				t.Errorf("line = %q, want %q", line, tc.wantText)
			}
		})
	}
}

// promServer serves /api/v1/query with body and records the query it got.
func promServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotQuery
}

func vectorBody(value string) string {
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1792021852.627,%q]}]}}`, value)
}

const emptyVectorBody = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func TestQueryFleet_SendsWorkerQueryAndRoundsToNearest(t *testing.T) {
	cases := map[string]int{
		"196.0409646064243": 196,
		"119.5":             120,
		"119.49":            119,
		"0":                 0,
	}
	for raw, want := range cases {
		srv, gotQuery := promServer(t, http.StatusOK, vectorBody(raw))
		// A trailing slash on the base URL must not produce "//api".
		got, err := spawnbudget.QueryFleet(context.Background(), nil, srv.URL+"/")
		if err != nil {
			t.Fatalf("%s: QueryFleet: %v", raw, err)
		}
		if got != want {
			t.Errorf("QueryFleet(%s) = %d, want %d", raw, got, want)
		}
		if *gotQuery != spawnbudget.FleetQuery {
			t.Errorf("query = %q, want %q", *gotQuery, spawnbudget.FleetQuery)
		}
	}
	if !strings.Contains(spawnbudget.FleetQuery, `agent_role="worker"`) || !strings.Contains(spawnbudget.FleetQuery, "[7d]") {
		t.Errorf("FleetQuery %q must select agent_role=\"worker\" over [7d]", spawnbudget.FleetQuery)
	}
	if strings.Contains(spawnbudget.FleetQuery, "repo") {
		t.Errorf("FleetQuery %q must not filter by repo: every repo counts", spawnbudget.FleetQuery)
	}
}

func TestQueryFleet_NoData(t *testing.T) {
	for _, body := range []string{emptyVectorBody, vectorBody("NaN")} {
		srv, _ := promServer(t, http.StatusOK, body)
		_, err := spawnbudget.QueryFleet(context.Background(), nil, srv.URL)
		if !errors.Is(err, spawnbudget.ErrNoData) {
			t.Errorf("body %s: err = %v, want ErrNoData", body, err)
		}
	}
}

func TestQueryFleet_Failures(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"http 500":     {http.StatusInternalServerError, `oops`},
		"bad json":     {http.StatusOK, `{`},
		"status error": {http.StatusOK, `{"status":"error","error":"parse error"}`},
		"matrix":       {http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
	}
	for name, tc := range cases {
		srv, _ := promServer(t, tc.status, tc.body)
		_, err := spawnbudget.QueryFleet(context.Background(), nil, srv.URL)
		if err == nil || errors.Is(err, spawnbudget.ErrNoData) {
			t.Errorf("%s: err = %v, want a non-ErrNoData error", name, err)
		}
	}
}

func localCount(n int) spawnbudget.LocalCountFunc {
	return func(context.Context, time.Time) (int, error) { return n, nil }
}

func TestLookup_CountIsTheHigherOfFleetAndLocal(t *testing.T) {
	cases := []struct {
		fleet, local, want int
	}{
		{50, 60, 60},
		{70, 60, 70},
		{0, 0, 0},
	}
	for _, tc := range cases {
		srv, _ := promServer(t, http.StatusOK, vectorBody(fmt.Sprint(tc.fleet)))
		r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
			PrometheusURL: srv.URL,
			Limit:         120,
			Local:         localCount(tc.local),
		})
		if !r.Known || !r.Fleet {
			t.Fatalf("fleet %d local %d: Known=%v Fleet=%v, want both true", tc.fleet, tc.local, r.Known, r.Fleet)
		}
		if r.Count != tc.want {
			t.Errorf("fleet %d local %d: Count = %d, want %d", tc.fleet, tc.local, r.Count, tc.want)
		}
		if r.FleetCount != tc.fleet || r.LocalCount != tc.local {
			t.Errorf("FleetCount/LocalCount = %d/%d, want %d/%d", r.FleetCount, r.LocalCount, tc.fleet, tc.local)
		}
		if !strings.Contains(r.Line(), "all hosts") {
			t.Errorf("line %q does not say all hosts", r.Line())
		}
	}
}

// closedURL returns an http URL on which nothing listens.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func TestLookup_PrometheusUnreachable_UsesLocalCountForThisHostOnly(t *testing.T) {
	r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		PrometheusURL: closedURL(t),
		Limit:         120,
		Local:         localCount(33),
	})
	if r.Fleet || !r.Known || r.Count != 33 {
		t.Fatalf("Fleet=%v Known=%v Count=%d, want false/true/33", r.Fleet, r.Known, r.Count)
	}
	if !strings.Contains(r.Line(), "this host only: Prometheus unreachable") {
		t.Errorf("line %q does not say this host only: Prometheus unreachable", r.Line())
	}
}

func TestLookup_PrometheusNoData_UsesLocalCountForThisHostOnly(t *testing.T) {
	srv, _ := promServer(t, http.StatusOK, emptyVectorBody)
	r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		PrometheusURL: srv.URL,
		Limit:         120,
		Local:         localCount(12),
	})
	if r.Fleet || r.Count != 12 {
		t.Fatalf("Fleet=%v Count=%d, want false/12", r.Fleet, r.Count)
	}
	if !strings.Contains(r.Line(), "this host only: Prometheus returned no data") {
		t.Errorf("line %q does not say this host only: Prometheus returned no data", r.Line())
	}
}

func TestLookup_NoPrometheusURL_UsesLocalCountForThisHostOnly(t *testing.T) {
	r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		Limit: 120,
		Local: localCount(7),
	})
	if r.Fleet || r.Count != 7 {
		t.Fatalf("Fleet=%v Count=%d, want false/7", r.Fleet, r.Count)
	}
	if !strings.Contains(r.Line(), "this host only: no Prometheus URL configured") {
		t.Errorf("line %q", r.Line())
	}
}

func TestLookupTimeout_IsTwoSeconds(t *testing.T) {
	if spawnbudget.LookupTimeout != 2*time.Second {
		t.Errorf("LookupTimeout = %v, want 2s", spawnbudget.LookupTimeout)
	}
	if spawnbudget.Window != 168*time.Hour {
		t.Errorf("Window = %v, want 168h", spawnbudget.Window)
	}
}

// A Prometheus that does not answer must cost no more than the timeout, and
// the lookup must fall back to the local count.
func TestLookup_SlowPrometheus_ReturnsAtTheTimeoutWithLocalCount(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	const timeout = 300 * time.Millisecond
	start := time.Now()
	r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		PrometheusURL: srv.URL,
		Limit:         120,
		Local:         localCount(9),
		Timeout:       timeout,
	})
	elapsed := time.Since(start)
	if elapsed > timeout+500*time.Millisecond {
		t.Errorf("Lookup took %v, want about %v", elapsed, timeout)
	}
	if r.Fleet || r.Count != 9 {
		t.Fatalf("Fleet=%v Count=%d, want false/9", r.Fleet, r.Count)
	}
	if !strings.Contains(r.Line(), "this host only: Prometheus did not answer in 300ms") {
		t.Errorf("line %q", r.Line())
	}
}

// A local count that ignores its context must not hold the lookup past the
// timeout either.
func TestLookup_StuckLocalCount_ReturnsAtTheTimeoutWithFleetCount(t *testing.T) {
	srv, _ := promServer(t, http.StatusOK, vectorBody("88"))
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	const timeout = 300 * time.Millisecond
	start := time.Now()
	r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		PrometheusURL: srv.URL,
		Limit:         120,
		Local: func(context.Context, time.Time) (int, error) {
			<-stuck
			return 1000, nil
		},
		Timeout: timeout,
	})
	if elapsed := time.Since(start); elapsed > timeout+500*time.Millisecond {
		t.Errorf("Lookup took %v, want about %v", elapsed, timeout)
	}
	if !r.Fleet || r.Count != 88 || r.LocalErr == nil {
		t.Fatalf("Fleet=%v Count=%d LocalErr=%v, want true/88/non-nil", r.Fleet, r.Count, r.LocalErr)
	}
}

func TestLookup_BothSourcesFail_CountNotAvailable(t *testing.T) {
	r := spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		PrometheusURL: closedURL(t),
		Limit:         120,
		Local: func(context.Context, time.Time) (int, error) {
			return 0, errors.New("database is locked")
		},
	})
	if r.Known {
		t.Fatalf("Known = true, want false")
	}
	if r.Level() != spawnbudget.LevelUnder {
		t.Errorf("Level = %v, want LevelUnder for an unknown count", r.Level())
	}
	if !strings.Contains(r.Line(), "count not available") {
		t.Errorf("line %q", r.Line())
	}
}

// The local count gets a cutoff of exactly Window before now.
func TestLookup_LocalCutoffIs168Hours(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var gotSince time.Time
	spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		Limit: 120,
		Now:   func() time.Time { return now },
		Local: func(_ context.Context, since time.Time) (int, error) {
			gotSince = since
			return 0, nil
		},
	})
	if want := now.Add(-168 * time.Hour); !gotSince.Equal(want) {
		t.Errorf("since = %v, want %v", gotSince, want)
	}
}
