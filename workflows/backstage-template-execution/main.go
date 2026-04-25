package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	dapr "github.com/dapr/go-sdk/client"
)

var backstageClient = func() *http.Client {
	if os.Getenv("BACKSTAGE_INSECURE_TLS") == "true" {
		return &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}
	return http.DefaultClient
}()

// ─────────────────────────────────────────────────────────────────────────────
// INPUT
// ─────────────────────────────────────────────────────────────────────────────

type Input struct {
	BackstageURL string  `json:"backstageURL"`
	AuthToken    string  `json:"authToken"`
	Stages       []Stage `json:"stages"`
}

type Stage struct {
	Name        string                 `json:"name"`
	TemplateRef string                 `json:"templateRef"`
	Values      map[string]interface{} `json:"values"`
	DryRun      bool                   `json:"dryRun"`
	Watch       *StageWatch            `json:"watch,omitempty"`
	// Exports maps an output key (under this stage's namespace) to a source
	// expression. Currently only the artifact transport is planned:
	//   "<artifactName>.<jsonKey>"  (artifact must contain outputs.json with a flat string map)
	// The activity that resolves these is not implemented yet — see issue #21.
	Exports map[string]string `json:"exports,omitempty"`
}

type StageWatch struct {
	Kind         string       `json:"kind"` // "pr" | "dispatch"
	Owner        string       `json:"owner"`
	Repo         string       `json:"repo"`
	WorkflowFile string       `json:"workflowFile"`
	Branch       string       `json:"branch,omitempty"` // only meaningful for kind=pr
	TimeoutMin   int          `json:"timeoutMin"`
	Merge        *MergeConfig `json:"merge,omitempty"` // only honored for kind=pr
}

type MergeConfig struct {
	Enabled bool   `json:"enabled"`
	Method  string `json:"method"` // squash | merge | rebase
}

// ─────────────────────────────────────────────────────────────────────────────
// WORKFLOW
// ─────────────────────────────────────────────────────────────────────────────

func BackstageTemplateWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var in Input
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	if len(in.Stages) == 0 {
		return nil, fmt.Errorf("input.stages is empty")
	}

	exports := map[string]string{} // "<stageName>.<key>" -> value
	var results []StageResult

	for i, stage := range in.Stages {
		if stage.Name == "" {
			stage.Name = fmt.Sprintf("stage%d", i)
		}
		label := fmt.Sprintf("[%d/%d %s]", i+1, len(in.Stages), stage.Name)

		resolved, err := resolveValues(stage.Values, exports)
		if err != nil {
			return results, fmt.Errorf("%s resolve values: %w", label, err)
		}
		stage.Values = resolved

		ctx.SetCustomStatus(label + " calling scaffolder")

		scaffIn := ScaffolderInput{
			BackstageURL: in.BackstageURL,
			AuthToken:    in.AuthToken,
			TemplateRef:  stage.TemplateRef,
			Values:       stage.Values,
			DryRun:       stage.DryRun,
		}
		var scaffOut ScaffolderResult
		if err := ctx.CallActivity(CallScaffolder, workflow.WithActivityInput(scaffIn)).Await(&scaffOut); err != nil {
			return results, fmt.Errorf("%s scaffolder: %w", label, err)
		}

		result := StageResult{
			Name:   stage.Name,
			TaskID: scaffOut.TaskID,
			LogURL: scaffOut.LogURL,
			DryRun: scaffOut.DryRun,
		}

		if stage.DryRun {
			ctx.SetCustomStatus(label + " dry-run done")
			results = append(results, result)
			continue
		}

		// Poll Backstage task until terminal.
		ctx.SetCustomStatus(fmt.Sprintf("%s polling task %s", label, scaffOut.TaskID))
		taskDone := false
		for j := range 40 {
			if err := ctx.CreateTimer(5 * time.Second).Await(nil); err != nil {
				return results, err
			}
			pollIn := PollInput{
				BackstageURL: in.BackstageURL,
				TaskID:       scaffOut.TaskID,
				AuthToken:    in.AuthToken,
			}
			var status TaskStatus
			if err := ctx.CallActivity(PollTask, workflow.WithActivityInput(pollIn)).Await(&status); err != nil {
				return results, err
			}
			ctx.SetCustomStatus(fmt.Sprintf("%s task %s: %s [%d/40]", label, scaffOut.TaskID, status.Status, j+1))
			if status.Status == "completed" {
				taskDone = true
				break
			}
			if status.Status == "failed" || status.Status == "cancelled" {
				return results, fmt.Errorf("%s task %s → %s (step %s)", label, scaffOut.TaskID, status.Status, status.FailedStep)
			}
		}
		if !taskDone {
			return results, fmt.Errorf("%s timed out polling task %s", label, scaffOut.TaskID)
		}
		result.TaskStatus = "completed"

		if stage.Watch != nil {
			ghRun, err := watchGHRun(ctx, label, stage, scaffOut.DispatchedAt)
			result.GitHubRun = ghRun
			if err != nil {
				results = append(results, result)
				return results, fmt.Errorf("%s %w", label, err)
			}

			if stage.Watch.Kind == "pr" && stage.Watch.Merge != nil && stage.Watch.Merge.Enabled {
				ctx.SetCustomStatus(fmt.Sprintf("%s merging PR for %s", label, stage.Watch.Branch))
				method := stage.Watch.Merge.Method
				if method == "" {
					method = "squash"
				}
				mergeIn := MergeInput{
					Owner:  stage.Watch.Owner,
					Repo:   stage.Watch.Repo,
					Branch: stage.Watch.Branch,
					Method: method,
				}
				var merged MergeResult
				if err := ctx.CallActivity(MergePullRequest, workflow.WithActivityInput(mergeIn)).Await(&merged); err != nil {
					results = append(results, result)
					return results, fmt.Errorf("%s merge: %w", label, err)
				}
				result.Merge = &merged
				ctx.SetCustomStatus(fmt.Sprintf("%s merged PR #%d (%s)", label, merged.PRNumber, merged.SHA))
			}

			if len(stage.Exports) > 0 {
				expIn := FetchExportsInput{
					Owner:   stage.Watch.Owner,
					Repo:    stage.Watch.Repo,
					RunID:   ghRun.ID,
					Exports: stage.Exports,
				}
				var resolved map[string]string
				if err := ctx.CallActivity(FetchStageExports, workflow.WithActivityInput(expIn)).Await(&resolved); err != nil {
					results = append(results, result)
					return results, fmt.Errorf("%s exports: %w", label, err)
				}
				for k, v := range resolved {
					exports[stage.Name+"."+k] = v
				}
				result.Exports = resolved
			}
		}

		results = append(results, result)
	}

	return results, nil
}

