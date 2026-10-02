package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dapr/durabletask-go/workflow"
)

func testReport() ReportInput {
	in := testInput(false)
	st := newState(in, t0)
	Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0.Add(time.Minute))
	evs := Advance(in, st, nil, &XRObservation{Found: true, Ready: bptr(true), ReadyCondition: true}, t0.Add(20*time.Minute))
	return ReportInput{InstanceID: "ns_w1", Target: in.Target, Event: evs[0], State: *st}
}

func TestCloudEvent(t *testing.T) {
	ce := cloudEvent(testReport())
	if ce["type"] != "io.sthings.clusterbuild.ready" || ce["id"] != "ns_w1-2" || ce["specversion"] != "1.0" {
		t.Fatalf("got %v", ce)
	}
	b, _ := json.Marshal(ce)
	if !strings.Contains(string(b), `"duration":"19m0s"`) {
		t.Fatalf("final event carries stage durations: %s", b)
	}
}

func TestTeamsMessageIsAdaptiveCard(t *testing.T) {
	b, _ := json.Marshal(teamsMessage(testReport()))
	s := string(b)
	for _, want := range []string{`"application/vnd.microsoft.card.adaptive"`, `"AdaptiveCard"`, `u26-kind1: ready`, `"Good"`, `**vm** 19m0s`} {
		if !strings.Contains(s, want) {
			t.Errorf("teams payload lacks %s: %s", want, s)
		}
	}
}

func TestStatusConfigMapName(t *testing.T) {
	for id, want := range map[string]string{
		"backstage-workflows_u26-kind1": "cluster-build-watch.backstage-workflows.u26-kind1",
		"watch-1759300000":              "cluster-build-watch.watch-1759300000",
		"Odd ID/with spaces_":           "cluster-build-watch.odd-id-with-spaces",
	} {
		if got := statusConfigMapName(id); got != want {
			t.Errorf("statusConfigMapName(%q) = %q, want %q", id, got, want)
		}
	}
	if n := statusConfigMapName(strings.Repeat("a", 400)); len(n) > 253 {
		t.Fatalf("name longer than 253: %d", len(n))
	}
}

// Report sends to every sink, and one failing sink does not stop the others.
func TestReportSinksAreIndependent(t *testing.T) {
	var gotCM map[string]any
	var gotCE map[string]any
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotCM)
		io.WriteString(w, `{}`)
	}))
	defer kube.Close()
	teams := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer teams.Close()
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/cloudevents+json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		json.NewDecoder(r.Body).Decode(&gotCE)
	}))
	defer hook.Close()

	t.Setenv("KUBE_API_SERVER", kube.URL)
	t.Setenv("STATUS_NAMESPACE", "watch-ns")
	t.Setenv("TEAMS_WEBHOOK_URL", teams.URL)
	t.Setenv("STATUS_WEBHOOK_URL", hook.URL)

	sent, err := Report(fakeActivity{in: testReport()})
	if err == nil || !strings.Contains(err.Error(), "teams: HTTP 502") {
		t.Fatalf("want the teams failure reported, got %v", err)
	}
	if got := strings.Join(sent.([]string), ","); got != "configmap,webhook" {
		t.Fatalf("the other sinks must still deliver, sent %q", got)
	}
	data, _ := gotCM["data"].(map[string]any)
	if data["phase"] != phaseReady || data["stage"] != "vm" {
		t.Fatalf("configmap data: %v", data)
	}
	if gotCE["type"] != "io.sthings.clusterbuild.ready" {
		t.Fatalf("cloudevent: %v", gotCE)
	}
}

