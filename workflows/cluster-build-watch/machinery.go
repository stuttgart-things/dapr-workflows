package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	rs "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// machinery as an observation source: one GetResourceDetail per observation,
// against a machinery ResourceService (read-only gRPC,
// github.com/stuttgart-things/machinery). machinery keeps an informer cache of
// the kinds it is configured for and projects each object into a
// ResourceStatus: conditions, generation, and per-kind "info fields" (a
// label -> value map of selected paths).
//
// The worker never needs RBAC on the watched cluster this way, and the watched
// cluster need not be the one the worker runs on.
//
// Environment (never the workflow input, which is persisted):
//
//	MACHINERY_AUTH_TOKEN_FILE  file holding a bearer token; wins over
//	MACHINERY_AUTH_TOKEN       the token itself
//	SSL_CERT_FILE              CA bundle for TLS (Go's system pool honours it)

// machineryCallTimeout bounds one dial + call. machinery answers from its
// cache, so anything near this is a network problem, not a slow server.
const machineryCallTimeout = 15 * time.Second

// Info field labels as the machinery deployment configures them, see
// README "Source machinery". machinery leaves a label out when the field is
// empty on the object, so "absent" means "not configured" OR "not written yet".
const (
	infoStage       = "Stage"       // ClusterStack status.stage
	infoStatusReady = "StatusReady" // ClusterStack status.ready, "true"/"false"
	infoRevision    = "Revision"    // Kustomization status.lastAppliedRevision
)

type machineryClient struct {
	server    string
	plaintext bool
	token     string
	// rootCAs nil means the system pool, which reads SSL_CERT_FILE.
	rootCAs  *x509.CertPool
	dialOpts []grpc.DialOption
}

// Test seams: the bufconn dialer and the test CA. Both nil in production.
var (
	machineryExtraDialOpts []grpc.DialOption
	machineryTestRootCAs   *x509.CertPool
)

func newMachineryClient(src *MachinerySource) (*machineryClient, error) {
	if src == nil || src.Server == "" {
		return nil, fmt.Errorf("machinery.server is not set")
	}
	token, err := machineryToken()
	if err != nil {
		return nil, err
	}
	return &machineryClient{
		server:    src.Server,
		plaintext: src.Plaintext,
		token:     token,
		rootCAs:   machineryTestRootCAs,
		dialOpts:  machineryExtraDialOpts,
	}, nil
}

// machineryToken is read on every call, so a rotated Secret takes effect
// without a restart. An unreadable file is an error rather than "no auth":
// the operator asked for a token, and silently calling without one would end
// in an Unauthenticated that points the wrong way.
func machineryToken() (string, error) {
	if f := os.Getenv("MACHINERY_AUTH_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read MACHINERY_AUTH_TOKEN_FILE: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv("MACHINERY_AUTH_TOKEN")), nil
}

func (c *machineryClient) dialOptions() []grpc.DialOption {
	creds := insecure.NewCredentials()
	if !c.plaintext {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.rootCAs})
	}
	return append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, c.dialOpts...)
}

// detail fetches one object. It returns nil, nil only on NotFound. Every other
// code is an error: InvalidArgument means the kind is not configured on that
// server, Unavailable that its CRD is not served or the transport failed --
// reporting either as "not found" would make the watch wait for an object it
// can never see (the same rule as a 403 in kube.go).
//
// One connection per call. An activity makes exactly one RPC every
// pollSeconds (>= 30 s by default), so a pooled connection would save one TLS
// handshake per half minute and cost lifecycle code: a cache keyed by server,
// closing on CA or token rotation, and a channel that may sit in
// TRANSIENT_FAILURE between activities. grpc.NewClient is lazy and Close is
// cheap.
func (c *machineryClient) detail(ctx context.Context, kind, namespace, name string) (*rs.ResourceStatus, error) {
	what := fmt.Sprintf("machinery %s: %s %s/%s", c.server, kind, namespace, name)
	conn, err := grpc.NewClient(c.server, c.dialOptions()...)
	if err != nil {
		return nil, c.sanitize(fmt.Errorf("%s: %w", what, err))
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(ctx, machineryCallTimeout)
	defer cancel()
	// Only over TLS. Plaintext is for an in-cluster Service, and a bearer
	// token in clear text is a token given away.
	if c.token != "" && !c.plaintext {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}

	res, err := rs.NewResourceServiceClient(conn).GetResourceDetail(ctx,
		&rs.ResourceDetailRequest{Kind: kind, Namespace: namespace, Name: name})
	switch status.Code(err) {
	case codes.OK:
		return res, nil
	case codes.NotFound:
		return nil, nil
	default:
		return nil, c.sanitize(fmt.Errorf("%s: %w", what, err))
	}
}

// sanitize keeps the token out of an error, which ends up in the workflow
// history, the custom status and the logs. Nothing here formats the token
// into an error, but a server or proxy may echo request headers back.
func (c *machineryClient) sanitize(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "<redacted>"))
}

