package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// checkRunsServer serves `runs` paginated at 100 per page, the way
// GET /commits/{sha}/check-runs does. totalOverride, when non-zero, reports a
// total_count that disagrees with what is served -- the truncation case.
func checkRunsServer(t *testing.T, runs []map[string]string, totalOverride int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		start := (page - 1) * 100
		end := start + 100
		if start > len(runs) {
			start = len(runs)
		}
		if end > len(runs) {
			end = len(runs)
		}
		total := len(runs)
		if totalOverride != 0 {
			total = totalOverride
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": total,
			"check_runs":  runs[start:end],
		})
	}))
}

func run(name, status, conclusion, startedAt string) map[string]string {
	return map[string]string{
		"name": name, "status": status, "conclusion": conclusion, "started_at": startedAt,
	}
}

// A red check beyond the first page must still block the merge.
//
// This is the regression test for a fail-OPEN gate: the previous version
// fetched ?per_page=100 once and judged whatever came back, so a failure on
// page 2 or 3 was invisible and the PR merged on a partial green.
func TestUnfinishedChecksSeesFailureBeyondFirstPage(t *testing.T) {
	var runs []map[string]string
	for i := 0; i < 250; i++ {
		runs = append(runs, run(fmt.Sprintf("green-%03d", i), "completed", "success", "2026-09-08T10:00:00Z"))
	}
	// index 240 lands on page 3; unreachable without pagination
	runs[240] = run("the-red-one", "completed", "failure", "2026-09-08T10:00:00Z")

	srv := checkRunsServer(t, runs, 0)
	defer srv.Close()
	githubAPI = srv.URL
	defer func() { githubAPI = "https://api.github.com" }()

	bad, err := unfinishedChecks("o", "r", "sha", "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "the-red-one") {
		t.Fatalf("failure on page 3 not reported, got %v", bad)
	}
}

// All-green across several pages must merge -- the gate has to be usable, not
// merely safe. A gate that refuses everything gets switched off.
func TestUnfinishedChecksAllGreenAcrossPages(t *testing.T) {
	var runs []map[string]string
	for i := 0; i < 250; i++ {
		runs = append(runs, run(fmt.Sprintf("green-%03d", i), "completed", "success", "2026-09-08T10:00:00Z"))
	}
	srv := checkRunsServer(t, runs, 0)
	defer srv.Close()
	githubAPI = srv.URL
	defer func() { githubAPI = "https://api.github.com" }()

	bad, err := unfinishedChecks("o", "r", "sha", "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("expected a clean merge, got %v", bad)
	}
}

// A list that cannot be accounted for is not a green light.
func TestUnfinishedChecksRefusesPartialList(t *testing.T) {
	var runs []map[string]string
	for i := 0; i < 40; i++ {
		runs = append(runs, run(fmt.Sprintf("green-%03d", i), "completed", "success", "2026-09-08T10:00:00Z"))
	}
	srv := checkRunsServer(t, runs, 500) // claims 500, serves 40
	defer srv.Close()
	githubAPI = srv.URL
	defer func() { githubAPI = "https://api.github.com" }()

	_, err := unfinishedChecks("o", "r", "sha", "tok")
	if err == nil {
		t.Fatal("a partial list was treated as a verdict; must refuse")
	}
	if !strings.Contains(err.Error(), "partial list") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Re-running a failed job creates a NEW suite, so the same name comes back
// twice. The newest entry wins -- across pages too, which is where the
// dedup and the pagination interact.
func TestUnfinishedChecksNewestWinsAcrossPages(t *testing.T) {
	var runs []map[string]string
	runs = append(runs, run("flaky", "completed", "failure", "2026-09-08T10:00:00Z"))
	for i := 0; i < 150; i++ {
		runs = append(runs, run(fmt.Sprintf("green-%03d", i), "completed", "success", "2026-09-08T10:00:00Z"))
	}
	// the re-run: same name, later start, on a later page
	runs = append(runs, run("flaky", "completed", "success", "2026-09-08T11:00:00Z"))

	srv := checkRunsServer(t, runs, 0)
	defer srv.Close()
	githubAPI = srv.URL
	defer func() { githubAPI = "https://api.github.com" }()

	bad, err := unfinishedChecks("o", "r", "sha", "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("a re-run that went green still blocks the merge: %v", bad)
	}
}

// A check still running is not green either.
func TestUnfinishedChecksCountsInProgress(t *testing.T) {
	runs := []map[string]string{
		run("done", "completed", "success", "2026-09-08T10:00:00Z"),
		run("still-going", "in_progress", "", "2026-09-08T10:00:00Z"),
	}
	srv := checkRunsServer(t, runs, 0)
	defer srv.Close()
	githubAPI = srv.URL
	defer func() { githubAPI = "https://api.github.com" }()

	bad, err := unfinishedChecks("o", "r", "sha", "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "still-going") {
		t.Fatalf("in_progress not treated as pending, got %v", bad)
	}
}

// fallbackServer answers check-runs with checkRunsCode (and `checkRuns` when
// that is 200) and serves `runs` from /actions/runs, paginated at 100 per page.
// actionsHits counts the fallback requests, so a test can prove it was NOT taken.
func fallbackServer(t *testing.T, checkRunsCode int, checkRuns []map[string]string,
	runs []map[string]interface{}, totalOverride int, actionsHits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if checkRunsCode != 200 {
				w.WriteHeader(checkRunsCode)
				w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": len(checkRuns), "check_runs": checkRuns,
			})
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			*actionsHits++
			if r.URL.Query().Get("head_sha") != "sha" {
				t.Errorf("fallback not filtered by head_sha: %s", r.URL.RawQuery)
			}
			start, end := (page-1)*100, page*100
			if start > len(runs) {
				start = len(runs)
			}
			if end > len(runs) {
				end = len(runs)
			}
			total := len(runs)
			if totalOverride != 0 {
				total = totalOverride
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": total, "workflow_runs": runs[start:end],
			})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

