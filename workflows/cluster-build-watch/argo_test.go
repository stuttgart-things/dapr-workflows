package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	rs "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// app builds an Application as machinery returns it, with the info fields of
// the flux watch-config (Project, Sync, Health, Operation).
func app(name, project, sync, health string, conds ...*rs.Condition) *rs.ResourceStatus {
	info := map[string]string{infoProject: project, "Revision": "f1fd2f9b"}
	if sync != "" {
		info[infoSync] = sync
	}
	if health != "" {
		info[infoHealth] = health
	}
	return &rs.ResourceStatus{Name: name, Namespace: "argocd", Kind: "Application", InfoFields: info, Conditions: conds}
}

func argoTarget(project string) ArgoTarget {
	in := testArgoInput(project)
	return *in.Argo
}

func testArgoInput(project string) *Input {
	in := testInput(false)
	in.Target.Name = project
	in.Argo = &ArgoTarget{ObservationSource: ObservationSource{
		Source: sourceMachinery, Machinery: &MachinerySource{Server: bufServer, Plaintext: true},
	}}
	in.applyDefaults()
	return in
}

// The live shape on platform-sthings (2026-10-02): inner apps in the cluster's
// project, outer install apps and the AppProject app in `default`, two
// clusters whose names are suffixes of each other's apps.
func fleet() []*rs.ResourceStatus {
	orphan := cond("OrphanedResourceWarning", "True", "", "Application has 1 orphaned resources")
	return []*rs.ResourceStatus{
		app("cilium-lb-app-dev", "app-dev", "Synced", "Healthy", orphan),
		app("cert-manager-24630576", "app-dev", "Synced", "Healthy"),
		app("cert-manager-install-app-dev", "default", "Synced", "Healthy"),
		app("proj-app-dev", "default", "Synced", "Healthy"),
		app("cilium-lb-dev", "dev", "Synced", "Healthy"),
		app("proj-dev", "default", "Synced", "Healthy"),
		app("cert-manager-install-homerun2-dev2", "default", "Synced", "Healthy"),
		app("base-platform", "default", "Synced", "Healthy"),
		app("cilium-lb-homerun2-dev2", "homerun2-dev2", "OutOfSync", "Progressing"),
	}
}

func TestArgoFromMachineryFilter(t *testing.T) {
	o := argoFromMachinery(fleet(), argoTarget("app-dev"))
	if o.Total != 4 || o.Ready != 4 || o.Degraded {
		t.Fatalf("app-dev: %+v", o)
	}
	// "proj-app-dev" ends in "-dev" too, but app-dev is the longer match.
	o = argoFromMachinery(fleet(), argoTarget("dev"))
	if o.Total != 2 {
		t.Fatalf("dev must get cilium-lb-dev and proj-dev only: %+v", o)
	}
	o = argoFromMachinery(fleet(), argoTarget("homerun2-dev2"))
	if o.Total != 2 || o.Ready != 1 || len(o.Pending) != 1 || o.Pending[0] != "cilium-lb-homerun2-dev2 (OutOfSync/Progressing)" {
		t.Fatalf("homerun2-dev2: %+v", o)
	}

	off := argoTarget("app-dev")
	no := false
	off.IncludeDefaultProject = &no
	if o := argoFromMachinery(fleet(), off); o.Total != 2 {
		t.Fatalf("without the default project: %+v", o)
	}
	other := argoTarget("app-dev")
	other.Namespace = "argocd-2"
	if o := argoFromMachinery(fleet(), other); o.Total != 0 {
		t.Fatalf("other namespace must match nothing: %+v", o)
	}
	if o := argoFromMachinery(nil, argoTarget("app-dev")); o.Total != 0 || o.Degraded {
		t.Fatalf("empty: %+v", o)
	}
}

