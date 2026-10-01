package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
