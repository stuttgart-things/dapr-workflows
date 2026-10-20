package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/api"
	"github.com/dapr/durabletask-go/workflow"
	rs "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// DISCOVERY — a watch for every ClusterStack, without anyone creating one
// ─────────────────────────────────────────────────────────────────────────────
//
// stuttgart-things/stuttgart-things#3831. The worker lists the ClusterStacks
// on machinery every interval, over the same ResourceService the watches read
// through, and starts
//
//   - a build watch (ClusterBuildWatchWorkflow, instance build-<uid>) for a
//     stack that is not Ready, not deleting, and new: created after the
//     worker started or within the look-back window;
//   - a teardown watch (ClusterTeardownWatchWorkflow, instance
//     teardown-<uid>) for a stack that is being deleted, whatever its age.
//
// The instance ID is the whole bookkeeping: Dapr already knows which watches
// exist, so a restarted worker asks it instead of remembering, and a rebuilt
// cluster with the same name gets a new uid and therefore a new watch. The
// explicit ClusterBuildWatch (kro) is untouched; its instances are named
// <namespace>_<name> and never collide with these.
//
// Off by default. Environment (never the workflow input):
//
//	CLUSTER_BUILD_WATCH_DISCOVERY              on|true|1 switches it on (default off)
//	CLUSTER_BUILD_WATCH_DISCOVERY_SERVER       machinery host:port (required when on)
//	CLUSTER_BUILD_WATCH_DISCOVERY_PLAINTEXT    true: dial without TLS (in-cluster only)
//	CLUSTER_BUILD_WATCH_DISCOVERY_KIND         machinery kind, default ClusterStack
//	CLUSTER_BUILD_WATCH_DISCOVERY_INTERVAL     list interval, default 60s
//	CLUSTER_BUILD_WATCH_DISCOVERY_LOOKBACK     how old a stack may be at worker start, default 30m
//	CLUSTER_BUILD_WATCH_DISCOVERY_GITOPS       <namespace>/<name> of the Flux Kustomization the
//	                                           build watch waits for first, default
//	                                           flux-system/machinery-xrs; set but empty: no gitops phase
//	CLUSTER_BUILD_WATCH_DISCOVERY_ARGO_SERVER  machinery on the Argo CD cluster; set: argo checkpoint
//	CLUSTER_BUILD_WATCH_DISCOVERY_TIMEOUT_MIN  build watch timeoutMin, default 240
//	CLUSTER_BUILD_WATCH_TEARDOWN_TIMEOUT_MIN   teardown watch timeoutMin, default 60
//	CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE      comma-separated <namespace>/<name> globs never watched
//
// MACHINERY_AUTH_TOKEN(_FILE) and SSL_CERT_FILE apply as for every machinery call.

const (
	buildWorkflowName    = "ClusterBuildWatchWorkflow"
	teardownWorkflowName = "ClusterTeardownWatchWorkflow"

	// optOutAnnotation on a ClusterStack keeps discovery away from it. See
	// infoWatch for how it reaches the worker.
	optOutAnnotation = "cluster-build-watch.stuttgart-things.com/watch"
)

// Info field labels discovery reads, besides Stage/StatusReady (machinery.go)
// and DeletionTimestamp/ResourceRefs (teardown.go).
const (
	// infoUID is metadata.uid. REQUIRED: without it there is no idempotent
	// instance ID, and the stack is skipped.
	infoUID = "UID"
	// infoWatch carries the opt-out annotation. machinery's info field paths
	// are split on every ".", so an annotation key with dots in it cannot be
	// mapped today (machinery v1.14.1 getNestedField). The worker honours the
	// label as soon as a machinery can deliver it; until then
	// CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE is the working opt-out.
	infoWatch = "Watch"
)

// DiscoveryConfig is the parsed environment.
type DiscoveryConfig struct {
	Enabled         bool
	Machinery       MachinerySource
	Interval        time.Duration
	Lookback        time.Duration
	GitOps          *GitOpsTarget // nil: no gitops phase
	ArgoServer      string
	BuildTimeoutMin int
	TeardownTimeout int
	Exclude         []string
}

