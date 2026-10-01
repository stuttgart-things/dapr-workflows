package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rs "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeMachinery answers GetResourceDetail from a fixed table and records the
// authorization header of the last call.
type fakeMachinery struct {
	rs.UnimplementedResourceServiceServer
	mu       sync.Mutex
	lastAuth []string
	objects  map[string]*rs.ResourceStatus // kind/namespace/name
	kinds    map[string]bool               // configured kinds
	unserved map[string]bool               // configured, CRD not served
	echoAuth bool                          // put the auth header into the error, like a careless proxy
}

func (f *fakeMachinery) GetResourceDetail(ctx context.Context, req *rs.ResourceDetailRequest) (*rs.ResourceStatus, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.lastAuth = md.Get("authorization")
	f.mu.Unlock()
	if f.echoAuth {
		return nil, status.Errorf(codes.PermissionDenied, "rejected header %v", md.Get("authorization"))
	}
	if !f.kinds[req.Kind] {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported kind %q", req.Kind)
	}
	if f.unserved[req.Kind] {
		return nil, status.Errorf(codes.Unavailable, "kind %q is not served by the cluster (yet)", req.Kind)
	}
	if o, ok := f.objects[req.Kind+"/"+req.Namespace+"/"+req.Name]; ok {
		return o, nil
	}
	return nil, status.Errorf(codes.NotFound, "resource %s/%s not found", req.Kind, req.Name)
}

func (f *fakeMachinery) auth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

// startMachinery serves f over bufconn, with TLS when tlsCert is non-nil, and
// points the client seams at it.
func startMachinery(t *testing.T, f *fakeMachinery, tlsCert *tls.Certificate, pool *x509.CertPool) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	var opts []grpc.ServerOption
	if tlsCert != nil {
		opts = append(opts, grpc.Creds(credentials.NewServerTLSFromCert(tlsCert)))
	}
	srv := grpc.NewServer(opts...)
	rs.RegisterResourceServiceServer(srv, f)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	machineryExtraDialOpts = []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})}
	machineryTestRootCAs = pool
	t.Cleanup(func() { machineryExtraDialOpts, machineryTestRootCAs = nil, nil })
}