// watchGHRun polls FetchGitHubRun until the run reaches a terminal state.
// Returns the final status (non-nil if a run was ever found) and an error
// if the run failed or polling timed out.
func watchGHRun(ctx *workflow.WorkflowContext, label string, stage Stage, dispatchedAt string) (*GitHubRunStatus, error) {
	timeoutMin := stage.Watch.TimeoutMin
	if timeoutMin <= 0 {
		timeoutMin = 30
	}
	maxIters := (timeoutMin * 60) / 10
	ctx.SetCustomStatus(fmt.Sprintf("%s waiting for %s (%s)", label, stage.Watch.WorkflowFile, stage.Watch.Kind))

	fetchIn := FetchRunInput{
		Kind:         stage.Watch.Kind,
		Owner:        stage.Watch.Owner,
		Repo:         stage.Watch.Repo,
		WorkflowFile: stage.Watch.WorkflowFile,
		Branch:       stage.Watch.Branch,
		DispatchedAt: dispatchedAt,
	}

	var ghRun GitHubRunStatus
	for j := range maxIters {
		if err := ctx.CreateTimer(10 * time.Second).Await(nil); err != nil {
			return nil, err
		}
		if err := ctx.CallActivity(FetchGitHubRun, workflow.WithActivityInput(fetchIn)).Await(&ghRun); err != nil {
			return nil, fmt.Errorf("fetch gh run: %w", err)
		}
		ctx.SetCustomStatus(fmt.Sprintf("%s gh run %d: %s/%s [%d/%d]", label, ghRun.ID, ghRun.Status, ghRun.Conclusion, j+1, maxIters))
		if ghRun.Status == "completed" {
			if ghRun.Conclusion != "success" {
				return &ghRun, fmt.Errorf("gh workflow %s → %s (%s)", stage.Watch.WorkflowFile, ghRun.Conclusion, ghRun.HTMLURL)
			}
			return &ghRun, nil
		}
	}
	return &ghRun, fmt.Errorf("timed out waiting for gh workflow %s", stage.Watch.WorkflowFile)
}