func discoveryConfigFromEnv() (DiscoveryConfig, error) {
	c := DiscoveryConfig{
		Interval:        60 * time.Second,
		Lookback:        30 * time.Minute,
		TeardownTimeout: 60,
	}
	on := strings.ToLower(strings.TrimSpace(os.Getenv("CLUSTER_BUILD_WATCH_DISCOVERY")))
	switch on {
	case "", "off", "false", "0":
		return c, nil
	case "on", "true", "1":
		c.Enabled = true
	default:
		return c, fmt.Errorf("CLUSTER_BUILD_WATCH_DISCOVERY=%q: want on or off", on)
	}

	c.Machinery.Server = strings.TrimSpace(os.Getenv("CLUSTER_BUILD_WATCH_DISCOVERY_SERVER"))
	if c.Machinery.Server == "" {
		return c, fmt.Errorf("CLUSTER_BUILD_WATCH_DISCOVERY_SERVER is required when discovery is on")
	}
	c.Machinery.Kind = envOr("CLUSTER_BUILD_WATCH_DISCOVERY_KIND", "ClusterStack")
	var err error
	if v := os.Getenv("CLUSTER_BUILD_WATCH_DISCOVERY_PLAINTEXT"); v != "" {
		if c.Machinery.Plaintext, err = strconv.ParseBool(v); err != nil {
			return c, fmt.Errorf("CLUSTER_BUILD_WATCH_DISCOVERY_PLAINTEXT=%q: %w", v, err)
		}
	}
	if c.Interval, err = envDuration("CLUSTER_BUILD_WATCH_DISCOVERY_INTERVAL", c.Interval); err != nil {
		return c, err
	}
	if c.Interval < 10*time.Second {
		return c, fmt.Errorf("CLUSTER_BUILD_WATCH_DISCOVERY_INTERVAL %s: at least 10s", c.Interval)
	}
	if c.Lookback, err = envDuration("CLUSTER_BUILD_WATCH_DISCOVERY_LOOKBACK", c.Lookback); err != nil {
		return c, err
	}

	gitops, set := os.LookupEnv("CLUSTER_BUILD_WATCH_DISCOVERY_GITOPS")
	if !set {
		gitops = "flux-system/machinery-xrs"
	}
	if gitops = strings.TrimSpace(gitops); gitops != "" {
		ns, name, ok := strings.Cut(gitops, "/")
		if !ok || ns == "" || name == "" {
			return c, fmt.Errorf("CLUSTER_BUILD_WATCH_DISCOVERY_GITOPS=%q: want <namespace>/<name>", gitops)
		}
		// Revision "": any applied revision counts (dapr-workflows#59). The
		// stack exists already, so the commit that carries it is applied;
		// the phase only confirms the Kustomization is healthy.
		c.GitOps = &GitOpsTarget{Kind: "flux", Namespace: ns, Name: name,
			ObservationSource: c.source("Kustomization")}
	}
	c.ArgoServer = strings.TrimSpace(os.Getenv("CLUSTER_BUILD_WATCH_DISCOVERY_ARGO_SERVER"))

	if c.BuildTimeoutMin, err = envInt("CLUSTER_BUILD_WATCH_DISCOVERY_TIMEOUT_MIN", 0); err != nil {
		return c, err
	}
	if c.TeardownTimeout, err = envInt("CLUSTER_BUILD_WATCH_TEARDOWN_TIMEOUT_MIN", c.TeardownTimeout); err != nil {
		return c, err
	}
	for _, p := range strings.Split(os.Getenv("CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			return c, fmt.Errorf("CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE pattern %q: %w", p, err)
		}
		c.Exclude = append(c.Exclude, p)
	}
	return c, nil
}

// source is the machinery source every discovered watch reads through, with
// its own copy of the server so inputs do not share a pointer.
func (c DiscoveryConfig) source(kind string) ObservationSource {
	m := c.Machinery
	m.Kind = kind
	return ObservationSource{Source: sourceMachinery, Machinery: &m}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def, fmt.Errorf("%s=%q: want a duration like 60s or 30m", k, v)
	}
	return d, nil
}