func TestArgoFromMachineryJudgement(t *testing.T) {
	apps := []*rs.ResourceStatus{
		app("a", "c1", "Synced", "Healthy"),
		app("b", "c1", "Synced", "Degraded"),
		app("c", "c1", "OutOfSync", "Healthy", cond("ComparisonError", "True", "", "repo not reachable")),
		app("d", "c1", "", ""), // server without the Sync/Health mapping
	}
	failed := app("e", "c1", "OutOfSync", "Healthy")
	failed.InfoFields[infoOperation] = "Failed"
	apps = append(apps, failed)

	o := argoFromMachinery(apps, argoTarget("c1"))
	if o.Total != 5 || o.Ready != 1 || !o.Degraded {
		t.Fatalf("%+v", o)
	}
	for _, want := range []string{"b: health Degraded", "c: ComparisonError: repo not reachable", "e: sync Failed"} {
		if !strings.Contains(o.Message, want) {
			t.Errorf("message %q lacks %q", o.Message, want)
		}
	}
	if !strings.Contains(strings.Join(o.Pending, ","), "d (-/-)") {
		t.Errorf("missing info fields must read as not ready: %v", o.Pending)
	}

	// Warnings are not errors.
	ok := argoFromMachinery([]*rs.ResourceStatus{app("a", "c1", "Synced", "Healthy",
		cond("OrphanedResourceWarning", "True", "", "x"))}, argoTarget("c1"))
	if ok.Degraded || ok.Ready != 1 {
		t.Fatalf("warning condition: %+v", ok)
	}

	// Pending is capped, with a count of the rest.
	var many []*rs.ResourceStatus
	for i := range 12 {
		many = append(many, app(strings.Repeat("x", i+1), "c1", "OutOfSync", "Missing"))
	}
	o = argoFromMachinery(many, argoTarget("c1"))
	if len(o.Pending) != maxPending+1 || o.Pending[maxPending] != "+4 more" {
		t.Fatalf("pending cap: %v", o.Pending)
	}
}

// Through gRPC (bufconn): GetResources with kind Application, token over TLS.
func TestObserveArgoMachinery(t *testing.T) {
	cert, pool := selfSigned(t)
	t.Setenv("MACHINERY_AUTH_TOKEN", fakeToken)
	f := newFake()
	f.kinds["Application"] = true
	f.lists = map[string][]*rs.ResourceStatus{"Application": fleet()}
	startMachinery(t, f, cert, pool)

	tg := argoTarget("app-dev")
	tg.Machinery.Plaintext = false
	o, err := observeArgoMachinery(context.Background(), tg)
	if err != nil || o.Total != 4 || o.Ready != 4 {
		t.Fatalf("%+v %v", o, err)
	}
	if f.lastKind != "Application" {
		t.Fatalf("listed kind %q", f.lastKind)
	}
	if got := f.auth(); len(got) != 1 || got[0] != "Bearer "+fakeToken {
		t.Fatalf("authorization header: %v", got)
	}

	// Kind not configured on the server: an error, never "zero Applications".
	tg.Machinery.Kind = "Applications"
	if _, err := observeArgoMachinery(context.Background(), tg); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unconfigured kind: %v", err)
	}
	// Configured but not served (no Argo CD there): an empty list -- the
	// grace period in AdvanceArgo is what turns this into a failure.
	f.unserved["Application"] = true
	tg.Machinery.Kind = "Application"
	if o, err := observeArgoMachinery(context.Background(), tg); err != nil || o.Total != 0 {
		t.Fatalf("unserved kind: %+v %v", o, err)
	}
}

func TestArgoRegisterFromXR(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"rancher": map[string]any{"argocd": map[string]any{"register": true}}}}
	if o := parseXR(obj); o.ArgoRegister == nil || !*o.ArgoRegister {
		t.Fatalf("kube: %+v", o)
	}
	if o := parseXR(map[string]any{}); o.ArgoRegister != nil {
		t.Fatalf("no field must stay nil: %+v", o)
	}

	cs := clusterStack("ready", "true", cond("Ready", "True", "Available", ""))
	cs.InfoFields[infoArgoRegister] = "false"
	o, err := xrFromMachinery("ClusterStack", cs)
	if err != nil || o.ArgoRegister == nil || *o.ArgoRegister {
		t.Fatalf("machinery false: %+v %v", o, err)
	}
	cs.InfoFields[infoArgoRegister] = "yes"
	if _, err := xrFromMachinery("ClusterStack", cs); err == nil {
		t.Fatal("a non-bool ArgoRegister must be an error")
	}
	delete(cs.InfoFields, infoArgoRegister)
	if o, _ := xrFromMachinery("ClusterStack", cs); o.ArgoRegister != nil {
		t.Fatalf("unmapped must stay nil: %+v", o)
	}
}