// ─────────────────────────────────────────────────────────────────────────────
// MAPPING — ResourceStatus into the shape the kube parsers read
// ─────────────────────────────────────────────────────────────────────────────

// machineryObject rebuilds the minimal unstructured object parseXR and
// parseFluxKustomization read, so both sources are judged by the same code:
//
//	status.conditions[]          <- Conditions (type, status, reason, message)
//	status.stage                 <- info field Stage
//	status.ready                 <- info field StatusReady ("true"/"false")
//	status.lastAppliedRevision   <- info field Revision
//
// An info field machinery does not return is left out, exactly like a field
// the object has not written yet. Callers check the gaps that matter
// (xrFromMachinery, gitOpsFromMachinery).
func machineryObject(r *rs.ResourceStatus) (map[string]any, error) {
	st := map[string]any{}
	conds := make([]any, 0, len(r.GetConditions()))
	for _, c := range r.GetConditions() {
		conds = append(conds, map[string]any{
			"type":    c.GetType(),
			"status":  c.GetStatus(),
			"reason":  c.GetReason(),
			"message": c.GetMessage(),
		})
	}
	st["conditions"] = conds

	info := r.GetInfoFields()
	if v, ok := info[infoStage]; ok {
		st["stage"] = v
	}
	if v, ok := info[infoStatusReady]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("machinery info field %s=%q is not a bool", infoStatusReady, v)
		}
		st["ready"] = b
	}
	if v, ok := info[infoRevision]; ok {
		st["lastAppliedRevision"] = v
	}
	return map[string]any{"status": st}, nil
}

// xrFromMachinery judges an XR read through machinery.
//
// Gap: the "both signals" rule (Ready condition AND status.ready) needs the
// StatusReady info field. machinery omits it both when the server does not
// map status.ready and when the object has not written it, and the two cannot
// be told apart. The kube path would treat a missing status.ready as "this XRD
// has none" and let the condition decide. For a ClusterStack, which always
// has status.ready, that would finish a watch on the condition alone -- the
// very case the rule exists for -- so it is an error instead.
func xrFromMachinery(kind string, r *rs.ResourceStatus) (XRObservation, error) {
	obj, err := machineryObject(r)
	if err != nil {
		return XRObservation{}, err
	}
	o := parseXR(obj)
	if _, ok := r.GetInfoFields()[infoStatusReady]; !ok && kind == "ClusterStack" && o.ReadyCondition {
		return XRObservation{}, fmt.Errorf(
			"machinery returns Ready=True but no %s info field for ClusterStack %s/%s: cannot confirm status.ready; map status.ready as %s on the machinery server",
			infoStatusReady, r.GetNamespace(), r.GetName(), infoStatusReady)
	}
	return o, nil
}

// gitOpsFromMachinery judges a Flux Kustomization read through machinery.
//
// Gap: the revision check needs the Revision info field. Without it a watch
// with gitops.revision would wait forever on "Synced at -". When the
// Kustomization is Ready but carries no Revision, that is a server mapping
// problem and reported as one.
func gitOpsFromMachinery(r *rs.ResourceStatus, wantRevision string) (GitOpsObservation, error) {
	obj, err := machineryObject(r)
	if err != nil {
		return GitOpsObservation{}, err
	}
	o := parseFluxKustomization(obj, wantRevision)
	ready, _ := conditionOf(obj, "Ready")
	if _, ok := r.GetInfoFields()[infoRevision]; !ok && wantRevision != "" && ready.Status == "True" {
		return GitOpsObservation{}, fmt.Errorf(
			"machinery returns Kustomization %s/%s Ready without a %s info field: cannot compare against gitops.revision; map status.lastAppliedRevision as %s on the machinery server",
			r.GetNamespace(), r.GetName(), infoRevision, infoRevision)
	}
	return o, nil
}

func observeXRMachinery(ctx context.Context, t XRTarget) (XRObservation, error) {
	c, err := newMachineryClient(t.Machinery)
	if err != nil {
		return XRObservation{}, err
	}
	r, err := c.detail(ctx, t.Machinery.Kind, t.Namespace, t.Name)
	if err != nil || r == nil {
		return XRObservation{}, err
	}
	return xrFromMachinery(t.Machinery.Kind, r)
}

func observeGitOpsMachinery(ctx context.Context, t GitOpsTarget) (GitOpsObservation, error) {
	if t.Kind != "flux" {
		return GitOpsObservation{}, fmt.Errorf("gitops kind %q cannot be read through machinery, only flux", t.Kind)
	}
	c, err := newMachineryClient(t.Machinery)
	if err != nil {
		return GitOpsObservation{}, err
	}
	r, err := c.detail(ctx, t.Machinery.Kind, t.Namespace, t.Name)
	if err != nil || r == nil {
		return GitOpsObservation{}, err
	}
	return gitOpsFromMachinery(r, t.Revision)
}