func envInt(k string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def, fmt.Errorf("%s=%q: want a non-negative integer", k, v)
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PLAN — which stacks get which watch, free of Dapr and gRPC
// ─────────────────────────────────────────────────────────────────────────────

// stackView is what discovery needs to know about one ClusterStack.
type stackView struct {
	Namespace, Name, UID string
	Created              time.Time // zero when machinery sent none
	Deleting             bool
	Ready                bool
	OptOut               bool
}

func (s stackView) key() string { return s.Namespace + "/" + s.Name }

func viewStack(r *rs.ResourceStatus) (stackView, error) {
	v := stackView{
		Namespace: r.GetNamespace(),
		Name:      r.GetName(),
		UID:       r.GetInfoFields()[infoUID],
		Deleting:  isDeleting(r),
		OptOut:    strings.EqualFold(strings.TrimSpace(r.GetInfoFields()[infoWatch]), "false"),
	}
	if ts := r.GetCreationTimestamp(); ts != "" {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return v, fmt.Errorf("%s: creationTimestamp %q: %w", v.key(), ts, err)
		}
		v.Created = t
	}
	// The same judgement as the build watch's end: Ready condition AND
	// status.ready where machinery maps it.
	obj, err := machineryObject(r)
	if err != nil {
		return v, fmt.Errorf("%s: %w", v.key(), err)
	}
	o := parseXR(obj)
	v.Ready = o.ReadyCondition && (o.Ready == nil || *o.Ready)
	return v, nil
}

// discoveryAction is one watch discovery wants to exist.
type discoveryAction struct {
	InstanceID string
	Workflow   string
	Stack      string // namespace/name, for logs
	Input      any
}

// plan decides, for one list of stacks, which watches should exist. The
// second result says why each other stack was left alone, for logs and tests.
// Whether a watch already exists is not plan's business: ensure asks Dapr.
func (c DiscoveryConfig) plan(stacks []stackView, workerStart, now time.Time) ([]discoveryAction, map[string]string) {
	var acts []discoveryAction
	skipped := map[string]string{}
	for _, s := range stacks {
		switch {
		case s.UID == "":
			skipped[s.key()] = "no " + infoUID + " info field (map metadata.uid on the machinery server)"
		case s.OptOut:
			skipped[s.key()] = "opted out (" + optOutAnnotation + ": \"false\")"
		case c.excluded(s.key()):
			skipped[s.key()] = "excluded by CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE"
		case s.Deleting:
			acts = append(acts, discoveryAction{
				InstanceID: "teardown-" + s.UID,
				Workflow:   teardownWorkflowName,
				Stack:      s.key(),
				Input:      c.teardownInput(s),
			})
		case s.Ready:
			skipped[s.key()] = "ready"
		case s.Created.IsZero():
			skipped[s.key()] = "no creationTimestamp"
		case s.Created.Before(workerStart) && now.Sub(s.Created) > c.Lookback:
			skipped[s.key()] = fmt.Sprintf("created %s, before the worker started and outside the look-back of %s",
				s.Created.UTC().Format(time.RFC3339), c.Lookback)
		default:
			acts = append(acts, discoveryAction{
				InstanceID: "build-" + s.UID,
				Workflow:   buildWorkflowName,
				Stack:      s.key(),
				Input:      c.buildInput(s),
			})
		}
	}
	return acts, skipped
}

func (c DiscoveryConfig) excluded(key string) bool {
	for _, p := range c.Exclude {
		if ok, _ := path.Match(p, key); ok {
			return true
		}
	}
	return false
}

// buildInput is what a ClusterBuildWatch for this stack would post.
func (c DiscoveryConfig) buildInput(s stackView) Input {
	in := Input{
		Name:       s.Name,
		Target:     XRTarget{Namespace: s.Namespace, Name: s.Name, ObservationSource: c.source(c.Machinery.Kind)},
		TimeoutMin: c.BuildTimeoutMin,
	}
	if c.GitOps != nil {
		g := *c.GitOps
		g.ObservationSource = c.source(c.GitOps.Machinery.Kind)
		in.GitOps = &g
	}
	if c.ArgoServer != "" {
		m := MachinerySource{Server: c.ArgoServer, Kind: "Application", Plaintext: c.Machinery.Plaintext}
		in.Argo = &ArgoTarget{ObservationSource: ObservationSource{Source: sourceMachinery, Machinery: &m}}
	}
	return in
}

