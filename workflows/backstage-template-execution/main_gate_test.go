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