// resolveValues recursively walks v and replaces strings of the form
// "${stages.<stageName>.<key>}" with the corresponding entry in exports.
// Any unmatched placeholder returns an error.
func resolveValues(v map[string]interface{}, exports map[string]string) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(v))
	for k, val := range v {
		resolved, err := resolveAny(val, exports)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

func resolveAny(v interface{}, exports map[string]string) (interface{}, error) {
	switch x := v.(type) {
	case string:
		return resolveString(x, exports)
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, el := range x {
			r, err := resolveAny(el, exports)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	case map[string]interface{}:
		return resolveValues(x, exports)
	default:
		return v, nil
	}
}

func resolveString(s string, exports map[string]string) (string, error) {
	const open, close = "${stages.", "}"
	out := s
	for {
		i := strings.Index(out, open)
		if i < 0 {
			return out, nil
		}
		j := strings.Index(out[i:], close)
		if j < 0 {
			return "", fmt.Errorf("unterminated placeholder in %q", s)
		}
		key := out[i+len(open) : i+j]
		val, ok := exports[key]
		if !ok {
			return "", fmt.Errorf("unknown export %q (have: %v)", key, mapKeys(exports))
		}
		out = out[:i] + val + out[i+j+len(close):]
	}
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ─────────────────────────────────────────────────────────────────────────────
// ACTIVITIES
// ─────────────────────────────────────────────────────────────────────────────

type ScaffolderInput struct {
	BackstageURL string                 `json:"backstageURL"`
	AuthToken    string                 `json:"authToken"`
	TemplateRef  string                 `json:"templateRef"`
	Values       map[string]interface{} `json:"values"`
	DryRun       bool                   `json:"dryRun"`
}

type ScaffolderResult struct {
	TaskID       string `json:"taskId"`
	LogURL       string `json:"logUrl"`
	DryRun       bool   `json:"dryRun"`
	DispatchedAt string `json:"dispatchedAt"` // RFC3339, used by dispatch-kind correlation
}

type StageResult struct {
	Name       string            `json:"name"`
	TaskID     string            `json:"taskId"`
	LogURL     string            `json:"logUrl"`
	DryRun     bool              `json:"dryRun"`
	TaskStatus string            `json:"taskStatus,omitempty"`
	GitHubRun  *GitHubRunStatus  `json:"githubRun,omitempty"`
	Merge      *MergeResult      `json:"merge,omitempty"`
	Exports    map[string]string `json:"exports,omitempty"`
}

type MergeInput struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Method string `json:"method"`
}

type MergeResult struct {
	PRNumber int    `json:"prNumber"`
	SHA      string `json:"sha"`
	Merged   bool   `json:"merged"`
	HTMLURL  string `json:"htmlUrl"`
}

type GitHubRunStatus struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"htmlUrl"`
	HeadBranch string `json:"headBranch"`
}

type PollInput struct {
	BackstageURL string `json:"backstageURL"`
	TaskID       string `json:"taskId"`
	AuthToken    string `json:"authToken"`
}

type TaskStatus struct {
	Status     string `json:"status"`
	FailedStep string `json:"failedStep"`
}

type FetchRunInput struct {
	Kind         string `json:"kind"`
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	WorkflowFile string `json:"workflowFile"`
	Branch       string `json:"branch"`
	DispatchedAt string `json:"dispatchedAt"`
}

type FetchExportsInput struct {
	Owner   string            `json:"owner"`
	Repo    string            `json:"repo"`
	RunID   int64             `json:"runId"`
	Exports map[string]string `json:"exports"`
}

func CallScaffolder(ctx workflow.ActivityContext) (any, error) {
	var in ScaffolderInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	if in.AuthToken == "" {
		in.AuthToken = os.Getenv("BACKSTAGE_AUTH_TOKEN")
	}

	dispatchedAt := time.Now().UTC().Format(time.RFC3339)

	apiURL := in.BackstageURL + "/api/scaffolder/v2/tasks"
	payload := map[string]interface{}{
		"templateRef": in.TemplateRef,
		"values":      in.Values,
	}
	if in.DryRun {
		tmpl, err := fetchTemplateEntity(in.BackstageURL, in.TemplateRef, in.AuthToken)
		if err != nil {
			return nil, fmt.Errorf("fetch template: %w", err)
		}
		apiURL = in.BackstageURL + "/api/scaffolder/v2/dry-run"
		payload = map[string]interface{}{
			"template":          tmpl,
			"values":            in.Values,
			"secrets":           map[string]string{},
			"directoryContents": []interface{}{},
		}
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if in.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+in.AuthToken)
	}

	resp, err := backstageClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}

	if in.DryRun {
		slog.Info("scaffolder dry-run completed", "bytes", len(raw))
		return &ScaffolderResult{TaskID: "dry-run", DryRun: true, DispatchedAt: dispatchedAt}, nil
	}

	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.ID == "" {
		return nil, fmt.Errorf("unexpected response: %s", raw)
	}

	slog.Info("scaffolder task created",
		"taskId", parsed.ID,
		"logUrl", fmt.Sprintf("%s/create/tasks/%s", in.BackstageURL, parsed.ID),
	)

	return &ScaffolderResult{
		TaskID:       parsed.ID,
		LogURL:       fmt.Sprintf("%s/create/tasks/%s", in.BackstageURL, parsed.ID),
		DispatchedAt: dispatchedAt,
	}, nil
}

func fetchTemplateEntity(baseURL, ref, token string) (map[string]interface{}, error) {
	kindRest := strings.SplitN(ref, ":", 2)
	if len(kindRest) != 2 {
		return nil, fmt.Errorf("invalid templateRef %q", ref)
	}
	kind := kindRest[0]
	nsName := strings.SplitN(kindRest[1], "/", 2)
	if len(nsName) != 2 {
		return nil, fmt.Errorf("invalid templateRef %q", ref)
	}
	apiURL := fmt.Sprintf("%s/api/catalog/entities/by-name/%s/%s/%s", baseURL, kind, nsName[0], nsName[1])

	req, _ := http.NewRequest(http.MethodGet, apiURL, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := backstageClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("catalog HTTP %d: %s", resp.StatusCode, raw)
	}
	var entity map[string]interface{}
	if err := json.Unmarshal(raw, &entity); err != nil {
		return nil, err
	}
	return entity, nil
}

func PollTask(ctx workflow.ActivityContext) (any, error) {
	var in PollInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	if in.AuthToken == "" {
		in.AuthToken = os.Getenv("BACKSTAGE_AUTH_TOKEN")
	}

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/scaffolder/v2/tasks/%s", in.BackstageURL, in.TaskID), nil)
	if in.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+in.AuthToken)
	}

	resp, err := backstageClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("poll HTTP %d: %s", resp.StatusCode, raw)
	}

	var parsed struct {
		Status string `json:"status"`
		Steps  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	ts := &TaskStatus{Status: parsed.Status}
	for _, s := range parsed.Steps {
		if s.Status == "failed" {
			ts.FailedStep = s.Name
			break
		}
	}

	slog.Info("task status", "taskId", in.TaskID, "status", ts.Status)
	return ts, nil
}

// FetchGitHubRun returns the most recent matching workflow run for the watch.
// For kind=pr (default) it filters by head branch and skips initial "skipped"
// runs from the label-driven re-trigger pattern.
// For kind=dispatch it filters by event=workflow_dispatch and picks the newest
// run created at or after DispatchedAt (with a 30s clock-skew buffer).
func FetchGitHubRun(ctx workflow.ActivityContext) (any, error) {
	var w FetchRunInput
	if err := ctx.GetInput(&w); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN env var not set in worker process")
	}

	var apiURL string
	switch w.Kind {
	case "", "pr":
		apiURL = fmt.Sprintf(
			"https://api.github.com/repos/%s/%s/actions/workflows/%s/runs?branch=%s&per_page=5",
			w.Owner, w.Repo, w.WorkflowFile, url.QueryEscape(w.Branch),
		)
	case "dispatch":
		apiURL = fmt.Sprintf(
			"https://api.github.com/repos/%s/%s/actions/workflows/%s/runs?event=workflow_dispatch&per_page=10",
			w.Owner, w.Repo, w.WorkflowFile,
		)
	default:
		return nil, fmt.Errorf("unknown watch kind %q", w.Kind)
	}

	req, _ := http.NewRequest(http.MethodGet, apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github HTTP %d: %s", resp.StatusCode, raw)
	}

	var parsed struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
			HeadBranch string `json:"head_branch"`
			Event      string `json:"event"`
			CreatedAt  string `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	var dispatchedAfter time.Time
	if w.Kind == "dispatch" && w.DispatchedAt != "" {
		if t, err := time.Parse(time.RFC3339, w.DispatchedAt); err == nil {
			dispatchedAfter = t.Add(-30 * time.Second)
		}
	}

	for i := range parsed.WorkflowRuns {
		r := parsed.WorkflowRuns[i]
		switch w.Kind {
		case "", "pr":
			// pr-vm-deploy.yaml fires on PR open before labels are applied —
			// the first run is skipped; the real run comes after the label
			// workflow re-triggers it.
			if r.Status == "completed" && r.Conclusion == "skipped" {
				continue
			}
		case "dispatch":
			if r.Event != "workflow_dispatch" {
				continue
			}
			if !dispatchedAfter.IsZero() {
				if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil && t.Before(dispatchedAfter) {
					continue
				}
			}
		}
		out := &GitHubRunStatus{
			ID:         r.ID,
			Status:     r.Status,
			Conclusion: r.Conclusion,
			HTMLURL:    r.HTMLURL,
			HeadBranch: r.HeadBranch,
		}
		slog.Info("github run",
			"id", out.ID, "status", out.Status, "conclusion", out.Conclusion, "url", out.HTMLURL)
		return out, nil
	}
	return &GitHubRunStatus{Status: "pending", HeadBranch: w.Branch}, nil
}

// MergePullRequest finds the open PR for the given branch and merges it.
func MergePullRequest(ctx workflow.ActivityContext) (any, error) {
	var in MergeInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN env var not set in worker process")
	}

	listURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/pulls?head=%s:%s&state=open&per_page=5",
		in.Owner, in.Repo, in.Owner, in.Branch,
	)
	listReq, _ := http.NewRequest(http.MethodGet, listURL, nil)
	listReq.Header.Set("Accept", "application/vnd.github+json")
	listReq.Header.Set("Authorization", "Bearer "+token)
	listReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		return nil, err
	}
	defer listResp.Body.Close()
	listRaw, _ := io.ReadAll(listResp.Body)
	if listResp.StatusCode != 200 {
		return nil, fmt.Errorf("list PRs HTTP %d: %s", listResp.StatusCode, listRaw)
	}

	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(listRaw, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, fmt.Errorf("no open PR found for branch %s", in.Branch)
	}
	pr := prs[0]

	mergeURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/merge", in.Owner, in.Repo, pr.Number)
	mergeBody, _ := json.Marshal(map[string]string{"merge_method": in.Method})
	mergeReq, _ := http.NewRequest(http.MethodPut, mergeURL, strings.NewReader(string(mergeBody)))
	mergeReq.Header.Set("Accept", "application/vnd.github+json")
	mergeReq.Header.Set("Authorization", "Bearer "+token)
	mergeReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	mergeReq.Header.Set("Content-Type", "application/json")

	mergeResp, err := http.DefaultClient.Do(mergeReq)
	if err != nil {
		return nil, err
	}
	defer mergeResp.Body.Close()
	mergeRaw, _ := io.ReadAll(mergeResp.Body)
	if mergeResp.StatusCode < 200 || mergeResp.StatusCode >= 300 {
		return nil, fmt.Errorf("merge PR #%d HTTP %d: %s", pr.Number, mergeResp.StatusCode, mergeRaw)
	}

	var parsed struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(mergeRaw, &parsed); err != nil {
		return nil, err
	}

	slog.Info("merged PR", "number", pr.Number, "sha", parsed.SHA, "method", in.Method, "url", pr.HTMLURL)
	return &MergeResult{
		PRNumber: pr.Number,
		SHA:      parsed.SHA,
		Merged:   parsed.Merged,
		HTMLURL:  pr.HTMLURL,
	}, nil
}

