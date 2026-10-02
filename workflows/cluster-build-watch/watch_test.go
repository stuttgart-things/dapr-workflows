package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

func testInput(gitops bool) *Input {
	in := &Input{Target: XRTarget{Namespace: "crossplane-system", Name: "u26-kind1"}}
	if gitops {
		in.GitOps = &GitOpsTarget{Name: "u26-kind1", Revision: "abc1234"}
	}
	in.applyDefaults()
	return in
}

func types(evs []Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, ",")
}

func bptr(b bool) *bool { return &b }

// The whole happy path: sync, XR appears, walks its stages, gets ready.
func TestAdvanceHappyPath(t *testing.T) {
	in := testInput(true)
	st := newState(in, t0)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

	if got := types(Advance(in, st, &GitOpsObservation{Found: true, Revision: "000aaaa"}, nil, at(1))); got != "" {
		t.Fatalf("old revision must not count as synced, got events %q", got)
	}
	if got := types(Advance(in, st, &GitOpsObservation{Found: true, Synced: true, Revision: "abc1234ffff"}, nil, at(2))); got != "synced" {
		t.Fatalf("want synced, got %q", got)
	}
	if st.Stage != stageXRPending {
		t.Fatalf("after sync want stage %s, got %s", stageXRPending, st.Stage)
	}
	if got := types(Advance(in, st, nil, &XRObservation{Found: false}, at(3))); got != "" {
		t.Fatalf("missing XR before it was ever seen is waiting, got %q", got)
	}

	for i, stage := range []string{"vm", "baseos", "distribution", "kubeconfig", "access", "platform"} {
		evs := Advance(in, st, nil, &XRObservation{Found: true, Stage: stage, Ready: bptr(false)}, at(10+i*5))
		if types(evs) != "stage" || evs[0].Stage != stage {
			t.Fatalf("stage %s: got %q", stage, types(evs))
		}
		// Polling the same stage again is silent.
		if got := types(Advance(in, st, nil, &XRObservation{Found: true, Stage: stage}, at(11+i*5))); got != "" {
			t.Fatalf("repeat poll of %s emitted %q", stage, got)
		}
	}

	evs := Advance(in, st, nil, &XRObservation{Found: true, Stage: "ready", Ready: bptr(true), ReadyCondition: true}, at(50))
	if types(evs) != "ready" || st.Phase != phaseReady {
		t.Fatalf("want ready, got %q phase %s", types(evs), st.Phase)
	}
	// gitops-sync, xr-pending, six XR stages.
	if n := len(evs[0].Stages); n != 8 {
		t.Fatalf("final event should carry 8 stage records, got %d", n)
	}
	for _, r := range evs[0].Stages {
		if r.End.IsZero() {
			t.Fatalf("stage %s left open after ready", r.Stage)
		}
	}
	if evs[0].Stages[0].Duration() != 2*time.Minute {
		t.Fatalf("gitops-sync took 2m, recorded %s", evs[0].Stages[0].Duration())
	}
	if Advance(in, st, nil, &XRObservation{Found: true}, at(51)) != nil {
		t.Fatal("a finished watch must not emit anything")
	}
}

// status.ready true while Crossplane's Ready condition is False is NOT done --
// the case the bootstrap/cluster README calls "a stack can look finished and
// not be".
func TestAdvanceReadyNeedsBothSignals(t *testing.T) {
	in := testInput(false)
	st := newState(in, t0)
	Advance(in, st, nil, &XRObservation{Found: true, Stage: "ready", Ready: bptr(true), ReadyCondition: false}, t0.Add(time.Minute))
	if st.done() {
		t.Fatal("status.ready alone must not finish the watch")
	}
	Advance(in, st, nil, &XRObservation{Found: true, Stage: "ready", Ready: bptr(false), ReadyCondition: true}, t0.Add(2*time.Minute))
	if st.done() {
		t.Fatal("Ready condition alone must not finish the watch when status.ready is false")
	}
	// An XRD without status.ready: the condition decides.
	Advance(in, st, nil, &XRObservation{Found: true, ReadyCondition: true}, t0.Add(3*time.Minute))
	if st.Phase != phaseReady {
		t.Fatalf("want Ready, got %s", st.Phase)
	}
}