var readyXR = func(register *bool) *XRObservation {
	return &XRObservation{Found: true, Stage: "ready", Ready: bptr(true), ReadyCondition: true, ArgoRegister: register}
}

func TestAdvanceArgoGate(t *testing.T) {
	// No argo block: ready as before, no mention of Argo.
	in := testInput(false)
	st := newState(in, t0)
	evs := Advance(in, st, nil, readyXR(bptr(true)), t0.Add(time.Minute))
	if types(evs) != "ready" || strings.Contains(evs[0].Message, "argo") {
		t.Fatalf("no argo: %q %v", types(evs), evs)
	}

	for name, c := range map[string]struct {
		register *bool
		always   bool
		want     string
		msg      string
	}{
		"registered":     {bptr(true), false, "stage", "waiting for the Argo CD Applications of project app-dev"},
		"not registered": {bptr(false), false, "ready", "not registered with Argo CD"},
		"unknown":        {nil, false, "ready", "argocd.register is unknown"},
		"always":         {nil, true, "stage", "argo-sync"},
	} {
		in := testArgoInput("app-dev")
		in.Argo.Always = c.always
		st := newState(in, t0)
		evs := Advance(in, st, nil, readyXR(c.register), t0.Add(time.Minute))
		if types(evs) != c.want || !strings.Contains(evs[0].Message, c.msg) {
			t.Errorf("%s: got %q %+v", name, types(evs), evs)
		}
		if c.want == "stage" && (st.Stage != stageArgoSync || st.done()) {
			t.Errorf("%s: want stage argo-sync, still watching; got %s %s", name, st.Stage, st.Phase)
		}
	}
}

// From XR ready to green: zero apps first, then partial, then two green polls.
func TestAdvanceArgoHappyPath(t *testing.T) {
	in := testArgoInput("app-dev")
	st := newState(in, t0)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	Advance(in, st, nil, readyXR(bptr(true)), at(10))

	// An XR observation in argo-sync is ignored, not a second ready.
	if got := types(Advance(in, st, nil, readyXR(bptr(true)), at(10))); got != "" || st.Stage != stageArgoSync {
		t.Fatalf("Advance must leave argo-sync alone: %q %s", got, st.Stage)
	}
	if got := types(AdvanceArgo(in, st, &ArgoObservation{}, at(11))); got != "" || !strings.Contains(st.Message, "to be generated") {
		t.Fatalf("zero apps within grace: %q %s", got, st.Message)
	}
	if got := types(AdvanceArgo(in, st, &ArgoObservation{Total: 4, Ready: 2, Pending: []string{"a (OutOfSync/Missing)", "b (Synced/Progressing)"}}, at(12))); got != "" ||
		!strings.Contains(st.Message, "2/4") || !strings.Contains(st.Message, "a (OutOfSync/Missing)") {
		t.Fatalf("partial: %q %s", got, st.Message)
	}
	// First green poll only confirms.
	if got := types(AdvanceArgo(in, st, &ArgoObservation{Total: 4, Ready: 4}, at(13))); got != "" || st.done() {
		t.Fatalf("one green poll must not finish: %q", got)
	}
	// More Applications appeared (app-of-apps children): start over.
	if got := types(AdvanceArgo(in, st, &ArgoObservation{Total: 6, Ready: 6}, at(14))); got != "" || st.done() {
		t.Fatalf("a changed count must reset the confirmation: %q", got)
	}
	evs := AdvanceArgo(in, st, &ArgoObservation{Total: 6, Ready: 6}, at(15))
	if types(evs) != "ready" || st.Phase != phaseReady || !strings.Contains(evs[0].Message, "6 Argo CD Applications") {
		t.Fatalf("want ready: %q %+v", types(evs), evs)
	}
	last := evs[0].Stages[len(evs[0].Stages)-1]
	if last.Stage != stageArgoSync || last.Duration() != 5*time.Minute {
		t.Fatalf("argo-sync record: %+v", last)
	}
	if AdvanceArgo(in, st, &ArgoObservation{Total: 6, Ready: 6}, at(16)) != nil {
		t.Fatal("a finished watch must not emit anything")
	}
}

