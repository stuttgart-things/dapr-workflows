package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func obj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseArgoApplication(t *testing.T) {
	synced := `{"status":{"sync":{"status":"Synced","revision":"abc1234def"},"health":{"status":"Progressing"},"operationState":{"phase":"Succeeded"}}}` // pragma: allowlist secret -- a fake commit SHA

	o := parseArgoApplication(obj(t, synced), "abc1234")
	if !o.Synced || o.Degraded || o.Health != "Progressing" {
		t.Fatalf("Synced at the wanted revision with Progressing health is synced: %+v", o)
	}
	if o := parseArgoApplication(obj(t, synced), "fff0000"); o.Synced {
		t.Fatal("Synced at ANOTHER revision is the state before the merge, not synced")
	}
	if o := parseArgoApplication(obj(t, synced), ""); !o.Synced {
		t.Fatal("no revision asked for: Synced is enough")
	}

	failed := `{"status":{"sync":{"status":"Synced","revision":"abc1234"},"operationState":{"phase":"Failed","message":"one or more objects failed"}}}`
	o = parseArgoApplication(obj(t, failed), "abc1234")
	if o.Synced || !o.Degraded || !strings.Contains(o.Message, "failed") {
		t.Fatalf("a failed sync operation is degraded, not synced: %+v", o)
	}

	multi := `{"status":{"sync":{"status":"Synced","revisions":["1111111aaaa","abc1234beef"]}}}` // pragma: allowlist secret -- fake SHAs
	o = parseArgoApplication(obj(t, multi), "abc1234")
	if !o.Synced || o.Revision != "abc1234beef" { // pragma: allowlist secret
		t.Fatalf("multi-source: any source at the revision counts: %+v", o)
	}
}

func TestParseFluxKustomization(t *testing.T) {
	ready := `{"status":{"lastAppliedRevision":"main@sha1:abc1234def5678","conditions":[{"type":"Ready","status":"True","reason":"ReconciliationSucceeded"}]}}`
	if o := parseFluxKustomization(obj(t, ready), "abc1234"); !o.Synced {
		t.Fatalf("want synced: %+v", o)
	}
	if o := parseFluxKustomization(obj(t, ready), "9999999"); o.Synced {
		t.Fatal("other revision is not synced")
	}
	failing := `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"BuildFailed","message":"kustomize build failed"}]}}`
	if o := parseFluxKustomization(obj(t, failing), ""); o.Synced || !o.Degraded {
		t.Fatalf("want degraded: %+v", o)
	}
	progressing := `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Progressing"}]}}`
	if o := parseFluxKustomization(obj(t, progressing), ""); o.Degraded {
		t.Fatal("Progressing is not degraded")
	}
}

func TestParseXR(t *testing.T) {
	o := parseXR(obj(t, `{"status":{"stage":"distribution","ready":false,"conditions":[
		{"type":"Synced","status":"True"},
		{"type":"Ready","status":"False","reason":"Creating","message":"Unready resources: ansiblerun"}]}}`))
	if o.Stage != "distribution" || o.Ready == nil || *o.Ready || o.ReadyCondition || o.SyncedFalse {
		t.Fatalf("got %+v", o)
	}
	if !strings.Contains(o.Message, "Unready resources") {
		t.Fatalf("the Ready message says what is pending: %q", o.Message)
	}

	o = parseXR(obj(t, `{"status":{"conditions":[{"type":"Synced","status":"False","reason":"ReconcileError","message":"cannot compose"}]}}`))
	if !o.SyncedFalse || o.Ready != nil || !strings.HasPrefix(o.Message, "ReconcileError") {
		t.Fatalf("got %+v", o)
	}
}

func TestResourcePath(t *testing.T) {
	if p := resourcePath("config.stuttgart-things.com/v1alpha1", "clusterstacks", "ns", "c1"); p != "/apis/config.stuttgart-things.com/v1alpha1/namespaces/ns/clusterstacks/c1" {
		t.Fatal(p)
	}
	if p := resourcePath("v1", "configmaps", "ns", "a.b"); p != "/api/v1/namespaces/ns/configmaps/a.b" {
		t.Fatal(p)
	}
}

func TestKubeClient(t *testing.T) {
	var applied map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/present"):
			io.WriteString(w, `{"status":{"stage":"vm"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/forbidden"):
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"reason":"Forbidden"}`)
		case r.Method == http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/apply-patch+yaml" || r.URL.Query().Get("fieldManager") == "" {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return
			}
			json.NewDecoder(r.Body).Decode(&applied)
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("KUBE_API_SERVER", srv.URL)
	t.Setenv("KUBE_TOKEN", "tok")

	k, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	if o, err := k.get("/x/present"); err != nil || str(o, "status", "stage") != "vm" {
		t.Fatalf("get present: %v %v", o, err)
	}
	if o, err := k.get("/x/absent"); err != nil || o != nil {
		t.Fatalf("404 is nil, nil: %v %v", o, err)
	}
	// A 403 must be an error, never "not found": the watch would otherwise wait
	// for the whole timeout on an object its RBAC cannot see.
	if _, err := k.get("/x/forbidden"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403 must surface: %v", err)
	}
	if err := k.apply("/api/v1/namespaces/n/configmaps/c", map[string]any{"kind": "ConfigMap"}); err != nil || applied["kind"] != "ConfigMap" {
		t.Fatalf("apply: %v %v", applied, err)
	}
}

func TestRevisionMatches(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{"abc1234def", "abc1234", true}, // pragma: allowlist secret
		{"main@sha1:abc1234def", "abc1234", true},
		{"abc1234def", "", true}, // pragma: allowlist secret
		{"", "abc1234", false},
		{"fff1234def", "abc1234", false},
	} {
		if got := revisionMatches(c.have, c.want); got != c.ok {
			t.Errorf("revisionMatches(%q, %q) = %v", c.have, c.want, got)
		}
	}
}
