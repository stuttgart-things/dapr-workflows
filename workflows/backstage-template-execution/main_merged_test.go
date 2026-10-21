package main

import (
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