func TestAdvanceArgoZeroAppsFailsAfterGrace(t *testing.T) {
	in := testArgoInput("app-dev")
	st := newState(in, t0)
	Advance(in, st, nil, readyXR(bptr(true)), t0)
	if got := types(AdvanceArgo(in, st, &ArgoObservation{}, t0.Add(14*time.Minute))); got != "" {
		t.Fatalf("within grace: %q", got)
	}
	evs := AdvanceArgo(in, st, &ArgoObservation{}, t0.Add(15*time.Minute))
	if types(evs) != "failed" || st.Phase != phaseFailed || !strings.Contains(st.Message, "no Argo CD Application in project app-dev") {
		t.Fatalf("want failed after grace: %q %s", types(evs), st.Message)
	}
}

func TestAdvanceArgoStuckDegradedTimeout(t *testing.T) {
	in := testArgoInput("app-dev")
	in.TimeoutMin = 60
	st := newState(in, t0)
	Advance(in, st, nil, readyXR(bptr(true)), t0)
	part := &ArgoObservation{Total: 3, Ready: 2, Pending: []string{"x (OutOfSync/Healthy)"}}

	if got := types(AdvanceArgo(in, st, &ArgoObservation{Total: 3, Ready: 2, Degraded: true, Message: "x: sync Failed"}, t0.Add(time.Minute))); got != "degraded" {
		t.Fatalf("degraded: %q", got)
	}
	if got := types(AdvanceArgo(in, st, part, t0.Add(2*time.Minute))); got != "recovered" {
		t.Fatalf("recovered: %q", got)
	}
	evs := AdvanceArgo(in, st, part, t0.Add(30*time.Minute))
	if types(evs) != "stuck" || !strings.Contains(evs[0].Message, "x (OutOfSync/Healthy)") {
		t.Fatalf("stuck after the argo-sync limit, naming what waits: %q %+v", types(evs), evs)
	}
	if got := types(AdvanceArgo(in, st, part, t0.Add(31*time.Minute))); got != "" {
		t.Fatalf("stuck once: %q", got)
	}
	// A failing observation still ends the watch at the overall timeout.
	if got := types(AdvanceArgo(in, st, nil, t0.Add(60*time.Minute))); got != "failed" {
		t.Fatalf("overall timeout: %q", got)
	}
}

func TestValidateArgo(t *testing.T) {
	in := testArgoInput("app-dev")
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}
	if in.Argo.Project != "app-dev" || in.Argo.Namespace != "argocd" || in.Argo.Machinery.Kind != "Application" ||
		in.Argo.GraceMin != 15 || in.Argo.SettlePolls != 2 || !in.Argo.includeDefault() {
		t.Fatalf("defaults: %+v %+v", in.Argo, in.Argo.Machinery)
	}
	if in.stageTimeout(stageArgoSync) != 30*time.Minute {
		t.Fatal("argo-sync default timeout")
	}

	kube := testArgoInput("app-dev")
	kube.Argo.ObservationSource = ObservationSource{}
	if err := kube.validate(); err == nil || !strings.Contains(err.Error(), "argo.source must be machinery") {
		t.Fatalf("kube source must be rejected: %v", err)
	}
	noServer := testArgoInput("app-dev")
	noServer.Argo.Machinery.Server = ""
	if noServer.validate() == nil {
		t.Fatal("machinery without server must be rejected")
	}

	// JSON as the trigger posts it.
	var j Input
	raw := `{"target":{"namespace":"default","name":"app-dev"},
		"argo":{"project":"p","includeDefaultProject":false,"source":"machinery","machinery":{"server":"m:443"}}}`
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatal(err)
	}
	j.applyDefaults()
	if err := j.validate(); err != nil || j.Argo.Project != "p" || j.Argo.includeDefault() {
		t.Fatalf("%+v %v", j.Argo, err)
	}
	// An input without argo marshals as before.
	b, _ := json.Marshal(testInput(false))
	if strings.Contains(string(b), `"argo"`) {
		t.Fatalf("input grew an argo key: %s", b)
	}
}