func (c DiscoveryConfig) teardownInput(s stackView) TeardownInput {
	return TeardownInput{
		Name:       s.Name,
		Target:     XRTarget{Namespace: s.Namespace, Name: s.Name, ObservationSource: c.source(c.Machinery.Kind)},
		TimeoutMin: c.TeardownTimeout,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LOOP — list, plan, ensure
// ─────────────────────────────────────────────────────────────────────────────

// workflowStarter is the part of the Dapr workflow client discovery uses;
// *workflow.Client satisfies it, tests fake it.
type workflowStarter interface {
	FetchWorkflowMetadata(ctx context.Context, id string, opts ...workflow.FetchWorkflowMetadataOptions) (*workflow.WorkflowMetadata, error)
	ScheduleWorkflow(ctx context.Context, name string, opts ...workflow.NewWorkflowOptions) (string, error)
}

type discoverer struct {
	cfg         DiscoveryConfig
	wf          workflowStarter
	workerStart time.Time
	now         func() time.Time
	// known holds the instance IDs Dapr has confirmed, so a steady state
	// costs one list per interval and no Dapr call.
	known map[string]bool
	// warned keeps a skip reason from being logged every interval.
	warned map[string]string
}

func newDiscoverer(cfg DiscoveryConfig, wf workflowStarter, workerStart time.Time) *discoverer {
	return &discoverer{cfg: cfg, wf: wf, workerStart: workerStart, now: time.Now,
		known: map[string]bool{}, warned: map[string]string{}}
}

// run polls until ctx ends. The first pass runs at once.
func (d *discoverer) run(ctx context.Context) {
	slog.Info("discovery on", "server", d.cfg.Machinery.Server, "kind", d.cfg.Machinery.Kind,
		"interval", d.cfg.Interval, "lookback", d.cfg.Lookback)
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		if err := d.pass(ctx); err != nil {
			slog.Warn("discovery pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pass is one list + plan + ensure. A failed ensure does not stop the
// others; it is retried next pass.
func (d *discoverer) pass(ctx context.Context) error {
	c, err := newMachineryClient(&d.cfg.Machinery)
	if err != nil {
		return err
	}
	list, err := c.list(ctx, d.cfg.Machinery.Kind)
	if err != nil {
		return err
	}
	var stacks []stackView
	var errs []error
	for _, r := range list {
		v, err := viewStack(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stacks = append(stacks, v)
	}

	acts, skipped := d.cfg.plan(stacks, d.workerStart, d.now())
	for key, why := range skipped {
		if d.warned[key] != why {
			d.warned[key] = why
			slog.Info("discovery skips", "stack", key, "reason", why)
		}
	}
	for _, a := range acts {
		started, err := d.ensure(ctx, a)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s %s: %w", a.InstanceID, a.Stack, err))
		case started:
			slog.Info("discovery started a watch", "instance", a.InstanceID, "workflow", a.Workflow, "stack", a.Stack)
		}
	}
	return errors.Join(errs...)
}

// ensure makes sure the instance exists, the Go form of the trigger Job's
// "start once": look first, start only on a clean not-found, and take an
// "already exists" on the start as known -- another pass or replica won.
// started is true only when this call created it.
func (d *discoverer) ensure(ctx context.Context, a discoveryAction) (started bool, err error) {
	if d.known[a.InstanceID] {
		return false, nil
	}
	_, err = d.wf.FetchWorkflowMetadata(ctx, a.InstanceID)
	switch {
	case err == nil:
		d.known[a.InstanceID] = true
		return false, nil
	case !instanceNotFound(err):
		return false, fmt.Errorf("cannot tell whether the instance exists, not starting it: %w", err)
	}
	_, err = d.wf.ScheduleWorkflow(ctx, a.Workflow,
		workflow.WithInstanceID(a.InstanceID),
		workflow.WithInput(a.Input),
		// Without it Dapr would restart a COMPLETED instance under the same
		// ID -- a finished or failed watch would run again.
		workflow.NewWorkflowOptions(api.WithEnforceUniqueInstanceID()))
	switch {
	case err == nil:
		d.known[a.InstanceID] = true
		return true, nil
	case instanceExists(err):
		d.known[a.InstanceID] = true
		return false, nil
	}
	return false, err
}

// instanceNotFound: durabletask's sentinel when the sidecar answers
// exists=false, and the codes/texts Dapr uses when it answers with an error.
func instanceNotFound(err error) bool {
	if errors.Is(err, api.ErrInstanceNotFound) || status.Code(err) == codes.NotFound {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such instance") || strings.Contains(msg, "not found")
}

func instanceExists(err error) bool {
	if errors.Is(err, api.ErrDuplicateInstance) || status.Code(err) == codes.AlreadyExists {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}