func TestAdvanceStuckOncePerStage(t *testing.T) {
	in := testInput(false)
	in.Target.StageTimeoutMin = map[string]int{"vm": 5}
	st := newState(in, t0)
	Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0)

	if got := types(Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0.Add(4*time.Minute))); got != "" {
		t.Fatalf("before the limit: %q", got)
	}
	if got := types(Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0.Add(5*time.Minute))); got != "stuck" {
		t.Fatalf("at the limit want stuck, got %q", got)
	}
	if got := types(Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0.Add(9*time.Minute))); got != "" {
		t.Fatalf("stuck must be reported once, got %q", got)
	}
	if st.done() {
		t.Fatal("a stuck stage is a warning, not a failure")
	}
	// The next stage gets its own clock and its own warning.
	Advance(in, st, nil, &XRObservation{Found: true, Stage: "baseos"}, t0.Add(10*time.Minute))
	if st.StuckNotified {
		t.Fatal("entering a new stage must reset the stuck flag")
	}
}

func TestAdvanceOverallTimeoutFailsEvenWithoutObservations(t *testing.T) {
	in := testInput(true)
	in.TimeoutMin = 60
	st := newState(in, t0)
	// Every observation failed (RBAC, API down): only the clocks run.
	got := types(Advance(in, st, nil, nil, t0.Add(20*time.Minute)))
	if got != "stuck" {
		t.Fatalf("gitops-sync limit is 15m, want stuck, got %q", got)
	}
	got = types(Advance(in, st, nil, nil, t0.Add(60*time.Minute)))
	if got != "failed" || st.Phase != phaseFailed {
		t.Fatalf("want failed at the overall timeout, got %q / %s", got, st.Phase)
	}
}

func TestAdvanceXRDeletedAfterSeenFails(t *testing.T) {
	in := testInput(false)
	st := newState(in, t0)
	Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0.Add(time.Minute))
	got := types(Advance(in, st, nil, &XRObservation{Found: false}, t0.Add(2*time.Minute)))
	if got != "failed" {
		t.Fatalf("want failed, got %q", got)
	}
}

func TestAdvanceDegradedReportedOnTransitionsOnly(t *testing.T) {
	in := testInput(false)
	st := newState(in, t0)
	bad := &XRObservation{Found: true, Stage: "vm", SyncedFalse: true, Message: "ReconcileError: boom"}
	if got := types(Advance(in, st, nil, bad, t0.Add(time.Minute))); got != "degraded,stage" {
		t.Fatalf("got %q", got)
	}
	if got := types(Advance(in, st, nil, bad, t0.Add(2*time.Minute))); got != "" {
		t.Fatalf("persisting error must stay quiet, got %q", got)
	}
	if got := types(Advance(in, st, nil, &XRObservation{Found: true, Stage: "vm"}, t0.Add(3*time.Minute))); got != "recovered" {
		t.Fatalf("got %q", got)
	}
}

