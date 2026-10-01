package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/workflow"
)

// ReportInput is one checkpoint plus the snapshot the status ConfigMap shows.
type ReportInput struct {
	InstanceID string     `json:"instanceID"`
	Target     XRTarget   `json:"target"`
	Event      Event      `json:"event"`
	State      WatchState `json:"state"`
}

// The three sinks, each switched on by its environment, each independent: a
// Teams outage must not stop the status ConfigMap or the webhook.
//
//	STATUS_NAMESPACE    namespace for the status ConfigMap; defaults to the
//	                    worker's own, read from the ServiceAccount mount
//	TEAMS_WEBHOOK_URL   Teams incoming webhook (Workflows / Power Automate)
//	STATUS_WEBHOOK_URL  any HTTP endpoint; receives a CloudEvent per checkpoint
var httpClient = &http.Client{Timeout: 15 * time.Second}

// Report delivers one checkpoint to every configured sink.
//
// Errors are collected rather than returned at the first one, so every sink
// gets its attempt. The workflow treats a failed report as a warning: losing a
// notification is no reason to stop watching the build.
func Report(ctx workflow.ActivityContext) (any, error) {
	var in ReportInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}

	var errs []error
	sent := []string{}

	if ns := statusNamespace(); ns != "" {
		if err := writeStatusConfigMap(ns, in); err != nil {
			errs = append(errs, fmt.Errorf("status configmap: %w", err))
		} else {
			sent = append(sent, "configmap")
		}
	}
	if u := os.Getenv("TEAMS_WEBHOOK_URL"); u != "" {
		if err := postJSON(u, "application/json", teamsMessage(in)); err != nil {
			errs = append(errs, fmt.Errorf("teams: %w", err))
		} else {
			sent = append(sent, "teams")
		}
	}
	if u := os.Getenv("STATUS_WEBHOOK_URL"); u != "" {
		if err := postJSON(u, "application/cloudevents+json", cloudEvent(in)); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		} else {
			sent = append(sent, "webhook")
		}
	}
	return sent, errors.Join(errs...)
}

func statusNamespace() string {
	if ns := os.Getenv("STATUS_NAMESPACE"); ns != "" {
		return ns
	}
	b, err := os.ReadFile(saDir + "/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// statusConfigMapName derives the name from the instance ID, which is unique
// per watch: a ClusterBuildWatch starts <namespace>_<name>. The `_` becomes a
// `.`, and since namespaces cannot contain dots, two CRs cannot collide. The
// display name is NOT used -- it is free text.
func statusConfigMapName(instanceID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(instanceID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		case r == '_':
			b.WriteRune('.')
		default:
			b.WriteRune('-')
		}
	}
	name := "cluster-build-watch." + strings.Trim(b.String(), ".-")
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], ".-")
	}
	return name
}

type stageRow struct {
	Stage    string `json:"stage"`
	Start    string `json:"start"`
	Duration string `json:"duration,omitempty"`
}

func stageRows(st []StageRecord) []stageRow {
	rows := make([]stageRow, 0, len(st))
	for _, s := range st {
		r := stageRow{Stage: s.Stage, Start: s.Start.UTC().Format(time.RFC3339)}
		if !s.End.IsZero() {
			r.Duration = s.Duration().Round(time.Second).String()
		}
		rows = append(rows, r)
	}
	return rows
}

func writeStatusConfigMap(ns string, in ReportInput) error {
	k, err := newKube()
	if err != nil {
		return err
	}
	stages, _ := json.Marshal(stageRows(in.State.Stages))
	event, _ := json.Marshal(in.Event)
	name := statusConfigMapName(in.InstanceID)
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by":         "cluster-build-watch",
				"cluster-build-watch.sthings.io/phase": in.State.Phase,
			},
		},
		"data": map[string]any{
			"instanceID": in.InstanceID,
			"name":       in.Event.Name,
			"target":     fmt.Sprintf("%s %s/%s", in.Target.Resource, in.Target.Namespace, in.Target.Name),
			"phase":      in.State.Phase,
			"stage":      in.State.Stage,
			"message":    in.State.Message,
			"startedAt":  in.State.StartedAt.UTC().Format(time.RFC3339),
			"updatedAt":  in.Event.At.UTC().Format(time.RFC3339),
			"lastEvent":  string(event),
			"stages":     string(stages),
		},
	}
	return k.apply(resourcePath("v1", "configmaps", ns, name), cm)
}

