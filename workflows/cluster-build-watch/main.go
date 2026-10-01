package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	dapr "github.com/dapr/go-sdk/client"
)

// ─────────────────────────────────────────────────────────────────────────────
// WORKFLOW
// ─────────────────────────────────────────────────────────────────────────────

// roundsPerGeneration bounds the history of one generation. A watch polls for
// hours; every round adds a timer and an activity to the history, and Dapr
// replays the whole history on every step. ContinueAsNew starts a fresh one
// with the state carried in the input, under the same instance ID.
const roundsPerGeneration = 60

var observeRetry = &workflow.RetryPolicy{
	MaxAttempts:          3,
	InitialRetryInterval: 5 * time.Second,
	BackoffCoefficient:   2,
	MaxRetryInterval:     30 * time.Second,
}

// ClusterBuildWatchWorkflow follows one build from the GitOps sync to the XR's
// Ready, and reports every checkpoint it crosses. See watch.go for the
// checkpoint logic itself; this function only runs the loop.
func ClusterBuildWatchWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var in Input
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	in.applyDefaults()
	if err := in.validate(); err != nil {
		return nil, err
	}

	now := ctx.CurrentTimeUTC()
	st := in.State
	if st == nil {
		st = newState(&in, now)
		start := fmt.Sprintf("watching %s %s/%s", in.Target.Resource, in.Target.Namespace, in.Target.Name)
		if in.GitOps != nil {
			start += fmt.Sprintf(", after %s %s/%s applies %s", in.GitOps.Kind, in.GitOps.Namespace, in.GitOps.Name, wantRev(in.GitOps.Revision))
		}
		report(ctx, &in, st, st.emit(&in, now, "started", sevInfo, start))
	}

	for range roundsPerGeneration {
		var gitops *GitOpsObservation
		var xr *XRObservation
		var obsErr error
		if st.Stage == stageGitOpsSync {
			var o GitOpsObservation
			obsErr = ctx.CallActivity(ObserveGitOps, workflow.WithActivityInput(in.GitOps),
				workflow.WithActivityRetryPolicy(observeRetry)).Await(&o)
			if obsErr == nil {
				gitops = &o
			}
		} else {
			var o XRObservation
			obsErr = ctx.CallActivity(ObserveXR, workflow.WithActivityInput(in.Target),
				workflow.WithActivityRetryPolicy(observeRetry)).Await(&o)
			if obsErr == nil {
				xr = &o
			}
		}

		now = ctx.CurrentTimeUTC()
		for _, ev := range Advance(&in, st, gitops, xr, now) {
			report(ctx, &in, st, ev)
		}

		status := fmt.Sprintf("[%s] %s: %s", st.Phase, st.Stage, st.Message)
		if obsErr != nil {
			// Not fatal: the clocks in Advance still end a watch that can
			// never observe anything. The error is what tells you why.
			status += fmt.Sprintf(" (observe failed: %v)", obsErr)
		}
		ctx.SetCustomStatus(truncate(status, 1000))

		switch st.Phase {
		case phaseReady:
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

// report hands one event to the Report activity. A failure is logged into the
// custom status and otherwise ignored: a lost Teams message is no reason to
// stop watching a build.
func report(ctx *workflow.WorkflowContext, in *Input, st *WatchState, ev Event) {
	ri := ReportInput{
		InstanceID: ctx.ID(),
		Target:     in.Target,
		Event:      ev,
		State:      *st,
	}
	err := ctx.CallActivity(Report, workflow.WithActivityInput(ri),
		workflow.WithActivityRetryPolicy(&workflow.RetryPolicy{
			MaxAttempts:          3,
			InitialRetryInterval: 10 * time.Second,
			BackoffCoefficient:   2,
		})).Await(nil)
	if err != nil {
		ctx.SetCustomStatus(truncate(fmt.Sprintf("report %s #%d failed: %v", ev.Type, ev.Seq, err), 1000))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ACTIVITIES — observation
// ─────────────────────────────────────────────────────────────────────────────

var gitOpsKinds = map[string]struct{ apiVersion, resource string }{
	"argocd": {"argoproj.io/v1alpha1", "applications"},
	"flux":   {"kustomize.toolkit.fluxcd.io/v1", "kustomizations"},
}

// ObserveGitOps reads the Argo CD Application or Flux Kustomization, from the
// local API server (source kube) or a machinery endpoint (source machinery).
func ObserveGitOps(ctx workflow.ActivityContext) (any, error) {
	var t GitOpsTarget
	if err := ctx.GetInput(&t); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	var o GitOpsObservation
	var err error
	if t.fromMachinery() {
		o, err = observeGitOpsMachinery(activityContext(ctx), t)
	} else {
		o, err = observeGitOpsKube(t)
	}
	if err != nil {
		return nil, err
	}
	slog.Info("gitops", "source", orDash(t.Source), "kind", t.Kind, "name", t.Name,
		"found", o.Found, "synced", o.Synced, "revision", o.Revision, "health", o.Health)
	return &o, nil
}

func observeGitOpsKube(t GitOpsTarget) (GitOpsObservation, error) {
	kind, ok := gitOpsKinds[t.Kind]
	if !ok {
		return GitOpsObservation{}, fmt.Errorf("unknown gitops kind %q", t.Kind)
	}
	k, err := newKube()
	if err != nil {
		return GitOpsObservation{}, err
	}
	obj, err := k.get(resourcePath(kind.apiVersion, kind.resource, t.Namespace, t.Name))
	if err != nil || obj == nil {
		return GitOpsObservation{}, err
	}
	if t.Kind == "flux" {
		return parseFluxKustomization(obj, t.Revision), nil
	}
	return parseArgoApplication(obj, t.Revision), nil
}

// ObserveXR reads the XR, from the local API server (source kube) or a
// machinery endpoint (source machinery).
func ObserveXR(ctx workflow.ActivityContext) (any, error) {
	var t XRTarget
	if err := ctx.GetInput(&t); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	var o XRObservation
	var err error
	if t.fromMachinery() {
		o, err = observeXRMachinery(activityContext(ctx), t)
	} else {
		o, err = observeXRKube(t)
	}
	if err != nil {
		return nil, err
	}
	slog.Info("xr", "source", orDash(t.Source), "resource", t.Resource, "name", t.Name,
		"found", o.Found, "stage", o.Stage, "readyCondition", o.ReadyCondition)
	return &o, nil
}

func observeXRKube(t XRTarget) (XRObservation, error) {
	k, err := newKube()
	if err != nil {
		return XRObservation{}, err
	}
	obj, err := k.get(resourcePath(t.APIVersion, t.Resource, t.Namespace, t.Name))
	if err != nil || obj == nil {
		return XRObservation{}, err
	}
	return parseXR(obj), nil
}

// activityContext is the activity's context, cancelled when the worker stops.
func activityContext(ctx workflow.ActivityContext) context.Context {
	if c := ctx.Context(); c != nil {
		return c
	}
	return context.Background()
}

// ─────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	r := workflow.NewRegistry()
	if err := r.AddWorkflowN("ClusterBuildWatchWorkflow", ClusterBuildWatchWorkflow); err != nil {
		log.Fatalf("register workflow: %v", err)
	}
	for name, fn := range map[string]workflow.Activity{
		"ObserveGitOps": ObserveGitOps,
		"ObserveXR":     ObserveXR,
		"Report":        Report,
	} {
		if err := r.AddActivityN(name, fn); err != nil {
			log.Fatalf("register %s: %v", name, err)
		}
	}

	wfClient, err := dapr.NewWorkflowClient()
	if err != nil {
		log.Fatalf("workflow client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := wfClient.StartWorker(ctx, r); err != nil {
			log.Fatalf("worker: %v", err)
		}
	}()

	time.Sleep(2 * time.Second)
	slog.Info("worker ready — use run.sh to start a watch")

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-sigCtx.Done()
	slog.Info("shutting down")
}