func TestEventSeqIsMonotonic(t *testing.T) {
	in := testInput(false)
	st := newState(in, t0)
	var seqs []int
	for i, s := range []string{"vm", "baseos", "distribution"} {
		for _, e := range Advance(in, st, nil, &XRObservation{Found: true, Stage: s}, t0.Add(time.Duration(i)*time.Minute)) {
			seqs = append(seqs, e.Seq)
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not increasing: %v", seqs)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Input){
		"no target name":  func(in *Input) { in.Target.Name = "" },
		"bad gitops kind": func(in *Input) { in.GitOps.Kind = "jenkins" },
		"short revision":  func(in *Input) { in.GitOps.Revision = "abc" },
		"core apiVersion": func(in *Input) { in.Target.APIVersion = "v1" },
		"gitops no name":  func(in *Input) { in.GitOps.Name = "" },
	}
	for name, mutate := range cases {
		in := testInput(true)
		mutate(in)
		if in.validate() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := testInput(true).validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func TestValidateSource(t *testing.T) {
	m := func(server string) *MachinerySource { return &MachinerySource{Server: server} }
	bad := map[string]func(*Input){
		"unknown target source":        func(in *Input) { in.Target.Source = "etcd" },
		"unknown gitops source":        func(in *Input) { in.GitOps.Source = "grpc" },
		"machinery without server":     func(in *Input) { in.Target.Source = sourceMachinery; in.Target.Machinery = m("") },
		"machinery without block":      func(in *Input) { in.Target.Source = sourceMachinery },
		"argocd through machinery":     func(in *Input) { in.GitOps.Source = sourceMachinery; in.GitOps.Machinery = m("h:443") },
		"machinery block, kube source": func(in *Input) { in.Target.Machinery = m("h:443") },
	}
	for name, mutate := range bad {
		in := testInput(true)
		mutate(in)
		in.applyDefaults()
		if in.validate() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	in := testInput(true)
	in.GitOps.Kind = "flux"
	in.GitOps.ObservationSource = ObservationSource{Source: sourceMachinery, Machinery: m("h:443")}
	in.Target.ObservationSource = ObservationSource{Source: sourceMachinery, Machinery: m("h:443")}
	in.applyDefaults()
	if err := in.validate(); err != nil {
		t.Fatalf("valid machinery input rejected: %v", err)
	}
	if in.GitOps.Machinery.Kind != "Kustomization" || in.Target.Machinery.Kind != "ClusterStack" {
		t.Fatalf("machinery kind defaults: %+v %+v", in.GitOps.Machinery, in.Target.Machinery)
	}
	if err := testInput(true).validate(); err != nil || testInput(true).Target.Source != "" {
		t.Fatalf("no source is kube, and stays unset: %v", err)
	}
	kube := testInput(false)
	kube.Target.Source = sourceKube
	if err := kube.validate(); err != nil {
		t.Fatalf("explicit kube rejected: %v", err)
	}
}

// The input JSON the trigger posts: source/machinery sit next to name.
func TestMachineryInputJSON(t *testing.T) {
	var in Input
	raw := `{"gitops":{"kind":"flux","name":"machinery-xrs","source":"machinery","machinery":{"server":"m:443"}},
		"target":{"namespace":"default","name":"app-dev","source":"machinery","machinery":{"server":"m:443","plaintext":true}}}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	in.applyDefaults()
	if err := in.validate(); err != nil {
		t.Fatal(err)
	}
	if !in.Target.fromMachinery() || !in.Target.Machinery.Plaintext || in.GitOps.Machinery.Server != "m:443" {
		t.Fatalf("got %+v / %+v", in.Target, in.GitOps)
	}
	// An input without the fields marshals exactly as before.
	b, _ := json.Marshal(testInput(false).Target)
	if strings.Contains(string(b), `"source"`) || strings.Contains(string(b), `"machinery"`) {
		t.Fatalf("kube input grew new keys: %s", b)
	}
}

// The example inputs in this directory stay valid.
func TestExampleInputs(t *testing.T) {
	for _, f := range []string{"input.json", "input-machinery.json", "input-argo.json"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var in Input
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		in.applyDefaults()
		if err := in.validate(); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestStageTimeoutsAllPaths(t *testing.T) {
	in := testInput(false)
	for stage, min := range map[string]int{
		"rancher": 30, "node-ip": 15, "join": 30, "management-plane": 45, "ready": 10,
		"vm": 30, "baseos": 30, "distribution": 30, "kubeconfig": 10, "access": 10, "platform": 45,
		"argo-sync": 30,
	} {
		if got := in.stageTimeout(stage); got != time.Duration(min)*time.Minute {
			t.Errorf("stage %s: %s, want %dm", stage, got, min)
		}
	}
	in.Target.StageTimeoutMin = map[string]int{"ready": 3}
	if in.stageTimeout("ready") != 3*time.Minute {
		t.Fatal("an override wins over a new built-in")
	}
}

func TestDefaults(t *testing.T) {
	in := &Input{GitOps: &GitOpsTarget{Kind: "flux", Name: "x"}, Target: XRTarget{Namespace: "n", Name: "c"}}
	in.applyDefaults()
	if in.GitOps.Namespace != "flux-system" || in.Name != "c" || in.Target.Resource != "clusterstacks" {
		t.Fatalf("defaults not applied: %+v", in)
	}
	if in.stageTimeout("vm") != 30*time.Minute || in.stageTimeout("something-new") != 30*time.Minute {
		t.Fatal("stage timeouts")
	}
}

func TestShortRev(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "-",
		"abc1234":              "abc1234",
		"0123456789abcdef0123": "0123456789ab", // pragma: allowlist secret -- fake SHA
		"main@sha1:0123456789abcdef0123456789abcd": "main@sha1:0123456789ab",
	} {
		if got := shortRev(in); got != want {
			t.Errorf("shortRev(%q) = %q, want %q", in, got, want)
		}
	}
}