// selfSigned makes a CA-less leaf for "bufnet", the authority bufconn dials.
func selfSigned(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bufnet"},
		DNSNames:              []string{"bufnet"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

const bufServer = "passthrough:///bufnet"

func clusterStack(stage, ready string, conds ...*rs.Condition) *rs.ResourceStatus {
	info := map[string]string{"Distribution": "k3s"}
	if stage != "" {
		info[infoStage] = stage
	}
	if ready != "" {
		info[infoStatusReady] = ready
	}
	return &rs.ResourceStatus{Name: "app-dev", Namespace: "default", Kind: "ClusterStack", InfoFields: info, Conditions: conds}
}

func cond(typ, st, reason, msg string) *rs.Condition {
	return &rs.Condition{Type: typ, Status: st, Reason: reason, Message: msg}
}

func newFake() *fakeMachinery {
	return &fakeMachinery{
		kinds:    map[string]bool{"ClusterStack": true, "Kustomization": true, "Gateway": true},
		unserved: map[string]bool{"Gateway": true},
		objects: map[string]*rs.ResourceStatus{
			"ClusterStack/default/app-dev": clusterStack("platform", "false",
				cond("Synced", "True", "ReconcileSuccess", ""), cond("Ready", "False", "Creating", "Unready resources: ansiblerun")),
		},
	}
}

func machineryXR(server string, plaintext bool) XRTarget {
	return XRTarget{Namespace: "default", Name: "app-dev", ObservationSource: ObservationSource{
		Source: sourceMachinery, Machinery: &MachinerySource{Server: server, Kind: "ClusterStack", Plaintext: plaintext},
	}}
}

func TestMachineryClientCodes(t *testing.T) {
	t.Setenv("MACHINERY_AUTH_TOKEN", "")
	f := newFake()
	startMachinery(t, f, nil, nil)
	c, err := newMachineryClient(&MachinerySource{Server: bufServer, Plaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	r, err := c.detail(ctx, "ClusterStack", "default", "app-dev")
	if err != nil || r == nil || r.InfoFields[infoStage] != "platform" {
		t.Fatalf("found: %v %v", r, err)
	}
	if r, err := c.detail(ctx, "ClusterStack", "default", "gone"); err != nil || r != nil {
		t.Fatalf("NotFound must be nil, nil: %v %v", r, err)
	}
	// Configured but not served, and not configured at all: both errors,
	// never "not found".
	if _, err := c.detail(ctx, "Gateway", "default", "x"); status.Code(err) != codes.Unavailable {
		t.Fatalf("Unavailable must be an error: %v", err)
	}
	if _, err := c.detail(ctx, "Application", "argocd", "x"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("InvalidArgument must be an error: %v", err)
	}

	// Through the activity code path: NotFound is Found=false.
	o, err := observeXRMachinery(ctx, XRTarget{Namespace: "default", Name: "gone", ObservationSource: ObservationSource{
		Source: sourceMachinery, Machinery: &MachinerySource{Server: bufServer, Kind: "ClusterStack", Plaintext: true}}})
	if err != nil || o.Found {
		t.Fatalf("absent XR: %+v %v", o, err)
	}
	o, err = observeXRMachinery(ctx, machineryXR(bufServer, true))
	if err != nil || !o.Found || o.Stage != "platform" || o.ReadyCondition {
		t.Fatalf("present XR: %+v %v", o, err)
	}
}

func TestMachineryUnreachableIsError(t *testing.T) {
	t.Setenv("MACHINERY_AUTH_TOKEN", "")
	c, _ := newMachineryClient(&MachinerySource{Server: "127.0.0.1:1", Plaintext: true})
	if _, err := c.detail(context.Background(), "ClusterStack", "default", "app-dev"); err == nil {
		t.Fatal("a dead endpoint must be an error, not not-found")
	}
}

const fakeToken = "s3cr3t-token-value" // pragma: allowlist secret -- test token

func TestMachineryTokenOnlyOverTLS(t *testing.T) {
	cert, pool := selfSigned(t)

	t.Run("tls with token", func(t *testing.T) {
		t.Setenv("MACHINERY_AUTH_TOKEN", fakeToken)
		f := newFake()
		startMachinery(t, f, cert, pool)
		if _, err := observeXRMachinery(context.Background(), machineryXR(bufServer, false)); err != nil {
			t.Fatal(err)
		}
		if got := f.auth(); len(got) != 1 || got[0] != "Bearer "+fakeToken {
			t.Fatalf("authorization header: %v", got)
		}
	})

	t.Run("tls without token", func(t *testing.T) {
		t.Setenv("MACHINERY_AUTH_TOKEN", "")
		f := newFake()
		startMachinery(t, f, cert, pool)
		if _, err := observeXRMachinery(context.Background(), machineryXR(bufServer, false)); err != nil {
			t.Fatal(err)
		}
		if got := f.auth(); len(got) != 0 {
			t.Fatalf("no token configured, yet authorization sent: %v", got)
		}
	})

	t.Run("token from file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "token")
		os.WriteFile(p, []byte(fakeToken+"\n"), 0o600)
		t.Setenv("MACHINERY_AUTH_TOKEN_FILE", p)
		t.Setenv("MACHINERY_AUTH_TOKEN", "other")
		f := newFake()
		startMachinery(t, f, cert, pool)
		if _, err := observeXRMachinery(context.Background(), machineryXR(bufServer, false)); err != nil {
			t.Fatal(err)
		}
		if got := f.auth(); len(got) != 1 || got[0] != "Bearer "+fakeToken {
			t.Fatalf("the file wins and is trimmed: %v", got)
		}
	})

	t.Run("unreadable token file is an error", func(t *testing.T) {
		t.Setenv("MACHINERY_AUTH_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
		if _, err := newMachineryClient(&MachinerySource{Server: bufServer}); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("plaintext never sends the token", func(t *testing.T) {
		t.Setenv("MACHINERY_AUTH_TOKEN", fakeToken)
		f := newFake()
		startMachinery(t, f, nil, nil)
		if _, err := observeXRMachinery(context.Background(), machineryXR(bufServer, true)); err != nil {
			t.Fatal(err)
		}
		if got := f.auth(); len(got) != 0 {
			t.Fatalf("token sent in clear text: %v", got)
		}
	})

	t.Run("tls client against plaintext server fails", func(t *testing.T) {
		t.Setenv("MACHINERY_AUTH_TOKEN", "")
		startMachinery(t, newFake(), nil, pool)
		if _, err := observeXRMachinery(context.Background(), machineryXR(bufServer, false)); err == nil {
			t.Fatal("plaintext: false must dial TLS, and a TLS handshake against a plaintext server fails")
		}
	})

	t.Run("untrusted certificate fails", func(t *testing.T) {
		t.Setenv("MACHINERY_AUTH_TOKEN", fakeToken)
		f := newFake()
		startMachinery(t, f, cert, x509.NewCertPool())
		_, err := observeXRMachinery(context.Background(), machineryXR(bufServer, false))
		if err == nil {
			t.Fatal("want a verification error")
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Fatalf("token in error: %v", err)
		}
		if got := f.auth(); len(got) != 0 {
			t.Fatalf("token reached an unverified server: %v", got)
		}
	})
}

func TestMachineryErrorNeverCarriesToken(t *testing.T) {
	cert, pool := selfSigned(t)
	t.Setenv("MACHINERY_AUTH_TOKEN", fakeToken)
	f := newFake()
	f.echoAuth = true
	startMachinery(t, f, cert, pool)
	_, err := observeXRMachinery(context.Background(), machineryXR(bufServer, false))
	if err == nil {
		t.Fatal("want the PermissionDenied error")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("token leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("got %v", err)
	}
	// And through the activity, whose error lands in the workflow history.
	_, err = ObserveXR(fakeActivity{in: machineryXR(bufServer, false)})
	if err == nil || strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("activity error: %v", err)
	}
}

func TestMachineryDialOptions(t *testing.T) {
	tlsC := &machineryClient{server: "x:443"}
	plainC := &machineryClient{server: "x:80", plaintext: true}
	// One transport-credentials option each; the distinction itself is
	// exercised end to end in TestMachineryTokenOnlyOverTLS.
	if len(tlsC.dialOptions()) != 1 || len(plainC.dialOptions()) != 1 {
		t.Fatal("unexpected dial options")
	}
	if _, err := newMachineryClient(&MachinerySource{}); err == nil {
		t.Fatal("empty server must be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MAPPING
// ─────────────────────────────────────────────────────────────────────────────

func TestXRFromMachinery(t *testing.T) {
	// Stage parked, building.
	o, err := xrFromMachinery("ClusterStack", clusterStack("baseos", "false",
		cond("Synced", "True", "ReconcileSuccess", ""), cond("Ready", "False", "Creating", "Unready resources: ansiblerun-baseos")))
	if err != nil || !o.Found || o.Stage != "baseos" || o.Ready == nil || *o.Ready || o.ReadyCondition || o.SyncedFalse {
		t.Fatalf("parked: %+v %v", o, err)
	}
	if !strings.Contains(o.Message, "ansiblerun-baseos") {
		t.Fatalf("the Ready message says what is pending: %q", o.Message)
	}

	// Ready on both signals.
	o, err = xrFromMachinery("ClusterStack", clusterStack("ready", "true",
		cond("Synced", "True", "", ""), cond("Ready", "True", "Available", ""), cond("Responsive", "True", "", "")))
	if err != nil || !o.ReadyCondition || o.Ready == nil || !*o.Ready || o.Stage != "ready" {
		t.Fatalf("ready: %+v %v", o, err)
	}

	// status.ready true, Ready condition False: not done (management-plane).
	o, err = xrFromMachinery("ClusterStack", clusterStack("management-plane", "true",
		cond("Ready", "False", "Creating", "Unready resources: mgmt")))
	if err != nil || o.ReadyCondition || !*o.Ready {
		t.Fatalf("status.ready alone: %+v %v", o, err)
	}

	// Synced=False carries its message.
	o, err = xrFromMachinery("ClusterStack", clusterStack("vm", "false",
		cond("Synced", "False", "ReconcileError", "cannot compose resources: boom"), cond("Ready", "False", "Creating", "")))
	if err != nil || !o.SyncedFalse || !strings.HasPrefix(o.Message, "ReconcileError: cannot compose") {
		t.Fatalf("synced false: %+v %v", o, err)
	}

	// Just created: no stage, no status.ready yet.
	o, err = xrFromMachinery("ClusterStack", clusterStack("", "", cond("Synced", "True", "", "")))
	if err != nil || o.Stage != "" || o.Ready != nil || o.ReadyCondition {
		t.Fatalf("fresh: %+v %v", o, err)
	}

	// The gap: Ready=True but no StatusReady for a ClusterStack is an error,
	// never a finished watch on the condition alone.
	if _, err := xrFromMachinery("ClusterStack", clusterStack("ready", "", cond("Ready", "True", "", ""))); err == nil || !strings.Contains(err.Error(), infoStatusReady) {
		t.Fatalf("want the missing-StatusReady error, got %v", err)
	}
	// Another XR kind without status.ready: the condition decides, as on kube.
	if o, err := xrFromMachinery("Other", clusterStack("", "", cond("Ready", "True", "", ""))); err != nil || !o.ReadyCondition {
		t.Fatalf("other kind: %+v %v", o, err)
	}
	if _, err := xrFromMachinery("ClusterStack", clusterStack("vm", "maybe")); err == nil {
		t.Fatal("a non-bool StatusReady must be an error")
	}
}

// The machinery mapping and the kube parser agree on the same object.
func TestXRFromMachineryMatchesKube(t *testing.T) {
	kube := parseXR(obj(t, `{"status":{"stage":"distribution","ready":false,"conditions":[
		{"type":"Synced","status":"True"},
		{"type":"Ready","status":"False","reason":"Creating","message":"Unready resources: ansiblerun"}]}}`))
	m, err := xrFromMachinery("ClusterStack", clusterStack("distribution", "false",
		cond("Synced", "True", "", ""), cond("Ready", "False", "Creating", "Unready resources: ansiblerun")))
	if err != nil {
		t.Fatal(err)
	}
	if kube.Stage != m.Stage || *kube.Ready != *m.Ready || kube.ReadyCondition != m.ReadyCondition || kube.SyncedFalse != m.SyncedFalse || kube.Message != m.Message {
		t.Fatalf("kube %+v != machinery %+v", kube, m)
	}
}

func kustomization(revision string, conds ...*rs.Condition) *rs.ResourceStatus {
	info := map[string]string{"Path": "./clusters/machinery/xrs"}
	if revision != "" {
		info[infoRevision] = revision
		info["Attempted"] = revision
	}
	return &rs.ResourceStatus{Name: "machinery-xrs", Namespace: "flux-system", Kind: "Kustomization",
		InfoFields: info, Conditions: conds, Generation: 3, ObservedGeneration: 3}
}

func TestGitOpsFromMachinery(t *testing.T) {
	const have = "refs/heads/main@sha1:abc1234def5678abc1234def5678abc1234def56" // pragma: allowlist secret -- fake SHA
	ready := cond("Ready", "True", "ReconciliationSucceeded", "Applied revision: "+have)

	o, err := gitOpsFromMachinery(kustomization(have, ready), "abc1234")
	if err != nil || !o.Found || !o.Synced || o.Revision != have || o.Health != "True" {
		t.Fatalf("at the wanted revision: %+v %v", o, err)
	}
	o, err = gitOpsFromMachinery(kustomization(have, ready), "fff0000")
	if err != nil || o.Synced {
		t.Fatalf("behind the wanted revision is not synced: %+v %v", o, err)
	}
	if o, err := gitOpsFromMachinery(kustomization(have, ready), ""); err != nil || !o.Synced {
		t.Fatalf("no revision asked: %+v %v", o, err)
	}

	// The usual hang on machinery: dependsOn machinery-fleet-state not ready.
	dep := cond("Ready", "False", "DependencyNotReady", "dependency 'flux-system/machinery-fleet-state' is not ready")
	o, err = gitOpsFromMachinery(kustomization(have, dep), "abc1234")
	if err != nil || o.Synced || o.Degraded {
		t.Fatalf("DependencyNotReady waits, it is not degraded: %+v %v", o, err)
	}
	if !strings.Contains(o.Message, "machinery-fleet-state") || !strings.Contains(o.Health, "DependencyNotReady") {
		t.Fatalf("the reason must reach Message and Health: %+v", o)
	}

	// A real failure is degraded.
	o, err = gitOpsFromMachinery(kustomization(have, cond("Ready", "False", "BuildFailed", "kustomize build failed")), "abc1234")
	if err != nil || !o.Degraded || !strings.HasPrefix(o.Message, "BuildFailed") {
		t.Fatalf("build failed: %+v %v", o, err)
	}

	// The gap: Ready without a Revision field cannot be checked against a
	// wanted revision -- an error, not an endless wait.
	if _, err := gitOpsFromMachinery(kustomization("", ready), "abc1234"); err == nil || !strings.Contains(err.Error(), infoRevision) {
		t.Fatalf("want the missing-Revision error, got %v", err)
	}
	if o, err := gitOpsFromMachinery(kustomization("", ready), ""); err != nil || !o.Synced {
		t.Fatalf("without a wanted revision Ready is enough: %+v %v", o, err)
	}
}

// The DependencyNotReady reason reaches the stuck notification, through
// Health, without any change to Advance.
func TestStuckGitOpsSyncShowsReason(t *testing.T) {
	in := testInput(true)
	in.GitOps.Kind = "flux"
	st := newState(in, t0)
	o, _ := gitOpsFromMachinery(kustomization("refs/heads/main@sha1:0000000aaaa",
		cond("Ready", "False", "DependencyNotReady", "dependency 'flux-system/machinery-fleet-state' is not ready")), in.GitOps.Revision)
	Advance(in, st, &o, nil, t0.Add(time.Minute))
	evs := Advance(in, st, &o, nil, t0.Add(16*time.Minute))
	if types(evs) != "stuck" || !strings.Contains(evs[0].Message, "machinery-fleet-state") {
		t.Fatalf("got %q: %+v", types(evs), evs)
	}
}
