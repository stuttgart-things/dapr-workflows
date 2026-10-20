package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	rs "github.com/stuttgart-things/machinery/resourceservice"
)

// ─────────────────────────────────────────────────────────────────────────────
// TEARDOWN — the other direction: an XR being deleted, until it is gone
// ─────────────────────────────────────────────────────────────────────────────
//
// Started by discovery (discovery.go) for a ClusterStack with a
// deletionTimestamp, under the instance ID teardown-<uid>. Like the build
// watch it only watches: it never deletes, retries or unblocks anything.
//
//	started ──► progress (a child XR is gone, n left) ... ──► deleted
//	                                                      └─► failed (timeoutMin)
//
// Read through machinery only, like the Argo checkpoint: discovery is
// machinery-only, and the explicit ClusterBuildWatch does not start
// teardowns.

const (
	stageTeardown = "teardown" // deletionTimestamp set, XR still there
	phaseDeleted  = "Deleted"
)

// Info field labels for a teardown, see README "Discovery". Both optional:
// without DeletionTimestamp the Ready condition's reason Deleting is the
// signal, without ResourceRefs the watch reports start and end only.
const (
	infoDeletionTimestamp = "DeletionTimestamp" // metadata.deletionTimestamp
	infoResourceRefs      = "ResourceRefs"      // spec.crossplane.resourceRefs, "ns/name, name, ..."
)

// TeardownInput is what discovery posts for one deleting XR.
type TeardownInput struct {
	// Name is what notifications call it, the stack name.
	Name string `json:"name"`
	// Target is the XR being deleted; source must be machinery.
	Target      XRTarget `json:"target"`
	PollSeconds int      `json:"pollSeconds,omitempty"`
	// TimeoutMin bounds the teardown. Past it the watch FAILS with what is
	// still left; the deletion itself goes on untouched.
	TimeoutMin int `json:"timeoutMin,omitempty"`

	// State is carried across ContinueAsNew, as in Input.
	State *WatchState `json:"state,omitempty"`
}

func (in *TeardownInput) applyDefaults() {
	if in.PollSeconds <= 0 {
		in.PollSeconds = 30
	}
	if in.TimeoutMin <= 0 {
		in.TimeoutMin = 60
	}
	if in.Target.APIVersion == "" {
		in.Target.APIVersion = "config.stuttgart-things.com/v1alpha1"
	}
	if in.Target.Resource == "" {
		in.Target.Resource = "clusterstacks"
	}
	if in.Name == "" {
		in.Name = in.Target.Name
	}
	in.Target.defaultKind("ClusterStack")
}

func (in *TeardownInput) validate() error {
	if in.Target.Namespace == "" || in.Target.Name == "" {
		return fmt.Errorf("target.namespace and target.name are required")
	}
	if err := in.Target.validate("target"); err != nil {
		return err
	}
	if !in.Target.fromMachinery() {
		return fmt.Errorf("target.source must be machinery: a teardown is read through machinery only")
	}
	return nil
}

// asInput is the view emit and report need: the display name and the target.
func (in *TeardownInput) asInput() *Input {
	return &Input{Name: in.Name, Target: in.Target, PollSeconds: in.PollSeconds, TimeoutMin: in.TimeoutMin}
}

// TeardownObservation is one look at the deleting XR.
type TeardownObservation struct {
	Found    bool   `json:"found"`
	Deleting bool   `json:"deleting"`
	Stage    string `json:"stage,omitempty"`
	// Children are the XR's composed resources, nil when machinery does not
	// map ResourceRefs -- an empty, non-nil list means "none left".
	Children []string `json:"children,omitempty"`
	// ChildrenKnown tells nil-because-unmapped from nil-because-none, which
	// omitempty would otherwise fold together in the workflow history.
	ChildrenKnown bool   `json:"childrenKnown"`
	Message       string `json:"message,omitempty"`
}

