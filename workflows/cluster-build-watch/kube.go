package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// A deliberately small Kubernetes client: the worker GETs three kinds of
// object and server-side-applies one ConfigMap. client-go would add tens of
// megabytes and a dependency tree to the image for that.
type kubeClient struct {
	base  string
	token string
	http  *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// newKube is a variable so tests can point it at an httptest server.
var newKube = newKubeClient

// newKubeClient uses the pod's ServiceAccount. KUBE_API_SERVER overrides it for
// local runs, typically `kubectl proxy` on http://127.0.0.1:8001, which needs
// no token.
func newKubeClient() (*kubeClient, error) {
	if base := os.Getenv("KUBE_API_SERVER"); base != "" {
		return &kubeClient{
			base:  strings.TrimRight(base, "/"),
			token: os.Getenv("KUBE_TOKEN"),
			http:  &http.Client{Timeout: 20 * time.Second},
		}, nil
	}

	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, fmt.Errorf("not in a cluster and KUBE_API_SERVER is not set")
	}
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount token: %w", err)
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("serviceaccount ca.crt holds no certificate")
	}
	return &kubeClient{
		base:  "https://" + host + ":" + port,
		token: strings.TrimSpace(string(token)),
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// get returns the object, or nil without error on 404. Any other status is an
// error: a 403 means the worker's RBAC is missing a kind, and reporting that as
// "not found" would make the watch wait on an object it can never see.
func (k *kubeClient) get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, k.base+path, nil)
	if err != nil {
		return nil, err
	}
	k.auth(req)
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		return obj, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 300))
	}
}

// apply server-side-applies obj at path: create when missing, update when not,
// in one idempotent call. JSON is valid YAML, so the apply content type takes
// it as is.
func (k *kubeClient) apply(path string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch,
		k.base+path+"?fieldManager=cluster-build-watch&force=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	k.auth(req)
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("apply %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 300))
	}
	return nil
}

func (k *kubeClient) auth(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
}

// resourcePath builds the REST path of a namespaced object.
func resourcePath(apiVersion, resource, namespace, name string) string {
	prefix := "/apis/" + apiVersion
	if !strings.Contains(apiVersion, "/") {
		prefix = "/api/" + apiVersion // core group, e.g. "v1"
	}
	return fmt.Sprintf("%s/namespaces/%s/%s/%s", prefix,
		url.PathEscape(namespace), resource, url.PathEscape(name))
}

// ─────────────────────────────────────────────────────────────────────────────
// PARSING — pure functions over the unstructured object
// ─────────────────────────────────────────────────────────────────────────────

func str(obj map[string]any, path ...string) string {
	s, _ := dig(obj, path...).(string)
	return s
}

func dig(obj map[string]any, path ...string) any {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

type condition struct {
	Status, Reason, Message string
}

func conditionOf(obj map[string]any, typ string) (condition, bool) {
	list, _ := dig(obj, "status", "conditions").([]any)
	for _, c := range list {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != typ {
			continue
		}
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		return condition{s, r, msg}, true
	}
	return condition{}, false
}

// revisionMatches accepts a full SHA, a prefix of one, and Flux's
// "<ref>@sha1:<sha>" form.
func revisionMatches(have, want string) bool {
	if want == "" {
		return true
	}
	if have == "" {
		return false
	}
	if i := strings.LastIndex(have, ":"); i >= 0 {
		have = have[i+1:]
	}
	return strings.HasPrefix(have, want)
}

// parseArgoApplication judges an argoproj.io Application.
//
// Synced means: sync.status Synced, at the wanted revision, and no sync
// operation that ended Failed/Error. Health is reported but NOT required --
// an Application that carries the XR stays Progressing for as long as the
// cluster builds, if Argo has a health check for the kind, and the build is
// what the stages after this one watch.
func parseArgoApplication(obj map[string]any, wantRevision string) GitOpsObservation {
	o := GitOpsObservation{Found: true}
	o.Revision = str(obj, "status", "sync", "revision")
	revs, _ := dig(obj, "status", "sync", "revisions").([]any) // multi-source
	matched := revisionMatches(o.Revision, wantRevision)
	for _, r := range revs {
		if s, _ := r.(string); s != "" && !matched && revisionMatches(s, wantRevision) {
			matched, o.Revision = true, s
		}
	}
	if o.Revision == "" && len(revs) > 0 {
		o.Revision, _ = revs[0].(string)
	}
	o.Health = str(obj, "status", "health", "status")

	phase := str(obj, "status", "operationState", "phase")
	opFailed := phase == "Failed" || phase == "Error"
	o.Synced = str(obj, "status", "sync", "status") == "Synced" && matched && !opFailed
	o.Degraded = opFailed || o.Health == "Degraded"
	switch {
	case opFailed:
		o.Message = "sync " + phase + ": " + str(obj, "status", "operationState", "message")
	case o.Health == "Degraded":
		o.Message = "health Degraded: " + str(obj, "status", "health", "message")
	}
	return o
}

// parseFluxKustomization judges a kustomize.toolkit.fluxcd.io Kustomization:
// Ready=True and lastAppliedRevision at the wanted commit.
func parseFluxKustomization(obj map[string]any, wantRevision string) GitOpsObservation {
	o := GitOpsObservation{Found: true}
	o.Revision = str(obj, "status", "lastAppliedRevision")
	ready, ok := conditionOf(obj, "Ready")
	o.Health = ready.Status
	o.Synced = ok && ready.Status == "True" && revisionMatches(o.Revision, wantRevision)
	// Ready=False with a failure reason is a degraded apply; Unknown is a
	// reconcile in progress and not worth a notification.
	o.Degraded = ok && ready.Status == "False" && ready.Reason != "Progressing" && ready.Reason != "DependencyNotReady"
	if ok && ready.Status != "True" && ready.Reason != "" {
		o.Message = truncate(ready.Reason+": "+ready.Message, 300)
		// Health is what the waiting message and a stuck notification show,
		// so the reason goes there too. Message alone would surface only on a
		// degraded transition, and DependencyNotReady -- the usual reason a
		// Kustomization hangs (e.g. dependsOn machinery-fleet-state) -- is
		// deliberately not one.
		o.Health = ready.Status + ", " + o.Message
	}
	return o
}

// parseXR reads a Crossplane composite resource.
func parseXR(obj map[string]any) XRObservation {
	o := XRObservation{Found: true}
	o.Stage = str(obj, "status", "stage")
	if b, ok := dig(obj, "status", "ready").(bool); ok {
		o.Ready = &b
	}
	if c, ok := conditionOf(obj, "Ready"); ok {
		o.ReadyCondition = c.Status == "True"
		if !o.ReadyCondition && c.Message != "" {
			o.Message = c.Reason + ": " + c.Message
		}
	}
	if c, ok := conditionOf(obj, "Synced"); ok && c.Status == "False" {
		o.SyncedFalse = true
		o.Message = c.Reason + ": " + c.Message
	}
	o.Message = truncate(o.Message, 500)
	return o
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
