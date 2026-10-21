package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/workflow"
)

// ─────────────────────────────────────────────────────────────────────────────
// SINK — homerun2 omni-pitcher (dapr-workflows#55)
// ─────────────────────────────────────────────────────────────────────────────
//
// Every checkpoint of a run (started, build started, build failed/green,
// merged, merge refused, timed out) goes to homerun2's omni-pitcher as one
// homerun.Message; notification-catcher routes it to Teams on
// system=backstage-template-execution.
//
// Same env contract as cluster-build-watch's sink (report.go there):
//
//	HOMERUN_PITCH_URL         omni-pitcher, e.g. https://<host>/pitch; empty = off
//	HOMERUN_AUTH_TOKEN_FILE   file holding omni-pitcher's bearer token; wins over
//	HOMERUN_AUTH_TOKEN        the token itself
//
// A COPY, NOT A SHARED PACKAGE. The two workflows are separate Go modules,
// built and released one by one; a third module for forty lines would put both
// releases behind it. Keep the two in step by hand: same struct, same token
// and https rules.
//
// BEST EFFORT. A homerun2 outage must never fail or hold up a VM build: the
// activity is retried a bounded number of times, and its error is dropped by
// the workflow (see pitch). The activity is ALWAYS scheduled and decides
// itself whether the sink is on -- reading the env in workflow code would make
// the history depend on it, and switching the sink on while a run is in flight
// would then fail that run's replay.