// ClusterTeardownWatchWorkflow follows one XR deletion until the XR is gone.
// Same loop shape as ClusterBuildWatchWorkflow; the logic is AdvanceTeardown.
func ClusterTeardownWatchWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var in TeardownInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	in.applyDefaults()
	if err := in.validate(); err != nil {
		return nil, err
	}
	view := in.asInput()

	st := in.State
	if st == nil {
		st = newTeardownState(ctx.CurrentTimeUTC())
	}

	for range roundsPerGeneration {
		var o TeardownObservation
		var obs *TeardownObservation
		obsErr := ctx.CallActivity(ObserveTeardown, workflow.WithActivityInput(in.Target),
			workflow.WithActivityRetryPolicy(observeRetry)).Await(&o)
		if obsErr == nil {
			obs = &o
		}

		for _, ev := range AdvanceTeardown(&in, st, obs, ctx.CurrentTimeUTC()) {
			report(ctx, view, st, ev)
		}

		status := fmt.Sprintf("[%s] %s: %s", st.Phase, st.Stage, st.Message)
		if obsErr != nil {
			status += fmt.Sprintf(" (observe failed: %v)", obsErr)
		}
		ctx.SetCustomStatus(truncate(status, 1000))

		switch st.Phase {
		case phaseDeleted:
			return st, nil
		case phaseFailed:
			return st, fmt.Errorf("%s: %s", in.Name, st.Message)
		}

		if err := ctx.CreateTimer(time.Duration(in.PollSeconds) * time.Second).Await(nil); err != nil {
			return nil, err
		}
	}

	in.State = st
	ctx.ContinueAsNew(in)
	return nil, nil
}

func newTeardownState(now time.Time) *WatchState {
	return &WatchState{
		Phase:      phaseWatching,
		StartedAt:  now,
		Stage:      stageTeardown,
		StageSince: now,
		Stages:     []StageRecord{{Stage: stageTeardown, Start: now}},
	}
}

// AdvanceTeardown folds one observation into the state and returns the
// events it crossed. obs nil means the observation failed; only the clock
// runs. Deterministic, like Advance.
//
// The first successful observation sends "started" -- or, when the XR is
// already gone, a single "deleted" instead of two cards in a row.
func AdvanceTeardown(in *TeardownInput, st *WatchState, obs *TeardownObservation, now time.Time) []Event {
	if st.done() {
		return nil
	}
	view := in.asInput()
	var evs []Event
	first := st.Seq == 0

	if obs != nil {
		// machinery leaves an empty list out (splitRefs): children seen
		// before and no field now means the last one is gone.
		if !obs.ChildrenKnown && len(st.Children) > 0 {
			o := *obs
			o.ChildrenKnown, o.Children = true, []string{}
			obs = &o
		}
		switch {
		case !obs.Found:
			st.Children = nil
			st.Message = "XR is gone"
			msg := fmt.Sprintf("%s is gone after %s", in.Name, now.Sub(st.StartedAt).Round(time.Second))
			if first {
				msg = fmt.Sprintf("%s was deleted (already gone when the teardown watch first looked)", in.Name)
			}
			st.finish(phaseDeleted, now)
			return append(evs, st.emit(view, now, "deleted", sevSuccess, msg))

		case first:
			st.Message = teardownMessage(obs)
			start := fmt.Sprintf("teardown of %s %s/%s started", in.Target.Resource, in.Target.Namespace, in.Target.Name)
			if obs.ChildrenKnown {
				start += fmt.Sprintf(", %d child resources: %s", len(obs.Children), childList(obs.Children))
			}
			st.Children = obs.Children
			evs = append(evs, st.emit(view, now, "started", sevInfo, start))

		case obs.ChildrenKnown && len(obs.Children) != len(st.Children):
			gone := missing(st.Children, obs.Children)
			st.Children = obs.Children
			st.Message = teardownMessage(obs)
			msg := fmt.Sprintf("%d child resources left", len(obs.Children))
			if len(gone) > 0 {
				msg += ", gone: " + childList(gone)
			}
			if len(obs.Children) > 0 {
				msg += "; left: " + childList(obs.Children)
			}
			evs = append(evs, st.emit(view, now, "progress", sevInfo, msg))

		default:
			st.Message = teardownMessage(obs)
		}
	} else if first {
		// Nothing observed yet; say so once a round later at the latest,
		// rather than nothing until the timeout.
		st.Message = "waiting for the first observation"
	}

	if now.Sub(st.StartedAt) >= time.Duration(in.TimeoutMin)*time.Minute {
		st.Message = fmt.Sprintf("still present after %d min: %s", in.TimeoutMin, orDash(st.Message))
		st.finish(phaseFailed, now)
		evs = append(evs, st.emit(view, now, "failed", sevError, st.Message))
	}
	return evs
}

