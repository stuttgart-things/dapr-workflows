package main

import (
	"context"
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

	dapr "github.com/dapr/go-sdk/client"
	"github.com/dapr/durabletask-go/workflow"
)

// ─────────────────────────────────────────────────────────────────────────────
// INPUT
// ─────────────────────────────────────────────────────────────────────────────

type Input struct {
	BackstageURL string                 `json:"backstageURL"`
	TemplateRef  string                 `json:"templateRef"`
	Values       map[string]interface{} `json:"values"`
	AuthToken    string                 `json:"authToken"`
	DryRun       bool                   `json:"dryRun"`
	Watch        *GitHubWatch           `json:"watch,omitempty"`
}

type GitHubWatch struct {
	Owner        string      `json:"owner"`
	Repo         string      `json:"repo"`
	WorkflowFile string      `json:"workflowFile"`
	Branch       string      `json:"branch"`
	TimeoutMin   int         `json:"timeoutMin"`
	Merge        *MergeConfig `json:"merge,omitempty"`
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

	ctx.SetCustomStatus("calling backstage scaffolder")

	var result ScaffolderResult
	if err := ctx.CallActivity(CallScaffolder, workflow.WithActivityInput(in)).Await(&result); err != nil {
		return nil, fmt.Errorf("scaffolder call failed: %w", err)
	}

	if in.DryRun {
		ctx.SetCustomStatus(fmt.Sprintf("dry-run complete: task %s", result.TaskID))
		return &result, nil
	}

	ctx.SetCustomStatus(fmt.Sprintf("polling task %s", result.TaskID))

	for i := range 40 {
		if err := ctx.CreateTimer(5 * time.Second).Await(nil); err != nil {
			return nil, err
		}

		pollIn := PollInput{
			BackstageURL: in.BackstageURL,
			TaskID:       result.TaskID,
			AuthToken:    in.AuthToken,
		}

		var status TaskStatus
		if err := ctx.CallActivity(PollTask, workflow.WithActivityInput(pollIn)).Await(&status); err != nil {
			return nil, err
		}

		ctx.SetCustomStatus(fmt.Sprintf("[%d/40] task %s: %s", i+1, result.TaskID, status.Status))

		switch status.Status {
		case "completed":
			result.FinalStatus = "completed"
			goto scaffolderDone
		case "failed", "cancelled":
			return nil, fmt.Errorf("task %s → %s (step: %s)", result.TaskID, status.Status, status.FailedStep)
		}
	}

	return nil, fmt.Errorf("timed out waiting for task %s", result.TaskID)

scaffolderDone:
	if in.Watch == nil {
		return &result, nil
	}

	// ── watch GitHub Actions run ──────────────────────────────────────────
	timeoutMin := in.Watch.TimeoutMin
	if timeoutMin <= 0 {
		timeoutMin = 30
	}
	maxIters := (timeoutMin * 60) / 10
	ctx.SetCustomStatus(fmt.Sprintf("waiting for GH workflow %s on %s", in.Watch.WorkflowFile, in.Watch.Branch))

	var ghRun GitHubRunStatus
	for i := range maxIters {
		if err := ctx.CreateTimer(10 * time.Second).Await(nil); err != nil {
			return nil, err
		}

		if err := ctx.CallActivity(FetchGitHubRun, workflow.WithActivityInput(*in.Watch)).Await(&ghRun); err != nil {
			return nil, fmt.Errorf("fetch gh run: %w", err)
		}

		ctx.SetCustomStatus(fmt.Sprintf("[%d/%d] gh run %d: %s/%s", i+1, maxIters, ghRun.ID, ghRun.Status, ghRun.Conclusion))

		if ghRun.Status == "completed" {
			result.GitHubRun = &ghRun
			if ghRun.Conclusion != "success" {
				return &result, fmt.Errorf("gh workflow %s → %s (%s)", in.Watch.WorkflowFile, ghRun.Conclusion, ghRun.HTMLURL)
			}

			// Auto-merge if configured.
			if in.Watch.Merge != nil && in.Watch.Merge.Enabled {
				ctx.SetCustomStatus(fmt.Sprintf("merging PR for branch %s", in.Watch.Branch))
				method := in.Watch.Merge.Method
				if method == "" {
					method = "squash"
				}
				var merged MergeResult
				mergeIn := MergeInput{
					Owner:  in.Watch.Owner,
					Repo:   in.Watch.Repo,
					Branch: in.Watch.Branch,
					Method: method,
				}
				if err := ctx.CallActivity(MergePullRequest, workflow.WithActivityInput(mergeIn)).Await(&merged); err != nil {
					return &result, fmt.Errorf("merge PR: %w", err)
				}
				result.Merge = &merged
				ctx.SetCustomStatus(fmt.Sprintf("merged PR #%d (%s)", merged.PRNumber, merged.SHA))
			}
			return &result, nil
		}
	}

	return &result, fmt.Errorf("timed out waiting for gh workflow %s on branch %s", in.Watch.WorkflowFile, in.Watch.Branch)
}

// ─────────────────────────────────────────────────────────────────────────────
// ACTIVITIES
// ─────────────────────────────────────────────────────────────────────────────

