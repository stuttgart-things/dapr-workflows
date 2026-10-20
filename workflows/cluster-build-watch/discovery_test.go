package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dapr/durabletask-go/api"
	"github.com/dapr/durabletask-go/api/protos"
	"github.com/dapr/durabletask-go/workflow"
	rs "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeDapr stands in for the workflow client: existing instances, and what
// ScheduleWorkflow should answer.
type fakeDapr struct {
	existing    map[string]bool
	fetchErr    error // returned for every fetch of an unknown instance instead of not-found
	scheduleErr error
	fetched     []string
	scheduled   map[string]any // instance ID -> input
	names       map[string]string
}

func newFakeDapr() *fakeDapr {
	return &fakeDapr{existing: map[string]bool{}, scheduled: map[string]any{}, names: map[string]string{}}
}

func (f *fakeDapr) FetchWorkflowMetadata(_ context.Context, id string, _ ...workflow.FetchWorkflowMetadataOptions) (*workflow.WorkflowMetadata, error) {
	f.fetched = append(f.fetched, id)
	if f.existing[id] {
		return &workflow.WorkflowMetadata{InstanceId: id}, nil
	}
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return nil, api.ErrInstanceNotFound
}

func (f *fakeDapr) ScheduleWorkflow(_ context.Context, name string, opts ...workflow.NewWorkflowOptions) (string, error) {
	if f.scheduleErr != nil {
		return "", f.scheduleErr
	}
	// Apply the options to the request the way the real client does, and
	// keep what discovery asked for.
	id, input, unique := applyOpts(opts)
	if !unique {
		return "", errors.New("discovery must schedule with EnforceUniqueInstanceId")
	}
	if f.existing[id] {
		return "", status.Error(codes.AlreadyExists, "an active workflow with ID '"+id+"' already exists")
	}
	f.existing[id] = true
	f.scheduled[id] = input
	f.names[id] = name
	return id, nil
}

// applyOpts runs the options against a CreateInstanceRequest.
func applyOpts(opts []workflow.NewWorkflowOptions) (id, input string, unique bool) {
	req := &protos.CreateInstanceRequest{}
	for _, o := range opts {
		_ = o(req)
	}
	return req.GetInstanceId(), req.GetInput().GetValue(), req.GetEnforceUniqueInstanceId()
}

var (
	tStart = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC) // worker start
	tNow   = tStart.Add(10 * time.Minute)
)