// FetchStageExports resolves a stage's `exports` map into concrete values.
// NOT YET IMPLEMENTED — see issue #21. The original plan called for GitHub job
// outputs, but those are not exposed via REST; we'll switch to artifact-based
// transport (artifact contains outputs.json with a flat string map).
// Until that is wired up, callers should leave `exports` empty.
func FetchStageExports(ctx workflow.ActivityContext) (any, error) {
	var in FetchExportsInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}
	return nil, fmt.Errorf("FetchStageExports not implemented (run %d, %d exports requested) — see issue #21", in.RunID, len(in.Exports))
}

// ─────────────────────────────────────────────────────────────────────────────
// MAIN
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	r := workflow.NewRegistry()
	if err := r.AddWorkflowN("BackstageTemplateWorkflow", BackstageTemplateWorkflow); err != nil {
		log.Fatalf("register workflow: %v", err)
	}
	activities := map[string]workflow.Activity{
		"CallScaffolder":    CallScaffolder,
		"PollTask":          PollTask,
		"FetchGitHubRun":    FetchGitHubRun,
		"MergePullRequest":  MergePullRequest,
		"FetchStageExports": FetchStageExports,
	}
	for name, fn := range activities {
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
	slog.Info("worker ready — use run.sh to start a workflow")

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-sigCtx.Done()
	slog.Info("shutting down")
}