type ScaffolderResult struct {
	TaskID      string           `json:"taskId"`
	FinalStatus string           `json:"finalStatus,omitempty"`
	LogURL      string           `json:"logUrl"`
	DryRun      bool             `json:"dryRun"`
	GitHubRun   *GitHubRunStatus `json:"githubRun,omitempty"`
	Merge       *MergeResult     `json:"merge,omitempty"`
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
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | cancelled | ...
	HTMLURL    string `json:"htmlUrl"`
	HeadBranch string `json:"headBranch"`
}

func CallScaffolder(ctx workflow.ActivityContext) (any, error) {
	var in Input
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}

	url := in.BackstageURL + "/api/scaffolder/v2/tasks"
	payload := map[string]interface{}{
		"templateRef": in.TemplateRef,
		"values":      in.Values,
	}
	if in.DryRun {
		// Backstage's /dry-run endpoint requires the full template entity inline.
		// Fetch it from the catalog first.
		tmpl, err := fetchTemplateEntity(in.BackstageURL, in.TemplateRef, in.AuthToken)
		if err != nil {
			return nil, fmt.Errorf("fetch template: %w", err)
		}
		url = in.BackstageURL + "/api/scaffolder/v2/dry-run"
		payload = map[string]interface{}{
			"template":          tmpl,
			"values":            in.Values,
			"secrets":           map[string]string{},
			"directoryContents": []interface{}{},
		}
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if in.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+in.AuthToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}

	if in.DryRun {
		// dry-run endpoint returns rendered output, no task ID
		slog.Info("scaffolder dry-run completed", "bytes", len(raw))
		return &ScaffolderResult{
			TaskID:      "dry-run",
			FinalStatus: "completed",
			DryRun:      true,
		}, nil
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
		TaskID: parsed.ID,
		LogURL: fmt.Sprintf("%s/create/tasks/%s", in.BackstageURL, parsed.ID),
		DryRun: false,
	}, nil
}

// fetchTemplateEntity loads a template from the Backstage catalog by ref
// (e.g. "template:default/flux-bootstrap") and returns its parsed JSON object.
func fetchTemplateEntity(baseURL, ref, token string) (map[string]interface{}, error) {
	// templateRef format: "<kind>:<namespace>/<name>"
	kindRest := strings.SplitN(ref, ":", 2)
	if len(kindRest) != 2 {
		return nil, fmt.Errorf("invalid templateRef %q", ref)
	}
	kind := kindRest[0]
	nsName := strings.SplitN(kindRest[1], "/", 2)
	if len(nsName) != 2 {
		return nil, fmt.Errorf("invalid templateRef %q", ref)
	}
	url := fmt.Sprintf("%s/api/catalog/entities/by-name/%s/%s/%s", baseURL, kind, nsName[0], nsName[1])

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
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

type PollInput struct {
	BackstageURL string `json:"backstageURL"`
	TaskID       string `json:"taskId"`
	AuthToken    string `json:"authToken"`
}

type TaskStatus struct {
	Status     string `json:"status"`
	FailedStep string `json:"failedStep"`
}

func PollTask(ctx workflow.ActivityContext) (any, error) {
	var in PollInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/scaffolder/v2/tasks/%s", in.BackstageURL, in.TaskID), nil)
	if in.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+in.AuthToken)
	}

	resp, err := http.DefaultClient.Do(req)
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

// FetchGitHubRun queries the latest workflow run for the given branch and
// returns its current status. The workflow polls this until status="completed".
func FetchGitHubRun(ctx workflow.ActivityContext) (any, error) {
	var w GitHubWatch
	if err := ctx.GetInput(&w); err != nil {
		return nil, fmt.Errorf("get input: %w", err)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN env var not set in worker process")
	}

	apiURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/actions/workflows/%s/runs?branch=%s&per_page=5",
		w.Owner, w.Repo, w.WorkflowFile, url.QueryEscape(w.Branch),
	)
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
			CreatedAt  string `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	// API returns most recent first. Skip runs that completed with conclusion
	// "skipped" — pr-vm-deploy.yaml fires on PR open before labels are applied,
	// so the first run is always skipped; the real run comes after the label
	// workflow re-triggers it.
	var picked *struct {
		ID         int64  `json:"id"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		HeadBranch string `json:"head_branch"`
		CreatedAt  string `json:"created_at"`
	}
	for i := range parsed.WorkflowRuns {
		r := parsed.WorkflowRuns[i]
		if r.Status == "completed" && r.Conclusion == "skipped" {
			continue
		}
		picked = &r
		break
	}
	if picked == nil {
		// No real run yet — keep polling.
		return &GitHubRunStatus{Status: "pending", HeadBranch: w.Branch}, nil
	}
	r := *picked
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

	// 1. Find the open PR for this head branch.
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

	// 2. Merge it.
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
	if err := r.AddActivityN("CallScaffolder", CallScaffolder); err != nil {
		log.Fatalf("register CallScaffolder: %v", err)
	}
	if err := r.AddActivityN("PollTask", PollTask); err != nil {
		log.Fatalf("register PollTask: %v", err)
	}
	if err := r.AddActivityN("FetchGitHubRun", FetchGitHubRun); err != nil {
		log.Fatalf("register FetchGitHubRun: %v", err)
	}
	if err := r.AddActivityN("MergePullRequest", MergePullRequest); err != nil {
		log.Fatalf("register MergePullRequest: %v", err)
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
