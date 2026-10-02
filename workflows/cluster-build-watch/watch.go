package main

import (
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// INPUT
// ─────────────────────────────────────────────────────────────────────────────

// Input is what a ClusterBuildWatch CR (or run.sh) posts to the sidecar.
//
// The workflow WATCHES, it does not drive: nothing here creates, changes or
// deletes anything on the cluster except the status ConfigMap the worker owns.
// Deleting a watch, or letting it fail, leaves the build exactly as it was.
type Input struct {
	// Name is what notifications call this build, e.g. the cluster name. Free
	// text: nothing is named after it.
	Name string `json:"name"`

	// GitOps is the checkpoint before the XR exists: has the GitOps tool
	// applied the commit that carries it? Optional -- without it the watch
	// starts at the XR.
	GitOps *GitOpsTarget `json:"gitops,omitempty"`

	// Target is the Crossplane XR whose status.stage is followed.
	Target XRTarget `json:"target"`

	// Argo is the checkpoint after the XR: once it is ready, wait until the
	// Argo CD Applications generated for the new cluster are Synced and
	// Healthy. Optional, and even when set it only runs for a stack that
	// registers the cluster with Argo CD (spec.rancher.argocd.register).
	Argo *ArgoTarget `json:"argo,omitempty"`

	PollSeconds int `json:"pollSeconds,omitempty"`
	// TimeoutMin bounds the whole watch. Past it the watch FAILS; a stage that
	// only overruns its own timeout is reported as stuck and watched on.
	TimeoutMin int `json:"timeoutMin,omitempty"`

	// State is carried across ContinueAsNew. Set by the workflow, never by a
	// caller -- a caller that sets it resumes a watch that never ran.
	State *WatchState `json:"state,omitempty"`
}

// GitOpsTarget names an Argo CD Application or a Flux Kustomization.
type GitOpsTarget struct {
	Kind      string `json:"kind,omitempty"` // argocd (default) | flux
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Revision is the commit that must be applied, full SHA or a prefix of it.
	// Without it, "Synced" can describe the commit BEFORE the merge: the
	// Application was in sync with the old tree and nothing has refreshed yet.
	// Set it whenever the caller knows the merge SHA.
	Revision string `json:"revision,omitempty"`

	// Where the object is read from, see ObservationSource.
	ObservationSource
}

// XRTarget addresses one composite resource.
type XRTarget struct {
	APIVersion string `json:"apiVersion,omitempty"` // default config.stuttgart-things.com/v1alpha1
	Resource   string `json:"resource,omitempty"`   // plural, default clusterstacks
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	// StageTimeoutMin overrides the per-stage timeout. Keys are stage values as
	// the XR writes them (vm, baseos, distribution, ...) plus the two stages
	// this workflow adds in front: gitops-sync and xr-pending.
	StageTimeoutMin map[string]int `json:"stageTimeoutMin,omitempty"`
	// DefaultStageTimeoutMin applies to every stage not named above.
	DefaultStageTimeoutMin int `json:"defaultStageTimeoutMin,omitempty"`

	// Where the XR is read from, see ObservationSource.
	ObservationSource
}

// ArgoTarget selects the Argo CD Applications of one cluster.
//
// Generated Applications carry no cluster label. The handle is the AppProject:
// the cluster-projects ApplicationSet creates one per registered cluster,
// named after it, and every Application generated for the cluster sits in it
// (spec.project). A few outer Applications sit in the `default` project
// instead and are named `<something>-<cluster>` (cert-manager-install-<c>,
// trust-manager-install-<c>, proj-<c>); IncludeDefaultProject picks those up.
//
// Read through machinery only: the worker runs on a different cluster than
// Argo CD and has no credentials there, by design.
type ArgoTarget struct {
	// Project is the AppProject, default the target's name (ClusterStack name
	// = cluster name = AppProject name).
	Project string `json:"project,omitempty"`
	// Namespace of the Applications, default argocd.
	Namespace string `json:"namespace,omitempty"`
	// IncludeDefaultProject also counts default-project Applications named
	// `*-<project>`, default true. When another project's name is a longer
	// suffix of the Application name (`proj-app-dev` for project `dev` vs
	// `app-dev`), it belongs to that project instead.
	IncludeDefaultProject *bool `json:"includeDefaultProject,omitempty"`
	// Always runs the checkpoint without looking at the stack's register flag
	// -- for an XR that has none. Default false: only a stack whose
	// ArgoRegister is true is waited for.
	Always bool `json:"always,omitempty"`
	// GraceMin is how long zero matching Applications is "not generated yet".
	// Past it the watch FAILS: a registered cluster with nothing in its project
	// means the wrong Argo CD, the wrong project, or a machinery that does not
	// serve Applications -- waiting longer cannot fix any of them. Default 15.
	GraceMin int `json:"graceMin,omitempty"`
	// SettlePolls is how many consecutive observations must find every
	// Application Synced + Healthy, with an unchanged count, before the watch
	// is done. Generation is staged (an app-of-apps creates its children only
	// once it has synced), so one all-green poll can come before the rest
	// exists. Default 2.
	SettlePolls int `json:"settlePolls,omitempty"`

	// Where the Applications are read from; machinery only, see above.
	ObservationSource
}

func (a *ArgoTarget) includeDefault() bool {
	return a.IncludeDefaultProject == nil || *a.IncludeDefaultProject
}

const (
	sourceKube      = "kube"
	sourceMachinery = "machinery"
)

// ObservationSource says where one side of the watch (gitops or target) is
// read from. Embedded, so the JSON keys sit next to name/namespace:
//
//	"target": {"namespace": "default", "name": "app-dev",
//	           "source": "machinery",
//	           "machinery": {"server": "machinery-grpc.example:443"}}
//
// There is no token here on purpose: the input lands in the workflow history
// and in the status ConfigMap. The worker reads MACHINERY_AUTH_TOKEN or
// MACHINERY_AUTH_TOKEN_FILE from its own environment (machinery.go).
type ObservationSource struct {
	// Source is "kube" (default: the worker GETs the object from the API
	// server it runs against) or "machinery" (a machinery ResourceService,
	// read-only gRPC, typically in front of another cluster).
	Source    string           `json:"source,omitempty"`
	Machinery *MachinerySource `json:"machinery,omitempty"`
}

// MachinerySource addresses a machinery gRPC endpoint
// (github.com/stuttgart-things/machinery, package resourceservice).
type MachinerySource struct {
	// Server is host:port, e.g. machinery-grpc.machinery.example.com:443.
	Server string `json:"server"`
	// Kind is the resource kind as configured on that server: default
	// ClusterStack for the target, Kustomization for gitops.
	Kind string `json:"kind,omitempty"`
	// Plaintext dials without TLS. For an in-cluster Service only; the auth
	// token is never sent over a plaintext connection.
	Plaintext bool `json:"plaintext,omitempty"`
}

// fromMachinery reports whether this side is read through machinery. An empty
// Source means kube. It is not written back as "kube", so an input from before
// the field existed -- and its workflow history -- stays byte-identical.
func (s ObservationSource) fromMachinery() bool { return s.Source == sourceMachinery }

func (s *ObservationSource) defaultKind(kind string) {
	if s.fromMachinery() && s.Machinery != nil && s.Machinery.Kind == "" {
		s.Machinery.Kind = kind
	}
}

func (s ObservationSource) validate(field string) error {
	switch s.Source {
	case "", sourceKube:
		// Most likely a forgotten `source: machinery`; reading the local API
		// server instead would watch the wrong cluster without a word.
		if s.Machinery != nil {
			return fmt.Errorf("%s.machinery is set but %s.source is %q: set %s.source to machinery to use it",
				field, field, orDash(s.Source), field)
		}
	case sourceMachinery:
		if s.Machinery == nil || s.Machinery.Server == "" {
			return fmt.Errorf("%s.machinery.server is required when %s.source is machinery", field, field)
		}
	default:
		return fmt.Errorf("%s.source %q: want kube or machinery", field, s.Source)
	}
	return nil
}

const (
	stageGitOpsSync = "gitops-sync" // waiting for Argo CD / Flux to apply the commit
	stageXRPending  = "xr-pending"  // synced, but the XR does not exist yet
	stageXRCreated  = "xr-created"  // XR exists, has written no status.stage yet
	stageArgoSync   = "argo-sync"   // XR ready, waiting for the cluster's Argo CD Applications
)

// Built-in stage timeouts, in minutes. They are generous on purpose: a stuck
// notification that fires on a healthy build teaches people to ignore it.
// Measured against the proxmox/k3s chain in crossplane-configurations
// bootstrap/cluster; vSphere and rke2 run longer and should override.
var defaultStageTimeouts = map[string]int{
	stageGitOpsSync: 15,
	stageXRPending:  10,
	stageXRCreated:  10,
	"vm":            30,
	"baseos":        30,
	"distribution":  30,
	"kubeconfig":    10,
	"access":        10,
	"platform":      45,
	// The rancher custom-node and machine-pool paths of xplane-cluster.
	"rancher": 30,
	"node-ip": 15,
	"join":    30,
	// Optional step after platform. status.ready does not wait for it, the
	// Ready condition does.
	"management-plane": 45,
	// The XR has written stage ready; what is left is Crossplane's Ready
	// condition catching up. Minutes, not tens of minutes.
	"ready": 10,
	// From XR ready to every generated Application Synced + Healthy. The
	// AppSets requeue every few minutes and the platform profiles install
	// charts (kube-prometheus-stack, openebs, kyverno) on a fresh cluster.
	stageArgoSync: 30,
}

func (in *Input) applyDefaults() {
	if in.PollSeconds <= 0 {
		in.PollSeconds = 30
	}
	if in.TimeoutMin <= 0 {
		in.TimeoutMin = 240
	}
	if in.Target.APIVersion == "" {
		in.Target.APIVersion = "config.stuttgart-things.com/v1alpha1"
	}
	if in.Target.Resource == "" {
		in.Target.Resource = "clusterstacks"
	}
	if in.Target.DefaultStageTimeoutMin <= 0 {
		in.Target.DefaultStageTimeoutMin = 30
	}
	if in.Name == "" {
		in.Name = in.Target.Name
	}
	in.Target.defaultKind("ClusterStack")
	if in.GitOps != nil {
		if in.GitOps.Kind == "" {
			in.GitOps.Kind = "argocd"
		}
		if in.GitOps.Namespace == "" {
			if in.GitOps.Kind == "flux" {
				in.GitOps.Namespace = "flux-system"
			} else {
				in.GitOps.Namespace = "argocd"
			}
		}
		in.GitOps.defaultKind("Kustomization")
	}
	if in.Argo != nil {
		if in.Argo.Project == "" {
			in.Argo.Project = in.Target.Name
		}
		if in.Argo.Namespace == "" {
			in.Argo.Namespace = "argocd"
		}
		if in.Argo.GraceMin <= 0 {
			in.Argo.GraceMin = 15
		}
		if in.Argo.SettlePolls <= 0 {
			in.Argo.SettlePolls = 2
		}
		in.Argo.defaultKind("Application")
	}
}

func (in *Input) validate() error {
	if in.Target.Namespace == "" || in.Target.Name == "" {
		return fmt.Errorf("target.namespace and target.name are required")
	}
	if !strings.Contains(in.Target.APIVersion, "/") {
		return fmt.Errorf("target.apiVersion %q must be group/version", in.Target.APIVersion)
	}
	if err := in.Target.ObservationSource.validate("target"); err != nil {
		return err
	}
	if in.GitOps != nil {
		if in.GitOps.Name == "" {
			return fmt.Errorf("gitops.name is required when gitops is set")
		}
		if in.GitOps.Kind != "argocd" && in.GitOps.Kind != "flux" {
			return fmt.Errorf("gitops.kind %q: want argocd or flux", in.GitOps.Kind)
		}
		// A short prefix would match the wrong commit sooner or later.
		if r := in.GitOps.Revision; r != "" && len(r) < 7 {
			return fmt.Errorf("gitops.revision %q: give at least 7 characters of the SHA", r)
		}
		if err := in.GitOps.ObservationSource.validate("gitops"); err != nil {
			return err
		}
		// The gitops Argo parser is not wired to machinery's Application
		// info fields (only the argo checkpoint after ready is).
		if in.GitOps.fromMachinery() && in.GitOps.Kind != "flux" {
			return fmt.Errorf("gitops.source machinery works with gitops.kind flux (Kustomization) only, not %s: read Argo CD Applications with source kube", in.GitOps.Kind)
		}
	}
	if in.Argo != nil {
		if err := in.Argo.ObservationSource.validate("argo"); err != nil {
			return err
		}
		// The worker has no access to the Argo CD cluster, and is not meant
		// to: reading "kube" would look for Applications on the worker's own
		// cluster and find none.
		if !in.Argo.fromMachinery() {
			return fmt.Errorf("argo.source must be machinery: Argo CD Applications are read through a machinery on the Argo CD cluster, never through the worker's API server")
		}
	}
	return nil
}

func (in *Input) stageTimeout(stage string) time.Duration {
	if m, ok := in.Target.StageTimeoutMin[stage]; ok && m > 0 {
		return time.Duration(m) * time.Minute
	}
	if m, ok := defaultStageTimeouts[stage]; ok {
		return time.Duration(m) * time.Minute
	}
	return time.Duration(in.Target.DefaultStageTimeoutMin) * time.Minute
}

// ─────────────────────────────────────────────────────────────────────────────
// OBSERVATIONS — what the activities report back
// ─────────────────────────────────────────────────────────────────────────────

type GitOpsObservation struct {
	Found    bool   `json:"found"`
	Synced   bool   `json:"synced"` // applied, at Revision if one was asked for
	Revision string `json:"revision,omitempty"`
	Health   string `json:"health,omitempty"`
	Degraded bool   `json:"degraded"` // a failed sync or Degraded health
	Message  string `json:"message,omitempty"`
}

type XRObservation struct {
	Found bool   `json:"found"`
	Stage string `json:"stage,omitempty"`
	// Ready is the XR's own status.ready, nil when the XRD has no such field.
	Ready *bool `json:"ready,omitempty"`
	// ReadyCondition is Crossplane's Ready condition. Both are required: a
	// ClusterStack can report status.ready true while Ready stays False (see
	// crossplane-configurations bootstrap/cluster README, "A stack can look
	// finished and not be").
	ReadyCondition bool   `json:"readyCondition"`
	SyncedFalse    bool   `json:"syncedFalse"` // Synced=False: a reconcile error
	Message        string `json:"message,omitempty"`
	// ArgoRegister is spec.rancher.argocd.register, nil when the XR has no
	// such field or machinery does not map it.
	ArgoRegister *bool `json:"argoRegister,omitempty"`
}

// ArgoObservation summarises the Applications of one cluster.
type ArgoObservation struct {
	Total int `json:"total"` // matching Applications
	Ready int `json:"ready"` // of those, Synced AND Healthy
	// Pending names the ones that are not, with their state, capped.
	Pending  []string `json:"pending,omitempty"`
	Degraded bool     `json:"degraded"` // Degraded health, a failed sync, or an *Error condition
	Message  string   `json:"message,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// STATE + EVENTS
// ─────────────────────────────────────────────────────────────────────────────

const (
	phaseWatching = "Watching"
	phaseReady    = "Ready"
	phaseFailed   = "Failed"
)

type StageRecord struct {
	Stage string    `json:"stage"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitempty"`
}

func (r StageRecord) Duration() time.Duration {
	if r.End.IsZero() {
		return 0
	}
	return r.End.Sub(r.Start)
}

type WatchState struct {
	Phase         string        `json:"phase"`
	StartedAt     time.Time     `json:"startedAt"`
	Stage         string        `json:"stage"`
	StageSince    time.Time     `json:"stageSince"`
	StuckNotified bool          `json:"stuckNotified"`
	XRSeen        bool          `json:"xrSeen"`
	Degraded      bool          `json:"degraded"`
	Message       string        `json:"message,omitempty"`
	Stages        []StageRecord `json:"stages"`
	// ArgoGreen counts consecutive all-green Argo observations; ArgoTotal is
	// the Application count of the last one. See ArgoTarget.SettlePolls.
	ArgoGreen int `json:"argoGreen,omitempty"`
	ArgoTotal int `json:"argoTotal,omitempty"`
	// Seq numbers the events so receivers can drop the duplicate an activity
	// retry may deliver.
	Seq int `json:"seq"`
}

const (
	sevInfo    = "info"
	sevSuccess = "success"
	sevWarning = "warning"
	sevError   = "error"
)

// Event is one checkpoint. Every event is reported to every sink.
type Event struct {
	Seq      int           `json:"seq"`
	Type     string        `json:"type"` // started|synced|stage|stuck|degraded|recovered|ready|failed
	Severity string        `json:"severity"`
	Name     string        `json:"name"`
	Stage    string        `json:"stage"`
	Message  string        `json:"message"`
	At       time.Time     `json:"at"`
	Elapsed  string        `json:"elapsed"`
	Stages   []StageRecord `json:"stages,omitempty"` // only on ready/failed
}

func newState(in *Input, now time.Time) *WatchState {
	first := stageXRPending
	if in.GitOps != nil {
		first = stageGitOpsSync
	}
	return &WatchState{
		Phase:      phaseWatching,
		StartedAt:  now,
		Stage:      first,
		StageSince: now,
		Stages:     []StageRecord{{Stage: first, Start: now}},
	}
}

func (st *WatchState) done() bool { return st.Phase != phaseWatching }

func (st *WatchState) emit(in *Input, now time.Time, typ, sev, msg string) Event {
	st.Seq++
	ev := Event{
		Seq:      st.Seq,
		Type:     typ,
		Severity: sev,
		Name:     in.Name,
		Stage:    st.Stage,
		Message:  msg,
		At:       now,
		Elapsed:  now.Sub(st.StartedAt).Round(time.Second).String(),
	}
	if st.done() {
		ev.Stages = append([]StageRecord(nil), st.Stages...)
	}
	return ev
}

// enter closes the current stage and opens the next one. Entering the stage
// the watch is already in is a no-op, so a caller need not check.
func (st *WatchState) enter(stage string, now time.Time) bool {
	if stage == st.Stage {
		return false
	}
	if n := len(st.Stages); n > 0 {
		st.Stages[n-1].End = now
	}
	st.Stage = stage
	st.StageSince = now
	st.StuckNotified = false
	st.Stages = append(st.Stages, StageRecord{Stage: stage, Start: now})
	return true
}

func (st *WatchState) finish(phase string, now time.Time) {
	if n := len(st.Stages); n > 0 && st.Stages[n-1].End.IsZero() {
		st.Stages[n-1].End = now
	}
	st.Phase = phase
}

// ─────────────────────────────────────────────────────────────────────────────
// ADVANCE — the whole checkpoint logic, free of Dapr and HTTP
// ─────────────────────────────────────────────────────────────────────────────

// Advance folds one observation into the state and returns the checkpoints it
// crossed. Exactly one of gitops / xr is non-nil when the observation
// succeeded; both nil means it failed, and only the clocks are checked.
//
// It must stay deterministic: the workflow calls it during replay, so it may
// read time only through `now`, which is the orchestration clock.
func Advance(in *Input, st *WatchState, gitops *GitOpsObservation, xr *XRObservation, now time.Time) []Event {
	if st.done() {
		return nil
	}
	var evs []Event

	switch {
	case gitops != nil && st.Stage == stageGitOpsSync:
		evs = append(evs, st.degradation(in, now, gitops.Degraded, gitops.Message)...)
		switch {
		case !gitops.Found:
			st.Message = fmt.Sprintf("%s %s/%s not found", in.GitOps.Kind, in.GitOps.Namespace, in.GitOps.Name)
		case gitops.Synced:
			st.enter(stageXRPending, now)
			st.Message = fmt.Sprintf("applied revision %s", shortRev(gitops.Revision))
			evs = append(evs, st.emit(in, now, "synced", sevInfo,
				fmt.Sprintf("%s %s applied revision %s (health: %s)", in.GitOps.Kind, in.GitOps.Name, shortRev(gitops.Revision), orDash(gitops.Health))))
		default:
			st.Message = fmt.Sprintf("waiting for %s, at %s (health: %s)", wantRev(in.GitOps.Revision), shortRev(gitops.Revision), orDash(gitops.Health))
		}

	case xr != nil && st.Stage != stageGitOpsSync && st.Stage != stageArgoSync:
		if !xr.Found {
			if st.XRSeen {
				st.Message = "XR was deleted while the build was being watched"
				st.finish(phaseFailed, now)
				return append(evs, st.emit(in, now, "failed", sevError, st.Message))
			}
			st.Message = fmt.Sprintf("waiting for %s %s/%s to exist", in.Target.Resource, in.Target.Namespace, in.Target.Name)
			break
		}
		st.XRSeen = true
		evs = append(evs, st.degradation(in, now, xr.SyncedFalse, xr.Message)...)

		if xr.ReadyCondition && (xr.Ready == nil || *xr.Ready) {
			wait, why := in.argoWanted(xr)
			if wait {
				// Not done yet: the watch moves on to the Applications, and
				// the clocks below start the argo-sync stage timer.
				prev := st.Stage
				st.enter(stageArgoSync, now)
				st.Message = fmt.Sprintf("XR ready, waiting for the Argo CD Applications of project %s", in.Argo.Project)
				evs = append(evs, st.emit(in, now, "stage", sevInfo,
					stageMessage(prev, stageArgoSync, st)+": "+st.Message))
				break
			}
			st.Message = "build complete"
			msg := fmt.Sprintf("%s is ready after %s", in.Name, now.Sub(st.StartedAt).Round(time.Second))
			if why != "" {
				st.Message += " (" + why + ")"
				msg += " (" + why + ")"
			}
			st.finish(phaseReady, now)
			return append(evs, st.emit(in, now, "ready", sevSuccess, msg))
		}

		stage := xr.Stage
		if stage == "" {
			stage = stageXRCreated
		}
		prev := st.Stage
		if st.enter(stage, now) {
			st.Message = fmt.Sprintf("stage %s", stage)
			evs = append(evs, st.emit(in, now, "stage", sevInfo, stageMessage(prev, stage, st)))
		}
	}

	return append(evs, st.clocks(in, now)...)
}

// argoWanted says whether the Argo checkpoint runs for this XR, and when it
// does not although one was asked for, why -- for the ready message.
func (in *Input) argoWanted(xr *XRObservation) (bool, string) {
	switch {
	case in.Argo == nil:
		return false, ""
	case in.Argo.Always:
		return true, ""
	case xr.ArgoRegister == nil:
		return false, "argo checkpoint skipped: the XR's argocd.register is unknown -- map spec.rancher.argocd.register as ArgoRegister on machinery, or set argo.always"
	case !*xr.ArgoRegister:
		return false, "not registered with Argo CD, argo checkpoint skipped"
	}
	return true, ""
}

// AdvanceArgo is Advance for the argo-sync stage: it folds one observation of
// the cluster's Applications into the state. argo nil means the observation
// failed; only the clocks are checked. Deterministic, like Advance.
func AdvanceArgo(in *Input, st *WatchState, argo *ArgoObservation, now time.Time) []Event {
	if st.done() || st.Stage != stageArgoSync || in.Argo == nil {
		return nil
	}
	var evs []Event
	if argo != nil {
		evs = append(evs, st.degradation(in, now, argo.Degraded, argo.Message)...)
		allGreen := argo.Total > 0 && argo.Ready == argo.Total
		switch {
		case argo.Total == 0:
			st.ArgoGreen = 0
			grace := time.Duration(in.Argo.GraceMin) * time.Minute
			if now.Sub(st.StageSince) >= grace {
				st.Message = fmt.Sprintf("no Argo CD Application in project %s (namespace %s) after %d min: wrong Argo CD, wrong project, or %s does not serve Applications",
					in.Argo.Project, in.Argo.Namespace, in.Argo.GraceMin, in.Argo.Machinery.Server)
				st.finish(phaseFailed, now)
				return append(evs, st.emit(in, now, "failed", sevError, st.Message))
			}
			st.Message = fmt.Sprintf("waiting for Applications in project %s to be generated", in.Argo.Project)
		case allGreen:
			if argo.Total == st.ArgoTotal {
				st.ArgoGreen++
			} else {
				st.ArgoGreen = 1
			}
			if st.ArgoGreen >= in.Argo.SettlePolls {
				st.ArgoTotal = argo.Total
				st.Message = fmt.Sprintf("build complete, %d Argo CD Applications Synced + Healthy", argo.Total)
				st.finish(phaseReady, now)
				return append(evs, st.emit(in, now, "ready", sevSuccess,
					fmt.Sprintf("%s is ready after %s, %d Argo CD Applications in project %s Synced + Healthy",
						in.Name, now.Sub(st.StartedAt).Round(time.Second), argo.Total, in.Argo.Project)))
			}
			st.Message = fmt.Sprintf("all %d Applications Synced + Healthy, confirming (%d/%d)", argo.Total, st.ArgoGreen, in.Argo.SettlePolls)
		default:
			st.ArgoGreen = 0
			st.Message = fmt.Sprintf("%d/%d Applications Synced + Healthy, waiting for %s", argo.Ready, argo.Total, strings.Join(argo.Pending, ", "))
		}
		st.ArgoTotal = argo.Total
	}
	return append(evs, st.clocks(in, now)...)
}

// clocks ends a watch past its overall timeout and reports a stage past its
// own once. They run whether or not this round observed anything: a watch
// whose every observation fails must still end.
func (st *WatchState) clocks(in *Input, now time.Time) []Event {
	var evs []Event
	if now.Sub(st.StartedAt) >= time.Duration(in.TimeoutMin)*time.Minute {
		st.Message = fmt.Sprintf("no result after %d min, last stage %s", in.TimeoutMin, st.Stage)
		st.finish(phaseFailed, now)
		return append(evs, st.emit(in, now, "failed", sevError, st.Message))
	}
	if limit := in.stageTimeout(st.Stage); !st.StuckNotified && now.Sub(st.StageSince) >= limit {
		st.StuckNotified = true
		evs = append(evs, st.emit(in, now, "stuck", sevWarning,
			fmt.Sprintf("stage %s has run %s (limit %s) — %s", st.Stage, now.Sub(st.StageSince).Round(time.Second), limit, orDash(st.Message))))
	}
	return evs
}

// degradation reports a reconcile error once when it appears and once when it
// clears, not on every poll it persists.
func (st *WatchState) degradation(in *Input, now time.Time, degraded bool, msg string) []Event {
	switch {
	case degraded && !st.Degraded:
		st.Degraded = true
		return []Event{st.emit(in, now, "degraded", sevWarning, orDash(msg))}
	case !degraded && st.Degraded:
		st.Degraded = false
		return []Event{st.emit(in, now, "recovered", sevInfo, "reconciling again")}
	}
	return nil
}

func stageMessage(prev, next string, st *WatchState) string {
	n := len(st.Stages)
	if n < 2 {
		return fmt.Sprintf("entered stage %s", next)
	}
	return fmt.Sprintf("%s → %s (%s took %s)", prev, next, prev, st.Stages[n-2].Duration().Round(time.Second))
}

func shortRev(r string) string {
	// Flux writes "main@sha1:<sha>"; keep the ref, shorten the SHA.
	if i := strings.LastIndex(r, ":"); i >= 0 && len(r)-i-1 > 12 {
		return r[:i+1] + r[i+1:i+13]
	}
	if len(r) > 12 && !strings.Contains(r, "@") {
		return r[:12]
	}
	return orDash(r)
}

func wantRev(r string) string {
	if r == "" {
		return "sync"
	}
	return "revision " + shortRev(r)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