// cloudEvent wraps the checkpoint in a structured CloudEvent. The id is stable
// per checkpoint, so a receiver can drop the duplicate an activity retry sends.
func cloudEvent(in ReportInput) map[string]any {
	return map[string]any{
		"specversion":     "1.0",
		"type":            "io.sthings.clusterbuild." + in.Event.Type,
		"source":          "dapr/cluster-build-watch",
		"subject":         fmt.Sprintf("%s/%s/%s", in.Target.Resource, in.Target.Namespace, in.Target.Name),
		"id":              fmt.Sprintf("%s-%d", in.InstanceID, in.Event.Seq),
		"time":            in.Event.At.UTC().Format(time.RFC3339),
		"datacontenttype": "application/json",
		"data": map[string]any{
			"instanceID": in.InstanceID,
			"name":       in.Event.Name,
			"event":      in.Event.Type,
			"severity":   in.Event.Severity,
			"phase":      in.State.Phase,
			"stage":      in.Event.Stage,
			"message":    in.Event.Message,
			"elapsed":    in.Event.Elapsed,
			"stages":     stageRows(in.Event.Stages),
		},
	}
}

var severityStyle = map[string]struct{ icon, color string }{
	sevInfo:    {"🔵", "Accent"},
	sevSuccess: {"✅", "Good"},
	sevWarning: {"⚠️", "Warning"},
	sevError:   {"❌", "Attention"},
}

// teamsMessage builds an Adaptive Card, the format the Teams "Workflows"
// webhooks accept. The retired Office 365 connector MessageCard is not used.
func teamsMessage(in ReportInput) map[string]any {
	style, ok := severityStyle[in.Event.Severity]
	if !ok {
		style = severityStyle[sevInfo]
	}
	facts := []map[string]string{
		{"title": "Stage", "value": orDash(in.Event.Stage)},
		{"title": "Phase", "value": in.State.Phase},
		{"title": "Elapsed", "value": in.Event.Elapsed},
		{"title": "Target", "value": fmt.Sprintf("%s %s/%s", in.Target.Resource, in.Target.Namespace, in.Target.Name)},
	}
	body := []any{
		map[string]any{
			"type": "TextBlock", "size": "Medium", "weight": "Bolder", "wrap": true,
			"color": style.color,
			"text":  fmt.Sprintf("%s %s: %s", style.icon, in.Event.Name, in.Event.Type),
		},
		map[string]any{"type": "TextBlock", "wrap": true, "text": in.Event.Message},
		map[string]any{"type": "FactSet", "facts": facts},
	}
	if len(in.Event.Stages) > 0 {
		var sb strings.Builder
		for _, r := range stageRows(in.Event.Stages) {
			fmt.Fprintf(&sb, "- **%s** %s\n", r.Stage, orDash(r.Duration))
		}
		body = append(body, map[string]any{"type": "TextBlock", "wrap": true, "text": sb.String()})
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard",
				"version": "1.4",
				"body":    body,
			},
		}},
	}
}

func postJSON(u, contentType string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST failed: %w", redactURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// redactURL drops the URL from a transport error. A webhook URL carries its
// credential (Teams puts the signature in the query string), and net/http
// writes the full URL into every *url.Error -- from where it would reach the
// workflow history, the custom status and the logs.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s <webhook>: %w", ue.Op, ue.Err)
	}
	return err
}
