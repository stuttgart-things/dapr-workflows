//go:build live

// Read-only smoke test against a real machinery endpoint. Not part of CI:
//
//	MACHINERY_SERVER=machinery-grpc.machinery.4sthings.tiab.ssc.sva.de:443 \
//	  go test -tags live -run TestLiveMachinery -v .
//
// Optional: LIVE_CLUSTERSTACK=<ns>/<name>, LIVE_KUSTOMIZATION=<ns>/<name>,
// LIVE_REVISION=<sha prefix>. Uses the system CA pool (SSL_CERT_FILE works)
// and MACHINERY_AUTH_TOKEN(_FILE) like the worker does.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func liveRef(env, def string) (string, string) {
	v := os.Getenv(env)
	if v == "" {
		v = def
	}
	ns, name, _ := strings.Cut(v, "/")
	return ns, name
}

func TestLiveMachinery(t *testing.T) {
	server := os.Getenv("MACHINERY_SERVER")
	if server == "" {
		t.Skip("MACHINERY_SERVER not set")
	}
	ctx := context.Background()

	in := Input{
		GitOps: &GitOpsTarget{Kind: "flux", Revision: os.Getenv("LIVE_REVISION"),
			ObservationSource: ObservationSource{Source: sourceMachinery, Machinery: &MachinerySource{Server: server}}},
		Target: XRTarget{ObservationSource: ObservationSource{Source: sourceMachinery, Machinery: &MachinerySource{Server: server}}},
	}
	in.GitOps.Namespace, in.GitOps.Name = liveRef("LIVE_KUSTOMIZATION", "flux-system/machinery-xrs")
	in.Target.Namespace, in.Target.Name = liveRef("LIVE_CLUSTERSTACK", "default/app-dev")
	in.applyDefaults()
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}

	show := func(label string, v any) {
		b, _ := json.Marshal(v)
		t.Logf("%s: %s", label, b)
	}

	// The raw info fields, to see the labels the server maps.
	for _, ref := range []struct{ kind, ns, name string }{
		{in.GitOps.Machinery.Kind, in.GitOps.Namespace, in.GitOps.Name},
		{in.Target.Machinery.Kind, in.Target.Namespace, in.Target.Name},
	} {
		c, err := newMachineryClient(in.Target.Machinery)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.detail(ctx, ref.kind, ref.ns, ref.name)
		if err != nil {
			t.Fatalf("%s: %v", ref.kind, err)
		}
		if r != nil {
			show("raw "+ref.kind+" infoFields", r.GetInfoFields())
		}
	}

	g, err := observeGitOpsMachinery(ctx, *in.GitOps)
	if err != nil {
		t.Fatalf("gitops: %v", err)
	}
	show("Kustomization "+in.GitOps.Namespace+"/"+in.GitOps.Name, g)

	x, err := observeXRMachinery(ctx, in.Target)
	if err != nil {
		t.Fatalf("xr: %v", err)
	}
	show("ClusterStack "+in.Target.Namespace+"/"+in.Target.Name, x)

	// What a watch would make of it, starting at each side.
	st := newState(&in, t0)
	for _, ev := range Advance(&in, st, &g, nil, t0) {
		show("event", ev)
	}
	for _, ev := range Advance(&in, st, nil, &x, t0) {
		show("event", ev)
	}
	t.Logf("watch state: phase=%s stage=%s message=%q", st.Phase, st.Stage, st.Message)

	x2, err := observeXRMachinery(ctx, XRTarget{Namespace: "default", Name: "does-not-exist-cbw-smoke",
		ObservationSource: in.Target.ObservationSource})
	if err != nil {
		t.Fatalf("absent xr: %v", err)
	}
	show("ClusterStack default/does-not-exist-cbw-smoke", x2)
}

// TestLiveArgo lists the Applications of one project through a real machinery
// on an Argo CD cluster:
//
//	LIVE_ARGO_SERVER=machinery-grpc.<argo cluster domain>:443 LIVE_ARGO_PROJECT=app-dev \
//	  go test -tags live -run TestLiveArgo -v .
//
// LIVE_ARGO_PLAINTEXT=true for a local or in-cluster machinery. Optionally
// LIVE_XR_SERVER + LIVE_CLUSTERSTACK to read ArgoRegister from the stack.
func TestLiveArgo(t *testing.T) {
	server := os.Getenv("LIVE_ARGO_SERVER")
	if server == "" {
		t.Skip("LIVE_ARGO_SERVER not set")
	}
	plain := os.Getenv("LIVE_ARGO_PLAINTEXT") == "true"
	in := Input{
		Target: XRTarget{Namespace: "default", Name: os.Getenv("LIVE_ARGO_PROJECT")},
		Argo: &ArgoTarget{ObservationSource: ObservationSource{Source: sourceMachinery,
			Machinery: &MachinerySource{Server: server, Plaintext: plain}}},
	}
	in.applyDefaults()
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}
	o, err := observeArgoMachinery(context.Background(), *in.Argo)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	t.Logf("project %s: %s", in.Argo.Project, b)

	if xs := os.Getenv("LIVE_XR_SERVER"); xs != "" {
		ns, name := liveRef("LIVE_CLUSTERSTACK", "default/"+in.Argo.Project)
		xr, err := observeXRMachinery(context.Background(), XRTarget{Namespace: ns, Name: name,
			ObservationSource: ObservationSource{Source: sourceMachinery,
				Machinery: &MachinerySource{Server: xs, Kind: "ClusterStack", Plaintext: os.Getenv("LIVE_XR_PLAINTEXT") == "true"}}})
		if err != nil {
			t.Fatal(err)
		}
		reg := "<nil>"
		if xr.ArgoRegister != nil {
			reg = strconv.FormatBool(*xr.ArgoRegister)
		}
		t.Logf("ClusterStack %s/%s: stage=%s ready=%v argoRegister=%s", ns, name, xr.Stage, xr.ReadyCondition, reg)
	}
}
