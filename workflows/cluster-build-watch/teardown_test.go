package main

import (
	"context"
	"strings"
	"testing"
	"time"

	rs "github.com/stuttgart-things/machinery/resourceservice"
)

func testTeardownInput() *TeardownInput {
	in := &TeardownInput{Target: machineryXR(bufServer, true)}
	in.applyDefaults()
	return in
}

func children(c ...string) *TeardownObservation {
	if c == nil {
		c = []string{}
	}
	return &TeardownObservation{Found: true, Deleting: true, ChildrenKnown: true, Children: c}
}

func TestAdvanceTeardownHappyPath(t *testing.T) {
	in := testTeardownInput()
	st := newTeardownState(t0)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

	evs := AdvanceTeardown(in, st, children("default/app-dev-vm", "default/app-dev-baseos", "default/app-dev-k3s"), at(0))
	if types(evs) != "started" || !strings.Contains(evs[0].Message, "3 child resources") {
		t.Fatalf("start: %q %v", types(evs), evs)
	}
	if got := types(AdvanceTeardown(in, st, children("default/app-dev-vm", "default/app-dev-baseos", "default/app-dev-k3s"), at(1))); got != "" {
		t.Fatalf("an unchanged poll is silent, got %q", got)
	}
	evs = AdvanceTeardown(in, st, children("default/app-dev-vm"), at(5))
	if types(evs) != "progress" || !strings.Contains(evs[0].Message, "1 child resources left") ||
		!strings.Contains(evs[0].Message, "gone: default/app-dev-baseos, default/app-dev-k3s") {
		t.Fatalf("progress: %v", evs)
	}
	// machinery drops the empty list: the field going away is "none left".
	evs = AdvanceTeardown(in, st, &TeardownObservation{Found: true, Deleting: true}, at(8))
	if types(evs) != "progress" || !strings.Contains(evs[0].Message, "0 child resources left") {
		t.Fatalf("last child: %v", evs)
	}
	// A failed observation only runs the clock.
	if got := types(AdvanceTeardown(in, st, nil, at(9))); got != "" {
		t.Fatalf("failed observation: %q", got)
	}
	evs = AdvanceTeardown(in, st, &TeardownObservation{Found: false}, at(10))
	if types(evs) != "deleted" || evs[0].Severity != sevSuccess || st.Phase != phaseDeleted || !strings.Contains(evs[0].Message, "gone after 10m0s") {
		t.Fatalf("end: %v %s", evs, st.Phase)
	}
	if len(evs[0].Stages) != 1 || evs[0].Stages[0].Duration() != 10*time.Minute {
		t.Fatalf("final card carries the teardown duration: %+v", evs[0].Stages)
	}
	if AdvanceTeardown(in, st, children(), at(11)) != nil {
		t.Fatal("a finished teardown emits nothing")
	}
}

// Gone before the first look: one card, not started + deleted.
func TestAdvanceTeardownAlreadyGone(t *testing.T) {
	in := testTeardownInput()
	st := newTeardownState(t0)
	evs := AdvanceTeardown(in, st, &TeardownObservation{Found: false}, t0)
	if types(evs) != "deleted" || !strings.Contains(evs[0].Message, "already gone") {
		t.Fatalf("%v", evs)
	}
}

// Without ResourceRefs mapped there is no progress, only start and end.
func TestAdvanceTeardownWithoutChildren(t *testing.T) {
	in := testTeardownInput()
	st := newTeardownState(t0)
	o := &TeardownObservation{Found: true, Deleting: true, Stage: "platform", Message: "Deleting: waiting for composed resources"}
	if got := types(AdvanceTeardown(in, st, o, t0)); got != "started" {
		t.Fatalf("%q", got)
	}
	if got := types(AdvanceTeardown(in, st, o, t0.Add(time.Minute))); got != "" {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(st.Message, "waiting for composed resources") {
		t.Fatalf("message: %q", st.Message)
	}
}

func TestAdvanceTeardownTimeout(t *testing.T) {
	in := testTeardownInput()
	in.TimeoutMin = 30
	st := newTeardownState(t0)
	AdvanceTeardown(in, st, children("default/app-dev-vm"), t0)
	if got := types(AdvanceTeardown(in, st, children("default/app-dev-vm"), t0.Add(29*time.Minute))); got != "" {
		t.Fatalf("%q", got)
	}
	// The timeout fires even when nothing can be observed.
	evs := AdvanceTeardown(in, st, nil, t0.Add(30*time.Minute))
	if types(evs) != "failed" || st.Phase != phaseFailed || !strings.Contains(evs[0].Message, "still present after 30 min") ||
		!strings.Contains(evs[0].Message, "1 child resources left") {
		t.Fatalf("%v", evs)
	}
}

func TestTeardownFromMachinery(t *testing.T) {
	r := &rs.ResourceStatus{Name: "app-dev", Namespace: "default", InfoFields: map[string]string{
		infoStage:             "platform",
		infoDeletionTimestamp: "2026-10-20T08:00:00Z",
		infoResourceRefs:      "default/app-dev-vm, app-dev-ns,default/app-dev-baseos",
	}, Conditions: []*rs.Condition{cond("Ready", "False", "Deleting", "")}}
	o := teardownFromMachinery(r)
	if !o.Found || !o.Deleting || !o.ChildrenKnown || o.Stage != "platform" ||
		strings.Join(o.Children, "|") != "app-dev-ns|default/app-dev-baseos|default/app-dev-vm" {
		t.Fatalf("%+v", o)
	}

	// Not mapped: children unknown; the Deleting reason alone says deleting.
	o = teardownFromMachinery(&rs.ResourceStatus{Conditions: []*rs.Condition{
		cond("Synced", "False", "ReconcileError", "cannot delete"), cond("Ready", "False", "Deleting", "")}})
	if !o.Deleting || o.ChildrenKnown || o.Children != nil || o.Message != "ReconcileError: cannot delete" {
		t.Fatalf("%+v", o)
	}
	if isDeleting(clusterStack("vm", "false", cond("Ready", "False", "Creating", ""))) {
		t.Fatal("a building stack is not deleting")
	}
}

// Through the activity's machinery path: NotFound is gone, not an error.
func TestObserveTeardownMachinery(t *testing.T) {
	t.Setenv("MACHINERY_AUTH_TOKEN", "")
	f := newFake()
	startMachinery(t, f, nil, nil)
	o, err := observeTeardownMachinery(context.Background(), machineryXR(bufServer, true))
	if err != nil || !o.Found {
		t.Fatalf("%+v %v", o, err)
	}
	gone := machineryXR(bufServer, true)
	gone.Name = "gone"
	if o, err := observeTeardownMachinery(context.Background(), gone); err != nil || o.Found {
		t.Fatalf("NotFound must be Found=false: %+v %v", o, err)
	}
}

func TestValidateTeardown(t *testing.T) {
	in := &TeardownInput{Target: XRTarget{Namespace: "default", Name: "app-dev"}}
	in.applyDefaults()
	if err := in.validate(); err == nil {
		t.Fatal("source kube must be rejected")
	}
	if err := testTeardownInput().validate(); err != nil {
		t.Fatal(err)
	}
}