func teardownMessage(o *TeardownObservation) string {
	var parts []string
	if o.ChildrenKnown {
		parts = append(parts, fmt.Sprintf("%d child resources left", len(o.Children)))
	}
	if o.Stage != "" {
		parts = append(parts, "stage "+o.Stage)
	}
	if o.Message != "" {
		parts = append(parts, o.Message)
	}
	if len(parts) == 0 {
		return "deleting"
	}
	return truncate(strings.Join(parts, ", "), 500)
}

// childList caps the names like ArgoObservation.Pending does.
func childList(c []string) string {
	if len(c) == 0 {
		return "-"
	}
	if len(c) > maxPending {
		return strings.Join(c[:maxPending], ", ") + fmt.Sprintf(", +%d more", len(c)-maxPending)
	}
	return strings.Join(c, ", ")
}

// missing returns the entries of before that are not in after, sorted.
func missing(before, after []string) []string {
	have := map[string]bool{}
	for _, a := range after {
		have[a] = true
	}
	var out []string
	for _, b := range before {
		if !have[b] {
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// ACTIVITY
// ─────────────────────────────────────────────────────────────────────────────

// ObserveTeardown reads the deleting XR through machinery.
func ObserveTeardown(ctx workflow.ActivityContext) (any, error) {
	var t XRTarget
	if err := ctx.GetInput(&t); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	if !t.fromMachinery() {
		return nil, fmt.Errorf("teardown source %q: only machinery is supported", t.Source)
	}
	o, err := observeTeardownMachinery(activityContext(ctx), t)
	if err != nil {
		return nil, err
	}
	slog.Info("teardown", "resource", t.Resource, "namespace", t.Namespace, "name", t.Name,
		"found", o.Found, "deleting", o.Deleting, "children", len(o.Children), "childrenKnown", o.ChildrenKnown)
	return &o, nil
}

func observeTeardownMachinery(ctx context.Context, t XRTarget) (TeardownObservation, error) {
	c, err := newMachineryClient(t.Machinery)
	if err != nil {
		return TeardownObservation{}, err
	}
	r, err := c.detail(ctx, t.Machinery.Kind, t.Namespace, t.Name)
	if err != nil || r == nil {
		return TeardownObservation{}, err
	}
	return teardownFromMachinery(r), nil
}

// teardownFromMachinery judges one ResourceStatus of a deleting XR. Pure.
func teardownFromMachinery(r *rs.ResourceStatus) TeardownObservation {
	info := r.GetInfoFields()
	o := TeardownObservation{Found: true, Deleting: isDeleting(r), Stage: info[infoStage]}
	if v, ok := info[infoResourceRefs]; ok {
		o.ChildrenKnown = true
		o.Children = splitRefs(v)
	}
	for _, c := range r.GetConditions() {
		if c.GetType() == "Ready" && c.GetStatus() != "True" && c.GetMessage() != "" {
			o.Message = truncate(c.GetReason()+": "+c.GetMessage(), 300)
		}
		if c.GetType() == "Synced" && c.GetStatus() == "False" {
			o.Message = truncate(c.GetReason()+": "+c.GetMessage(), 300)
			break
		}
	}
	return o
}

// splitRefs reads machinery's rendering of a list of object references:
// "ns/name, name, ...". machinery leaves an EMPTY list out altogether, so the
// last child going away shows up as the info field disappearing; the XR
// itself goes shortly after, and that is the event that counts.
func splitRefs(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// isDeleting: the DeletionTimestamp info field when machinery maps it, else
// Crossplane's Ready condition with reason Deleting, which the composite
// reconciler sets as soon as it sees the deletionTimestamp.
func isDeleting(r *rs.ResourceStatus) bool {
	if r.GetInfoFields()[infoDeletionTimestamp] != "" {
		return true
	}
	for _, c := range r.GetConditions() {
		if c.GetType() == "Ready" && c.GetReason() == "Deleting" {
			return true
		}
	}
	return false
}