func wfRun(id int64, name, status, conclusion string, attempt int, started string) map[string]interface{} {
	return map[string]interface{}{
		"workflow_id": id, "name": name, "status": status, "conclusion": conclusion,
		"run_attempt": attempt, "created_at": started, "run_started_at": started,
	}
}

// A fine-grained PAT cannot hold the Checks permission, so check-runs answers
// 403; the gate must then judge the head by its Actions runs -- and only then.
func TestUnfinishedChecksFallback(t *testing.T) {
	green := wfRun(1, "pr-vm-deploy", "completed", "success", 1, "2026-10-20T10:00:00Z")
	var many []map[string]interface{}
	for i := 0; i < 40; i++ {
		many = append(many, wfRun(int64(100+i), fmt.Sprintf("wf-%03d", i), "completed", "success", 1, "2026-10-20T10:00:00Z"))
	}

	tests := []struct {
		name          string
		checkRunsCode int
		checkRuns     []map[string]string
		runs          []map[string]interface{}
		totalOverride int
		wantBad       []string
		wantErr       string // substring; "" means no error
		wantFallback  bool
	}{
		{
			name:          "403 on check-runs, all actions runs green",
			checkRunsCode: 403,
			runs: []map[string]interface{}{
				green,
				wfRun(2, "validate-cluster-components", "completed", "skipped", 1, "2026-10-20T10:00:00Z"),
				wfRun(3, "lint", "completed", "neutral", 1, "2026-10-20T10:00:00Z"),
			},
			wantFallback: true,
		},
		{
			name:          "newest run of a workflow failed",
			checkRunsCode: 403,
			runs: []map[string]interface{}{
				green,
				wfRun(2, "validate-cluster-components", "completed", "success", 1, "2026-10-20T10:00:00Z"),
				wfRun(2, "validate-cluster-components", "completed", "failure", 1, "2026-10-20T11:00:00Z"),
			},
			wantBad:      []string{"validate-cluster-components (failure)"},
			wantFallback: true,
		},
		{
			name:          "older failed, newer re-run green",
			checkRunsCode: 403,
			runs: []map[string]interface{}{
				// API order is newest first; the gate must not rely on it
				wfRun(2, "validate-cluster-components", "completed", "success", 1, "2026-10-20T11:00:00Z"),
				green,
				wfRun(2, "validate-cluster-components", "completed", "failure", 1, "2026-10-20T10:00:00Z"),
			},
			wantFallback: true,
		},
		{
			name:          "re-run attempt beats a run with the same start",
			checkRunsCode: 403,
			runs: []map[string]interface{}{
				wfRun(2, "flaky", "completed", "failure", 1, "2026-10-20T10:00:00Z"),
				wfRun(2, "flaky", "completed", "success", 2, "2026-10-20T10:00:00Z"),
			},
			wantFallback: true,
		},
		{
			name:          "in progress is reported",
			checkRunsCode: 403,
			runs: []map[string]interface{}{
				green,
				wfRun(2, "validate-cluster-components", "in_progress", "", 1, "2026-10-20T10:00:00Z"),
			},
			wantBad:      []string{"validate-cluster-components (in_progress)"},
			wantFallback: true,
		},
		{
			name:          "zero runs is not green",
			checkRunsCode: 403,
			runs:          nil,
			wantErr:       "none found",
			wantFallback:  true,
		},
		{
			name:          "partial list is refused",
			checkRunsCode: 403,
			runs:          many,
			totalOverride: 500,
			wantErr:       "partial list",
			wantFallback:  true,
		},
		{
			name:          "500 on check-runs is an error, no fallback",
			checkRunsCode: 500,
			runs:          []map[string]interface{}{green},
			wantErr:       "HTTP 500",
		},
		{
			name:          "200 on check-runs keeps the old path",
			checkRunsCode: 200,
			checkRuns: []map[string]string{
				run("deploy", "completed", "success", "2026-10-20T10:00:00Z"),
				run("validate", "completed", "failure", "2026-10-20T10:00:00Z"),
			},
			runs:    []map[string]interface{}{green},
			wantBad: []string{"validate (failure)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := fallbackServer(t, tc.checkRunsCode, tc.checkRuns, tc.runs, tc.totalOverride, &hits)
			defer srv.Close()
			githubAPI = srv.URL
			defer func() { githubAPI = "https://api.github.com" }()

			bad, err := unfinishedChecks("o", "r", "sha", "tok")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got bad=%v err=%v", tc.wantErr, bad, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fmt.Sprint(bad) != fmt.Sprint(tc.wantBad) {
				t.Fatalf("notGreen: want %v, got %v", tc.wantBad, bad)
			}
			if (hits > 0) != tc.wantFallback {
				t.Fatalf("fallback taken=%v, want %v", hits > 0, tc.wantFallback)
			}
		})
	}
}