// stack builds a ClusterStack as machinery v1.14 returns it, with the info
// fields discovery needs.
func stack(name, uid string, created time.Time, ready bool, info map[string]string, conds ...*rs.Condition) *rs.ResourceStatus {
	fields := map[string]string{infoUID: uid, infoStage: "vm", infoStatusReady: fmt.Sprint(ready)}
	for k, v := range info {
		if v == "" {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	if len(conds) == 0 {
		st := "False"
		if ready {
			st = "True"
		}
		conds = []*rs.Condition{cond("Ready", st, "Creating", "")}
	}
	return &rs.ResourceStatus{Name: name, Namespace: "default", Kind: "ClusterStack", InfoFields: fields,
		Conditions: conds, CreationTimestamp: created.UTC().Format(time.RFC3339)}
}

func testDiscoveryConfig() DiscoveryConfig {
	c := DiscoveryConfig{
		Enabled:         true,
		Machinery:       MachinerySource{Server: bufServer, Kind: "ClusterStack", Plaintext: true},
		Interval:        time.Minute,
		Lookback:        30 * time.Minute,
		TeardownTimeout: 60,
	}
	c.GitOps = &GitOpsTarget{Kind: "flux", Namespace: "flux-system", Name: "machinery-xrs", ObservationSource: c.source("Kustomization")}
	return c
}

// discover runs one pass against a fake machinery serving stacks.
func discover(t *testing.T, d *discoverer, stacks ...*rs.ResourceStatus) error {
	t.Helper()
	t.Setenv("MACHINERY_AUTH_TOKEN", "")
	f := newFake()
	f.lists = map[string][]*rs.ResourceStatus{"ClusterStack": stacks}
	startMachinery(t, f, nil, nil)
	return d.pass(context.Background())
}

func newTestDiscoverer(dp *fakeDapr) *discoverer {
	d := newDiscoverer(testDiscoveryConfig(), dp, tStart)
	d.now = func() time.Time { return tNow }
	return d
}

func TestDiscoveryNewStackStartsBuildWatch(t *testing.T) {
	dp := newFakeDapr()
	d := newTestDiscoverer(dp)
	if err := discover(t, d, stack("app-dev", "u-1", tStart.Add(time.Minute), false, nil)); err != nil {
		t.Fatal(err)
	}
	raw, ok := dp.scheduled["build-u-1"].(string)
	if !ok || dp.names["build-u-1"] != buildWorkflowName {
		t.Fatalf("want build-u-1 as %s, got %v", buildWorkflowName, dp.names)
	}
	// The input is a valid ClusterBuildWatch input, read through machinery,
	// with the gitops phase at revision "" (any).
	var in Input
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	in.applyDefaults()
	if err := in.validate(); err != nil {
		t.Fatalf("discovered input does not validate: %v\n%s", err, raw)
	}
	if in.Target.Namespace != "default" || in.Target.Name != "app-dev" || !in.Target.fromMachinery() ||
		in.Target.Machinery.Server != bufServer || in.Target.Machinery.Kind != "ClusterStack" {
		t.Fatalf("target: %+v", in.Target)
	}
	if in.GitOps == nil || in.GitOps.Name != "machinery-xrs" || in.GitOps.Revision != "" || in.GitOps.Machinery.Kind != "Kustomization" {
		t.Fatalf("gitops: %+v", in.GitOps)
	}
	if strings.Contains(raw, "state") {
		t.Fatalf("a new watch must not carry state: %s", raw)
	}
}

// A stack created before the worker started is watched only within the
// look-back window.
func TestDiscoveryLookback(t *testing.T) {
	dp := newFakeDapr()
	d := newTestDiscoverer(dp)
	err := discover(t, d,
		stack("recent", "u-recent", tNow.Add(-20*time.Minute), false, nil),
		stack("old", "u-old", tNow.Add(-2*time.Hour), false, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dp.scheduled["build-u-recent"]; !ok {
		t.Fatal("a stack inside the look-back must be watched")
	}
	if _, ok := dp.scheduled["build-u-old"]; ok {
		t.Fatal("an old, not-ready stack outside the look-back must not be watched")
	}
	if !strings.Contains(d.warned["default/old"], "look-back") {
		t.Fatalf("skip reason: %q", d.warned["default/old"])
	}
}

// Known: an instance that exists (a restart mid-build) is not started again,
// and once confirmed, Dapr is not even asked again.
func TestDiscoveryKnownStackIsNotStartedAgain(t *testing.T) {
	dp := newFakeDapr()
	dp.existing["build-u-1"] = true
	d := newTestDiscoverer(dp)
	s := stack("app-dev", "u-1", tStart.Add(time.Minute), false, nil)
	for range 3 {
		if err := discover(t, d, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(dp.scheduled) != 0 {
		t.Fatalf("known instance scheduled again: %v", dp.scheduled)
	}
	if len(dp.fetched) != 1 {
		t.Fatalf("want one fetch, then the known set; fetched %v", dp.fetched)
	}

	// A watch this worker started is known too.
	dp2 := newFakeDapr()
	d2 := newTestDiscoverer(dp2)
	for range 2 {
		if err := discover(t, d2, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(dp2.scheduled) != 1 || len(dp2.fetched) != 1 {
		t.Fatalf("second pass must not start or ask again: scheduled %v, fetched %v", dp2.scheduled, dp2.fetched)
	}
}

// A rebuilt cluster with the same name has a new uid and gets a new watch.
func TestDiscoveryRebuiltStackGetsNewWatch(t *testing.T) {
	dp := newFakeDapr()
	dp.existing["build-u-1"] = true
	d := newTestDiscoverer(dp)
	if err := discover(t, d, stack("app-dev", "u-2", tStart.Add(5*time.Minute), false, nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := dp.scheduled["build-u-2"]; !ok {
		t.Fatalf("new uid must get a new watch: %v", dp.scheduled)
	}
}

func TestDiscoveryDeletingStackStartsTeardownWatch(t *testing.T) {
	dp := newFakeDapr()
	dp.existing["build-u-1"] = true
	d := newTestDiscoverer(dp)
	err := discover(t, d,
		// deletionTimestamp mapped, an OLD and READY stack: age and Ready do
		// not matter for a teardown.
		stack("by-field", "u-1", tNow.Add(-30*24*time.Hour), true, map[string]string{infoDeletionTimestamp: "2026-10-20T08:09:00Z"}),
		// not mapped: Crossplane's Ready reason Deleting is enough.
		stack("by-reason", "u-2", tNow.Add(-time.Hour), false, nil, cond("Ready", "False", "Deleting", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"teardown-u-1", "teardown-u-2"} {
		raw, ok := dp.scheduled[id].(string)
		if !ok || dp.names[id] != teardownWorkflowName {
			t.Fatalf("want %s as %s, got %v", id, teardownWorkflowName, dp.names)
		}
		var in TeardownInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		in.applyDefaults()
		if err := in.validate(); err != nil || in.TimeoutMin != 60 {
			t.Fatalf("teardown input: %v %+v", err, in)
		}
	}
	if _, ok := dp.scheduled["build-u-2"]; ok {
		t.Fatal("a deleting stack must not get a build watch")
	}
}

func TestDiscoveryOptOut(t *testing.T) {
	dp := newFakeDapr()
	d := newTestDiscoverer(dp)
	d.cfg.Exclude = []string{"default/test-*"}
	err := discover(t, d,
		stack("quiet", "u-1", tStart.Add(time.Minute), false, map[string]string{infoWatch: "false"}),
		stack("quiet-gone", "u-2", tStart.Add(time.Minute), false, map[string]string{infoWatch: "False", infoDeletionTimestamp: "2026-10-20T08:09:00Z"}),
		stack("test-kind9", "u-3", tStart.Add(time.Minute), false, nil),
		stack("loud", "u-4", tStart.Add(time.Minute), false, map[string]string{infoWatch: "true"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dp.scheduled) != 1 || dp.scheduled["build-u-4"] == nil {
		t.Fatalf("only the stack without opt-out may be watched: %v", dp.scheduled)
	}
	if !strings.Contains(d.warned["default/quiet"], optOutAnnotation) || !strings.Contains(d.warned["default/test-kind9"], "EXCLUDE") {
		t.Fatalf("skip reasons: %v", d.warned)
	}
}

func TestDiscoveryOldButReadyIsIgnored(t *testing.T) {
	dp := newFakeDapr()
	d := newTestDiscoverer(dp)
	err := discover(t, d,
		stack("old-ready", "u-1", tNow.Add(-48*time.Hour), true, nil),
		// New and Ready already (a fast build between two passes): nothing to watch.
		stack("new-ready", "u-2", tStart.Add(time.Minute), true, nil),
		// Ready condition but status.ready false: still building.
		stack("half", "u-3", tStart.Add(time.Minute), false, nil, cond("Ready", "True", "Available", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dp.scheduled) != 1 || dp.scheduled["build-u-3"] == nil {
		t.Fatalf("want only build-u-3: %v", dp.scheduled)
	}
	if d.warned["default/old-ready"] != "ready" {
		t.Fatalf("skip reason: %v", d.warned)
	}
}

// Instance already exists: the fetch said not-found, but the start lost a race
// (another pass or replica); that is "known", not an error.
func TestDiscoveryInstanceAlreadyExists(t *testing.T) {
	for name, err := range map[string]error{
		"grpc AlreadyExists": fmt.Errorf("failed to start workflow: %w", status.Error(codes.AlreadyExists, "exists")),
		"sentinel":           fmt.Errorf("start: %w", api.ErrDuplicateInstance),
		"dapr text":          errors.New("rpc error: code = Unknown desc = an active workflow with ID 'build-u-1' already exists"),
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDapr()
			dp.scheduleErr = err
			d := newTestDiscoverer(dp)
			if err := discover(t, d, stack("app-dev", "u-1", tStart.Add(time.Minute), false, nil)); err != nil {
				t.Fatalf("already exists must not be an error: %v", err)
			}
			if !d.known["build-u-1"] {
				t.Fatal("already exists must count as known")
			}
		})
	}
}

// When Dapr cannot say whether the instance exists, nothing is started: a
// duplicate watch sends every card twice.
func TestDiscoveryUnsureDoesNotStart(t *testing.T) {
	dp := newFakeDapr()
	dp.fetchErr = status.Error(codes.Unavailable, "sidecar not ready")
	d := newTestDiscoverer(dp)
	err := discover(t, d, stack("app-dev", "u-1", tStart.Add(time.Minute), false, nil))
	if err == nil || len(dp.scheduled) != 0 || d.known["build-u-1"] {
		t.Fatalf("want an error and no start: %v %v", err, dp.scheduled)
	}
	// Next pass, sidecar up: started.
	dp.fetchErr = nil
	if err := discover(t, d, stack("app-dev", "u-1", tStart.Add(time.Minute), false, nil)); err != nil || dp.scheduled["build-u-1"] == nil {
		t.Fatalf("retry: %v %v", err, dp.scheduled)
	}
}

// Without the UID info field there is no idempotent ID: skipped, with the
// fix in the reason.
func TestDiscoveryMissingUIDIsSkipped(t *testing.T) {
	dp := newFakeDapr()
	d := newTestDiscoverer(dp)
	if err := discover(t, d, stack("app-dev", "", tStart.Add(time.Minute), false, map[string]string{infoUID: ""})); err != nil {
		t.Fatal(err)
	}
	if len(dp.scheduled) != 0 || !strings.Contains(d.warned["default/app-dev"], "metadata.uid") {
		t.Fatalf("scheduled %v, reason %q", dp.scheduled, d.warned["default/app-dev"])
	}
}

func TestDiscoveryListErrorIsReported(t *testing.T) {
	dp := newFakeDapr()
	d := newTestDiscoverer(dp)
	d.cfg.Machinery.Kind = "NotConfigured"
	if err := discover(t, d); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want the InvalidArgument of an unconfigured kind, got %v", err)
	}
}

func TestDiscoveryConfigFromEnv(t *testing.T) {
	unset := func() {
		for _, k := range []string{"CLUSTER_BUILD_WATCH_DISCOVERY", "CLUSTER_BUILD_WATCH_DISCOVERY_SERVER", "CLUSTER_BUILD_WATCH_DISCOVERY_PLAINTEXT",
			"CLUSTER_BUILD_WATCH_DISCOVERY_KIND", "CLUSTER_BUILD_WATCH_DISCOVERY_INTERVAL", "CLUSTER_BUILD_WATCH_DISCOVERY_LOOKBACK",
			"CLUSTER_BUILD_WATCH_DISCOVERY_ARGO_SERVER", "CLUSTER_BUILD_WATCH_DISCOVERY_TIMEOUT_MIN",
			"CLUSTER_BUILD_WATCH_TEARDOWN_TIMEOUT_MIN", "CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE"} {
			t.Setenv(k, "")
		}
	}
	unset()
	c, err := discoveryConfigFromEnv()
	if err != nil || c.Enabled {
		t.Fatalf("default must be off: %+v %v", c, err)
	}

	t.Setenv("CLUSTER_BUILD_WATCH_DISCOVERY", "on")
	if _, err := discoveryConfigFromEnv(); err == nil {
		t.Fatal("on without a server must fail")
	}
	t.Setenv("CLUSTER_BUILD_WATCH_DISCOVERY_SERVER", "machinery-grpc.example:443")
	c, err = discoveryConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Interval != 60*time.Second || c.Lookback != 30*time.Minute || c.TeardownTimeout != 60 ||
		c.BuildTimeoutMin != 0 || c.Machinery.Kind != "ClusterStack" || c.Machinery.Plaintext || c.ArgoServer != "" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.GitOps == nil || c.GitOps.Namespace != "flux-system" || c.GitOps.Name != "machinery-xrs" || c.GitOps.Machinery.Server != "machinery-grpc.example:443" {
		t.Fatalf("default gitops: %+v", c.GitOps)
	}

	t.Setenv("CLUSTER_BUILD_WATCH_DISCOVERY_GITOPS", "")
	t.Setenv("CLUSTER_BUILD_WATCH_DISCOVERY_INTERVAL", "2m")
	t.Setenv("CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE", "default/test-*, ci/*")
	t.Setenv("CLUSTER_BUILD_WATCH_TEARDOWN_TIMEOUT_MIN", "90")
	c, err = discoveryConfigFromEnv()
	if err != nil || c.GitOps != nil || c.Interval != 2*time.Minute || len(c.Exclude) != 2 || c.TeardownTimeout != 90 {
		t.Fatalf("set-but-empty gitops is no gitops phase: %+v %v", c, err)
	}

	for k, v := range map[string]string{
		"CLUSTER_BUILD_WATCH_DISCOVERY":          "maybe",
		"CLUSTER_BUILD_WATCH_DISCOVERY_INTERVAL": "1s",
		"CLUSTER_BUILD_WATCH_DISCOVERY_GITOPS":   "machinery-xrs",
		"CLUSTER_BUILD_WATCH_DISCOVERY_EXCLUDE":  "[",
	} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := discoveryConfigFromEnv(); err == nil {
				t.Fatalf("%s=%q must be rejected", k, v)
			}
		})
	}
}
