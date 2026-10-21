package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func vmInput(tmpl string, vals map[string]interface{}) Input {
	return Input{TemplateRef: "template:default/" + tmpl, Values: vals,
		Watch: &GitHubWatch{Owner: "stuttgart-things", Repo: "stuttgart-things"}}
}

func TestRunSubject(t *testing.T) {
	cases := []struct {
		in   Input
		want string
	}{
		{vmInput("create-terraform-vm", map[string]interface{}{"vm_name": "cutover2"}), "VM cutover2"},
		{vmInput("delete-terraform-vm", map[string]interface{}{"vmname": "cutover2"}), "VM cutover2 deletion"},
		{vmInput("flux-bootstrap", map[string]interface{}{}), "flux-bootstrap"},
		{vmInput("create-terraform-vm", map[string]interface{}{"vm_name": "  "}), "create-terraform-vm"},
	}
	for _, c := range cases {
		if got := runSubject(c.in); got != c.want {
			t.Errorf("runSubject(%s) = %q, want %q", c.in.TemplateRef, got, c.want)
		}
	}
}

func TestBuildPitchMerged(t *testing.T) {
	in := vmInput("create-terraform-vm", map[string]interface{}{
		"vm_name": "cutover2", "lab": "LabDA", "cloud": "vsphere", "vm_sizing": "small"})
	at := time.Date(2026, 10, 21, 4, 5, 8, 0, time.UTC)
	m := buildPitch(in, checkpoint{Event: "merged", Severity: "success", Detail: "PR #3897 merged",
		PRNumber: 3897, PRURL: prURL(in.Watch, 3897), RunURL: "https://github.com/x/runs/1"}, at)

	if m.Title != "VM cutover2: merged (#3897)" {
		t.Errorf("title = %q", m.Title)
	}
	if m.URL != "https://github.com/stuttgart-things/stuttgart-things/pull/3897" {
		t.Errorf("url = %q, want the PR", m.URL)
	}
	if m.System != homerunSystem || m.Author != homerunSystem || m.Severity != "success" {
		t.Errorf("system/author/severity = %q/%q/%q", m.System, m.Author, m.Severity)
	}
	if m.Timestamp != "2026-10-21T04:05:08Z" {
		t.Errorf("timestamp = %q", m.Timestamp)
	}
	for _, want := range []string{"PR #3897 merged", "Template: template:default/create-terraform-vm",
		"lab=LabDA", "cloud=vsphere", "vm_sizing=small", "PR: https://github.com/stuttgart-things/stuttgart-things/pull/3897",
		"Run: https://github.com/x/runs/1"} {
		if !strings.Contains(m.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, m.Message)
		}
	}
	if m.Tags != "backstage-template-run,merged,create-terraform-vm" {
		t.Errorf("tags = %q", m.Tags)
	}
}

func TestBuildPitchLinkFallsBack(t *testing.T) {
	in := vmInput("delete-terraform-vm", map[string]interface{}{"vmname": "x"})
	m := buildPitch(in, checkpoint{Event: "started", Severity: "info", TaskURL: "https://backstage/task/1"}, time.Now())
	if m.URL != "https://backstage/task/1" || m.Title != "VM x deletion: started" {
		t.Errorf("url/title = %q/%q", m.URL, m.Title)
	}
	m = buildPitch(in, checkpoint{Event: "build-started", RunURL: "https://run", TaskURL: "https://task"}, time.Now())
	if m.URL != "https://run" {
		t.Errorf("url = %q, want the run before the task", m.URL)
	}
}

func TestDeliverPitchOff(t *testing.T) {
	t.Setenv("HOMERUN_PITCH_URL", "")
	got, err := deliverPitch(homerunMessage{Title: "t", Message: "m"})
	if err != nil || got != "off" {
		t.Fatalf("deliverPitch = %q, %v; want off, nil", got, err)
	}
}

func TestDeliverPitchSendsBearerAndBody(t *testing.T) {
	var gotAuth string
	var gotMsg homerunMessage
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotMsg)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	old := homerunClient
	homerunClient = srv.Client()
	defer func() { homerunClient = old }()

	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMERUN_PITCH_URL", srv.URL+"/pitch")
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", tok)
	t.Setenv("HOMERUN_AUTH_TOKEN", "env-token") // the file wins

	got, err := deliverPitch(homerunMessage{Title: "VM a: merged", Message: "m", System: homerunSystem})
	if err != nil || got != "sent" {
		t.Fatalf("deliverPitch = %q, %v", got, err)
	}
	if gotAuth != "Bearer file-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotMsg.Title != "VM a: merged" || gotMsg.System != homerunSystem {
		t.Errorf("body = %+v", gotMsg)
	}
}

func TestDeliverPitchRefusesTokenOverHTTP(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	t.Setenv("HOMERUN_PITCH_URL", srv.URL+"/pitch")
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", "secret-token")
	if _, err := deliverPitch(homerunMessage{Title: "t", Message: "m"}); err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("err = %v, want https refusal", err)
	}
	if called {
		t.Error("the token went over plain http")
	}
}

func TestDeliverPitchPlainHTTPWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("sent an Authorization header without a token")
		}
	}))
	defer srv.Close()
	t.Setenv("HOMERUN_PITCH_URL", srv.URL+"/pitch")
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", "")
	if got, err := deliverPitch(homerunMessage{Title: "t", Message: "m"}); err != nil || got != "sent" {
		t.Fatalf("deliverPitch = %q, %v", got, err)
	}
}

// A failing pitcher is an activity error (which only drives the bounded
// retry; the workflow's pitch() has no error return), and the token never
// appears in it even when the server echoes the header back.
func TestDeliverPitchFailureIsScrubbed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream said: " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	old := homerunClient
	homerunClient = srv.Client()
	defer func() { homerunClient = old }()
	t.Setenv("HOMERUN_PITCH_URL", srv.URL+"/pitch")
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", "s3cr3t-token-value")

	_, err := deliverPitch(homerunMessage{Title: "t", Message: "m"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %v, want HTTP 502", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-token-value") {
		t.Errorf("token leaked into the error: %v", err)
	}
}

func TestDeliverPitchUnreachableHidesURL(t *testing.T) {
	t.Setenv("HOMERUN_PITCH_URL", "https://127.0.0.1:1/pitch?sig=abc")
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", "")
	_, err := deliverPitch(homerunMessage{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "sig=abc") {
		t.Errorf("URL leaked into the error: %v", err)
	}
}

func TestWorkflowRunCarriesPRNumber(t *testing.T) {
	var parsed struct {
		WorkflowRuns []ghWorkflowRun `json:"workflow_runs"`
	}
	raw := `{"workflow_runs":[{"id":7,"status":"completed","conclusion":"success","pull_requests":[{"number":3897}]}]}`
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	if n := parsed.WorkflowRuns[0].PullRequests; len(n) != 1 || n[0].Number != 3897 {
		t.Fatalf("pull_requests = %+v", n)
	}
}
