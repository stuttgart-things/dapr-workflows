package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// prServer answers the two lookups the merge gate makes when no PR is open:
// the open-PR list (always empty here) and either the PR by number or the
// newest closed PR of the branch.
func prServer(t *testing.T, byNumber, closedList string) (*httptest.Server, *[]string) {
	t.Helper()
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case r.Method == http.MethodPut:
			t.Errorf("merge attempted on an already merged PR: %s", r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.URL.Query().Get("state") == "open":
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.URL.Query().Get("state") == "closed":
			_, _ = w.Write([]byte(closedList))
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(byNumber))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	githubAPI = srv.URL
	t.Cleanup(func() { srv.Close(); githubAPI = "https://api.github.com" })
	return srv, &hits
}

func TestMergePRAlreadyMergedByNumber(t *testing.T) {
	prServer(t, `{"number":3911,"html_url":"https://github.com/o/r/pull/3911","merged_at":"2026-10-21T07:37:36Z","merge_commit_sha":"abc123","merged_by":{"login":"someone"}}`, `[]`)
	m, err := mergePR(MergeInput{Owner: "o", Repo: "r", Branch: "b", Method: "squash", PRNumber: 3911}, "t")
	if err != nil {
		t.Fatalf("already merged must not be a refusal: %v", err)
	}
	if !m.Merged || !m.ByHand || m.PRNumber != 3911 || m.MergedBy != "someone" || m.SHA != "abc123" {
		t.Fatalf("unexpected result: %+v", m)
	}
}

func TestMergePRAlreadyMergedByBranch(t *testing.T) {
	_, hits := prServer(t, `{}`, `[{"number":7,"html_url":"u","merged_at":"2026-10-21T07:37:36Z","merge_commit_sha":"def","merged_by":null}]`)
	m, err := mergePR(MergeInput{Owner: "o", Repo: "r", Branch: "b", Method: "squash"}, "t")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.ByHand || m.PRNumber != 7 || m.MergedBy != "someone" {
		t.Fatalf("unexpected result: %+v", m)
	}
	for _, h := range *hits {
		if strings.Contains(h, "/pulls/") {
			t.Fatalf("without a PR number the gate must not ask for one by number: %v", *hits)
		}
	}
}

func TestMergePRClosedUnmergedIsStillARefusal(t *testing.T) {
	prServer(t, `{"number":5,"html_url":"u","merged_at":null}`, `[]`)
	_, err := mergePR(MergeInput{Owner: "o", Repo: "r", Branch: "b", Method: "squash", PRNumber: 5}, "t")
	if err == nil || !strings.Contains(err.Error(), "none merged") {
		t.Fatalf("closed without merge must stay an error, got %v", err)
	}
}

func TestMergePRNoPRAtAll(t *testing.T) {
	prServer(t, `{}`, `[]`)
	_, err := mergePR(MergeInput{Owner: "o", Repo: "r", Branch: "b", Method: "squash"}, "t")
	if err == nil || !strings.Contains(err.Error(), "no open PR") {
		t.Fatalf("want no-open-PR error, got %v", err)
	}
}

// openPRServer answers the gate for one open PR whose head carries `runs`.
// A merge (PUT) fails the test: none of these heads may be merged.
func openPRServer(t *testing.T, runs []map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			t.Errorf("merged a head that is not green: %s", r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[{"number":4138,"html_url":"https://github.com/o/r/pull/4138","head":{"sha":"578ad1b"}}]`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"total_count": len(runs), "check_runs": runs})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	githubAPI = srv.URL
	t.Cleanup(func() { srv.Close(); githubAPI = "https://api.github.com" })
}

// stuttgart-things#4138: the watched workflow was green, validate-templates
// still queued. That is a "not yet", not a refusal.
func TestMergePROnlyUnfinishedIsPending(t *testing.T) {
	openPRServer(t, []map[string]string{
		run("Verify-Claim-Deletion", "completed", "success", "2026-10-26T14:29:33Z"),
		run("validate-templates", "queued", "", ""),
	})
	m, err := mergePR(MergeInput{Owner: "o", Repo: "r", Branch: "b", Method: "squash", PRNumber: 4138}, "t")
	if err != nil {
		t.Fatalf("unfinished checks only must not be a refusal: %v", err)
	}
	if m.Merged || m.PRNumber != 4138 || fmt.Sprint(m.Pending) != "[validate-templates (queued)]" {
		t.Fatalf("unexpected result: %+v", m)
	}
}

// Red is a verdict even while other checks still run: refuse now, do not wait.
func TestMergePRRedWhileOthersRunIsARefusal(t *testing.T) {
	openPRServer(t, []map[string]string{
		run("validate-cluster-components", "completed", "failure", "2026-10-26T14:29:33Z"),
		run("validate-templates", "in_progress", "", "2026-10-26T14:34:10Z"),
	})
	_, err := mergePR(MergeInput{Owner: "o", Repo: "r", Branch: "b", Method: "squash"}, "t")
	if err == nil || !strings.Contains(err.Error(), "validate-cluster-components (failure)") {
		t.Fatalf("want a refusal naming the red check, got %v", err)
	}
}

func TestOnlyUnfinished(t *testing.T) {
	tests := []struct {
		bad  []string
		want bool
	}{
		{nil, false},
		{[]string{"a (queued)", "b (in_progress)", "c (waiting)"}, true},
		{[]string{"a (queued)", "b (failure)"}, false},
		{[]string{"a (cancelled)"}, false},
		{[]string{"a (timed_out)"}, false},
	}
	for _, tc := range tests {
		if got := onlyUnfinished(tc.bad); got != tc.want {
			t.Errorf("onlyUnfinished(%v) = %v, want %v", tc.bad, got, tc.want)
		}
	}
}