// A transport error must not leak the webhook URL, which is its credential.
func TestPostJSONRedactsURL(t *testing.T) {
	secret := "http://127.0.0.1:1/workflows/abc/triggers/manual?sig=SECRET" // pragma: allowlist secret -- a fake signature
	err := postJSON(secret, "application/json", map[string]any{})
	if err == nil {
		t.Fatal("want an error from a closed port")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("webhook URL leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "<webhook>") {
		t.Fatalf("got %v", err)
	}
}

// fakeActivity is an ActivityContext that only answers GetInput, which is all
// the activities here use. Any other method panics on the nil embed.
type fakeActivity struct {
	workflow.ActivityContext
	in any
}

func (f fakeActivity) Context() context.Context { return context.Background() }

func (f fakeActivity) GetInput(v any) error {
	b, err := json.Marshal(f.in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// homerunServer is an omni-pitcher stand-in over TLS. It records the
// Authorization header and the decoded body, and answers like /pitch does.
func homerunServer(t *testing.T, status int, reply string) (*httptest.Server, *http.Header, *map[string]any) {
	t.Helper()
	var hdr http.Header
	var body map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/pitch" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		hdr = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	old := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = old })
	return srv, &hdr, &body
}

func TestHomerunPitchMapping(t *testing.T) {
	in := testReport()
	m := homerunPitch(in)
	b, _ := json.Marshal(m)
	var got map[string]any
	json.Unmarshal(b, &got)

	want := map[string]any{
		"title":     "u26-kind1: ready",
		"severity":  sevSuccess,
		"author":    "cluster-build-watch",
		"system":    "cluster-build-watch",
		"timestamp": in.Event.At.UTC().Format(time.RFC3339),
		"tags":      "cluster-build,ready," + in.Event.Stage + "," + in.Target.Namespace + "/" + in.Target.Name,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["url"]; ok {
		t.Errorf("url must be left out when the input has none: %s", b)
	}
	msg, _ := got["message"].(string)
	if !strings.HasPrefix(msg, in.Event.Message) || !strings.HasSuffix(msg, "\nStages: xr-pending 1m0s, vm 19m0s") {
		t.Errorf("final event message carries the stage summary: %q", msg)
	}
	// Only the field names homerun.Message (homerun-library message.go) has.
	for k := range got {
		switch k {
		case "title", "message", "severity", "author", "timestamp", "system", "tags", "url":
		default:
			t.Errorf("field %q is not part of homerun.Message", k)
		}
	}
}

func TestHomerunSeverityPassthrough(t *testing.T) {
	for _, sev := range []string{sevInfo, sevSuccess, sevWarning, sevError} {
		in := testReport()
		in.Event.Severity = sev
		in.Event.Stages = nil
		if got := homerunPitch(in); got.Severity != sev {
			t.Errorf("severity %q became %q", sev, got.Severity)
		}
		if strings.Contains(homerunPitch(in).Message, "Stages:") {
			t.Errorf("no stage summary without stages")
		}
	}
}

func TestHomerunSinkSendsBearer(t *testing.T) {
	srv, hdr, body := homerunServer(t, http.StatusOK, `{"status":"success"}`)
	tokenFile := t.TempDir() + "/token"
	os.WriteFile(tokenFile, []byte("file-token\n"), 0o600) // pragma: allowlist secret
	t.Setenv("STATUS_NAMESPACE", "")
	t.Setenv("KUBE_API_SERVER", "")
	t.Setenv("TEAMS_WEBHOOK_URL", "")
	t.Setenv("STATUS_WEBHOOK_URL", "")
	t.Setenv("HOMERUN_PITCH_URL", srv.URL+"/pitch")
	t.Setenv("HOMERUN_AUTH_TOKEN", "env-token")    // pragma: allowlist secret
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", tokenFile) // wins over the env token

	if statusNamespace() != "" {
		t.Skip("running in a pod: the ServiceAccount namespace switches the ConfigMap sink on")
	}

	sent, err := Report(fakeActivity{in: testReport()})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sent.([]string), ","); got != "homerun" {
		t.Fatalf("sent %q", got)
	}
	if got := hdr.Get("Authorization"); got != "Bearer file-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if (*body)["title"] != "u26-kind1: ready" || (*body)["system"] != "cluster-build-watch" {
		t.Fatalf("body %v", *body)
	}
}

// With a token, plain http is refused before anything goes on the wire.
func TestHomerunRefusesPlainHTTPWithToken(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", "s3cr3t-token") // pragma: allowlist secret

	err := pitchHomerun(srv.URL+"/pitch", testReport())
	if err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if called {
		t.Fatal("the request went out over plain http")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Fatalf("token in error: %v", err)
	}

	// Without a token plain http is fine (in-cluster pitcher without auth).
	t.Setenv("HOMERUN_AUTH_TOKEN", "")
	if err := pitchHomerun(srv.URL+"/pitch", testReport()); err != nil || !called {
		t.Fatalf("plain http without a token: err=%v called=%v", err, called)
	}
}

// A server that echoes the Authorization header back must not get the token
// into the error, which lands in the workflow history and the logs.
func TestHomerunTokenNeverInErrors(t *testing.T) {
	const token = "very-secret-token" // pragma: allowlist secret
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "bad header: "+r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	old := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = old }()
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", token)

	err := pitchHomerun(srv.URL+"/pitch", testReport())
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("want the 401, got %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked: %v", err)
	}

	// Transport error: closed port.
	err = pitchHomerun("https://127.0.0.1:1/pitch?"+token, testReport())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("want a redacted transport error, got %v", err)
	}

	// Unreadable token file is an error, not "no auth".
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", t.TempDir()+"/missing")
	if err := pitchHomerun(srv.URL+"/pitch", testReport()); err == nil || !strings.Contains(err.Error(), "HOMERUN_AUTH_TOKEN_FILE") {
		t.Fatalf("got %v", err)
	}
}

// No HOMERUN_PITCH_URL, no pitch -- and a homerun failure leaves the other
// sinks delivering.
func TestHomerunSinkOffAndIndependent(t *testing.T) {
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }))
	defer kube.Close()
	t.Setenv("KUBE_API_SERVER", kube.URL)
	t.Setenv("STATUS_NAMESPACE", "watch-ns")
	t.Setenv("TEAMS_WEBHOOK_URL", "")
	t.Setenv("STATUS_WEBHOOK_URL", "")
	t.Setenv("HOMERUN_PITCH_URL", "")
	t.Setenv("HOMERUN_AUTH_TOKEN_FILE", "")
	t.Setenv("HOMERUN_AUTH_TOKEN", "")

	sent, err := Report(fakeActivity{in: testReport()})
	if err != nil || strings.Join(sent.([]string), ",") != "configmap" {
		t.Fatalf("sink off: sent %v err %v", sent, err)
	}

	srv, _, _ := homerunServer(t, http.StatusServiceUnavailable, `{"status":"error","message":"Failed to enqueue message"}`)
	t.Setenv("HOMERUN_PITCH_URL", srv.URL+"/pitch")
	sent, err = Report(fakeActivity{in: testReport()})
	if err == nil || !strings.Contains(err.Error(), "homerun: HTTP 503") {
		t.Fatalf("want the homerun failure reported, got %v", err)
	}
	if got := strings.Join(sent.([]string), ","); got != "configmap" {
		t.Fatalf("configmap must still deliver, sent %q", got)
	}
}