// homerunMessage is homerun-library's homerun.Message, the body omni-pitcher's
// POST /pitch decodes. Twin of cluster-build-watch's; the JSON names must stay
// those of homerun.Message. omni-pitcher requires title and message.
type homerunMessage struct {
	Title     string `json:"title"`
	Message   string `json:"message"`
	Severity  string `json:"severity,omitempty"`
	Author    string `json:"author,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	System    string `json:"system,omitempty"`
	Tags      string `json:"tags,omitempty"`
	URL       string `json:"url,omitempty"`
}

// homerunSystem is both system and author. notification-catcher routes on it;
// keep it stable.
const homerunSystem = "backstage-template-execution"

// PitchInput is one checkpoint, built in the workflow (deterministic: the
// timestamp is the orchestration clock) and delivered by the Pitch activity.
type PitchInput struct {
	Message homerunMessage `json:"message"`
}

var homerunClient = &http.Client{Timeout: 15 * time.Second}

// pitchRetry bounds the delivery: three attempts over about half a minute,
// then the checkpoint is given up.
var pitchRetry = &workflow.RetryPolicy{
	MaxAttempts:          3,
	InitialRetryInterval: 10 * time.Second,
	BackoffCoefficient:   2,
}

// checkpoint is what the workflow knows at one point of a run.
type checkpoint struct {
	Event    string // started | build-started | build-failed | build-succeeded | merged | merge-refused | scaffolder-failed | watch-failed | timeout
	Severity string // info | success | warning | error
	Detail   string // one line: what happened
	PRNumber int
	PRURL    string
	RunURL   string
	TaskURL  string // Backstage task log
}

// pitch hands one checkpoint to the Pitch activity and ignores the outcome.
// It has no error return on purpose: no caller can let a lost Teams card
// change the run's result.
func pitch(ctx *workflow.WorkflowContext, in Input, cp checkpoint) {
	if in.DryRun {
		return // a rendering check, not a run anybody waits for
	}
	pi := PitchInput{Message: buildPitch(in, cp, ctx.CurrentTimeUTC())}
	_ = ctx.CallActivity(Pitch, workflow.WithActivityInput(pi),
		workflow.WithActivityRetryPolicy(pitchRetry)).Await(nil)
}

// Pitch POSTs one message to omni-pitcher, or does nothing when the sink is
// off. Its error only drives the retry; the workflow drops it.
func Pitch(ctx workflow.ActivityContext) (any, error) {
	var in PitchInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	return deliverPitch(in.Message)
}

// deliverPitch is the activity's body: "off" when HOMERUN_PITCH_URL is
// empty, "sent" on a 2xx, an error otherwise.
func deliverPitch(msg homerunMessage) (string, error) {
	u := os.Getenv("HOMERUN_PITCH_URL")
	if u == "" {
		return "off", nil
	}
	if err := pitchHomerun(u, msg); err != nil {
		slog.Warn("homerun pitch failed", "title", msg.Title, "err", err)
		return "", err
	}
	slog.Info("homerun pitch sent", "title", msg.Title, "severity", msg.Severity)
	return "sent", nil
}

// runSubject names what a run is about, for the title: "VM cutover2" for
// create-terraform-vm, "VM cutover2 deletion" for delete-terraform-vm, the
// template's name for anything else.
func runSubject(in Input) string {
	name := ""
	for _, k := range []string{"vm_name", "vmname"} {
		if v, ok := in.Values[k]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				name = s
				break
			}
		}
	}
	tmpl := in.TemplateRef
	if i := strings.LastIndex(tmpl, "/"); i >= 0 {
		tmpl = tmpl[i+1:]
	}
	if name == "" {
		return tmpl
	}
	if strings.HasPrefix(tmpl, "delete-") {
		return "VM " + name + " deletion"
	}
	return "VM " + name
}

// buildPitch maps one checkpoint onto a homerun.Message.
func buildPitch(in Input, cp checkpoint, at time.Time) homerunMessage {
	title := fmt.Sprintf("%s: %s", runSubject(in), cp.Event)
	if cp.PRNumber > 0 {
		title += fmt.Sprintf(" (#%d)", cp.PRNumber)
	}

	lines := []string{}
	if cp.Detail != "" {
		lines = append(lines, cp.Detail)
	}
	lines = append(lines, "Template: "+in.TemplateRef)
	var vals []string
	for _, k := range []string{"lab", "cloud", "vm_sizing", "provisioning_profile"} {
		if v, ok := in.Values[k]; ok && fmt.Sprint(v) != "" {
			vals = append(vals, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(vals) > 0 {
		lines = append(lines, "Values: "+strings.Join(vals, ", "))
	}
	if cp.PRURL != "" {
		lines = append(lines, "PR: "+cp.PRURL)
	}
	if cp.RunURL != "" {
		lines = append(lines, "Run: "+cp.RunURL)
	}
	if cp.TaskURL != "" {
		lines = append(lines, "Backstage task: "+cp.TaskURL)
	}

	// The link of the card: the PR once there is one, else the run, else the
	// Backstage task.
	link := cp.PRURL
	if link == "" {
		link = cp.RunURL
	}
	if link == "" {
		link = cp.TaskURL
	}

	tmpl := in.TemplateRef
	if i := strings.LastIndex(tmpl, "/"); i >= 0 {
		tmpl = tmpl[i+1:]
	}
	return homerunMessage{
		Title:     title,
		Message:   strings.Join(lines, "\n"),
		Severity:  cp.Severity,
		Author:    homerunSystem,
		Timestamp: at.UTC().Format(time.RFC3339),
		System:    homerunSystem,
		Tags:      strings.Join([]string{"backstage-template-run", cp.Event, tmpl}, ","),
		URL:       link,
	}
}

// prURL is the PR's address, built from the watch (the run only carries the
// number).
func prURL(w *GitHubWatch, number int) string {
	if w == nil || number <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", w.Owner, w.Repo, number)
}

// homerunToken is read on every pitch, so a rotated Secret takes effect
// without a restart. The file wins; an unreadable file is an error, not "no
// auth".
func homerunToken() (string, error) {
	if f := os.Getenv("HOMERUN_AUTH_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read HOMERUN_AUTH_TOKEN_FILE: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv("HOMERUN_AUTH_TOKEN")), nil
}

// pitchHomerun POSTs one message with Authorization: Bearer <token>. A token
// is only ever sent over https; without a token plain http is allowed (an
// in-cluster pitcher without auth). The token is scrubbed from every error,
// and the URL from transport errors.
func pitchHomerun(u string, msg homerunMessage) error {
	token, err := homerunToken()
	if err != nil {
		return err
	}
	pu, perr := url.Parse(u)
	if perr != nil || pu.Host == "" {
		return errors.New("HOMERUN_PITCH_URL is not a valid URL")
	}
	if token != "" && !strings.EqualFold(pu.Scheme, "https") {
		return fmt.Errorf("refusing to send the auth token over %q: HOMERUN_PITCH_URL must be https", pu.Scheme)
	}
	err = postPitch(u, token, msg)
	if err != nil && token != "" && strings.Contains(err.Error(), token) {
		return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
	}
	return err
}

func postPitch(u, token string, msg homerunMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("HOMERUN_PITCH_URL is not a valid URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := homerunClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("POST <homerun>: %w", ue.Err)
		}
		return fmt.Errorf("POST <homerun>: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
