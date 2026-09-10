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
	"sort"
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
	BackstageURL string                 `json:"backstageURL"`
	TemplateRef  string                 `json:"templateRef"`
	Values       map[string]interface{} `json:"values"`
	AuthToken    string                 `json:"authToken"`
	DryRun       bool                   `json:"dryRun"`
	Watch        *GitHubWatch           `json:"watch,omitempty"`
}

type GitHubWatch struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	WorkflowFile string `json:"workflowFile"`
	Branch       string `json:"branch"`
	TimeoutMin   int    `json:"timeoutMin"`
	// NotBefore is stamped by the workflow, never by the caller. Runs created
	// before this instance started belong to an earlier attempt that reused the
	// branch name, and must not be judged as this attempt's result.
	NotBefore string       `json:"notBefore,omitempty"`
	Merge     *MergeConfig `json:"merge,omitempty"`
}

type MergeConfig struct {
	Enabled bool   `json:"enabled"`
	Method  string `json:"method"` // squash | merge | rebase
}

// ─────────────────────────────────────────────────────────────────────────────
// WORKFLOW
// ─────────────────────────────────────────────────────────────────────────────

func BackstageTemplateWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	// The orchestration clock: replay-safe, and earlier than any GitHub run this
	// instance can cause, since those only exist after the scaffolder pushes.
	startedAt := ctx.CurrentTimeUTC().Format(time.RFC3339)
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

		watchIn := *in.Watch
		watchIn.NotBefore = startedAt
		if err := ctx.CallActivity(FetchGitHubRun, workflow.WithActivityInput(watchIn)).Await(&ghRun); err != nil {
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

// GateAndMergeWorkflow judges an EXISTING pull request and merges it, without
// scaffolding anything or waiting for a build.
//
// BackstageTemplateWorkflow already does this, but only as the tail of one long
// instance: scaffold, wait for the GitHub run, gate, merge. When the wait runs
// out the instance FAILS and the gate never runs -- even though the PR is fine
// and the build finished a minute later. The build is unaffected (the workflow
// watches, it does not drive), so what is lost is only the judgement, and there
// is no way to ask for just that part again.
//
// Witnessed on labda-dev-a: timeoutMin 45 is calibrated for a plain VM, and a
// provisioning_profile=kubernetes build adds base-os, RKE2 and a flux
// bootstrap. The PR was healthy and had to be merged by hand.
//
// This is the same MergePullRequest activity, reachable on its own.
//
// NOT reachable from a BackstageTemplateRun yet, despite the CR carrying a
// `workflowName` field: the RGD hardcodes the payload as
// {backstageURL, templateRef, dryRun, values, watch}, which is the shape
// BackstageTemplateWorkflow reads and nothing like MergeInput. Pointing
// workflowName here today sends that payload, every MergeInput field lands
// empty, and the guard below refuses it. A second RGD -- or a payload the
// existing one can shape per workflow -- is the follow-up.
//
// Until then this is started by POSTing MergeInput to the sidecar directly,
// which is what the recovery case actually needs: a PR is already open and
// somebody wants it judged.
func GateAndMergeWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var in MergeInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}
	if in.Owner == "" || in.Repo == "" || in.Branch == "" {
		return nil, fmt.Errorf("owner, repo and branch are required")
	}
	if in.Method == "" {
		in.Method = "squash"
	}

	ctx.SetCustomStatus(fmt.Sprintf("gating %s/%s on branch %s", in.Owner, in.Repo, in.Branch))

	// The activity finds the open PR for the branch, refuses on any check that
	// is not green, and merges otherwise. A refusal comes back as an error,
	// which is the intended outcome rather than a fault: this workflow says
	// whether the PR was merged, and if not, which checks stopped it.
	var merged MergeResult
	if err := ctx.CallActivity(MergePullRequest, workflow.WithActivityInput(in)).Await(&merged); err != nil {
		return nil, fmt.Errorf("gate and merge: %w", err)
	}
	ctx.SetCustomStatus(fmt.Sprintf("merged PR #%d (%s)", merged.PRNumber, merged.SHA))
	return &merged, nil
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
	if in.AuthToken == "" {
		in.AuthToken = os.Getenv("BACKSTAGE_AUTH_TOKEN")
	}
	// The Backstage endpoint is per-lab, exactly like the token next to it,
	// so it belongs to the cluster and not to the caller's JSON. Without this
	// fallback every input file has to name a lab, and the ones in this repo
	// all named LabUL -- which resolves to nothing on a LabDA cluster and
	// fails as a DNS timeout inside a workflow run, where nobody is looking.
	if in.BackstageURL == "" {
		in.BackstageURL = os.Getenv("BACKSTAGE_URL")
	}
	if in.BackstageURL == "" {
		return nil, fmt.Errorf("no Backstage URL: set it in the workflow input or BACKSTAGE_URL in the worker environment")
	}

	// The entity is fetched on BOTH paths now. dry-run needs it inline anyway;
	// the real path needs it for the schema defaults, which the scaffolder API
	// does not apply and the browser form does. Without this, every field the
	// caller did not name arrives empty -- see applySchemaDefaults.
	tmpl, err := fetchTemplateEntity(in.BackstageURL, in.TemplateRef, in.AuthToken)
	if err != nil {
		return nil, fmt.Errorf("fetch template: %w", err)
	}
	values := applySchemaDefaults(tmpl, in.Values)
	if added := len(values) - len(in.Values); added > 0 {
		slog.Info("filled schema defaults the caller did not supply",
			"templateRef", in.TemplateRef, "count", added)
	}

	url := in.BackstageURL + "/api/scaffolder/v2/tasks"
	payload := map[string]interface{}{
		"templateRef": in.TemplateRef,
		"values":      values,
	}
	if in.DryRun {
		url = in.BackstageURL + "/api/scaffolder/v2/dry-run"
		payload = map[string]interface{}{
			"template":          tmpl,
			"values":            values,
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

	resp, err := backstageClient.Do(req)
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
	if in.AuthToken == "" {
		in.AuthToken = os.Getenv("BACKSTAGE_AUTH_TOKEN")
	}
	// The workflow forwards the input's BackstageURL, which may now be empty
	// because CallScaffolder resolved its own copy from the environment.
	if in.BackstageURL == "" {
		in.BackstageURL = os.Getenv("BACKSTAGE_URL")
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
		WorkflowRuns []ghWorkflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	picked := pickRun(parsed.WorkflowRuns, w.NotBefore)
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

// ghWorkflowRun is one entry of GET /repos/{o}/{r}/actions/workflows/{f}/runs.
type ghWorkflowRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	HeadBranch string `json:"head_branch"`
	CreatedAt  string `json:"created_at"`
}

// pickRun chooses the run this instance should judge, or nil to keep polling.
// The API lists most recent first, so the first eligible entry is the newest.
//
// Three kinds of run are NOT a verdict on this attempt:
//
//   - skipped: pr-vm-deploy.yaml fires on PR open before labels are applied,
//     so the first run is always skipped and the real one follows.
//   - cancelled: the pipeline commits its generated OpenBao secrets back to the
//     branch, which cancels its own in-flight run via cancel-in-progress. The
//     successor is the real run. A run a human cancelled also lands here and
//     simply polls until the watch times out, which is the safe direction.
//   - created before notBefore: it belongs to an EARLIER attempt. Every attempt
//     at the same cluster reuses the branch name, and filtering by branch alone
//     let a new instance judge the previous attempt's finished run -- on
//     labda-dev-a the fourth attempt read the third attempt's failure at poll
//     1/540, gave up, and never saw its own run go green.
//
// clockSkewTolerance widens the floor. notBefore comes from the cluster's clock
// and created_at from GitHub's; if the cluster runs ahead, this attempt's own
// run can carry a created_at slightly BEFORE the instance's start, and a hard
// floor would discard it -- the watch would then poll to its timeout with the
// right run sitting in the list.
//
// Thirty seconds does not reopen the stale-run hole: a previous attempt's run
// is at least one scaffolder task plus a build older than the next instance,
// i.e. minutes, and one that is younger was cancelled by the new push anyway.
const clockSkewTolerance = 30 * time.Second

func pickRun(runs []ghWorkflowRun, notBefore string) *ghWorkflowRun {
	var floor time.Time
	if notBefore != "" {
		if t, err := time.Parse(time.RFC3339, notBefore); err == nil {
			floor = t.Add(-clockSkewTolerance)
		}
	}
	for i := range runs {
		r := runs[i]
		if r.Status == "completed" && (r.Conclusion == "skipped" || r.Conclusion == "cancelled") {
			continue
		}
		if !floor.IsZero() {
			created, err := time.Parse(time.RFC3339, r.CreatedAt)
			if err != nil || created.Before(floor) {
				continue
			}
		}
		return &r
	}
	return nil
}

// unfinishedChecks names every check run on `sha` that is not a green light:
// still running, or completed with anything other than success/neutral/skipped.
// An empty result means the head is clear to merge.
//
// `neutral` and `skipped` are green on purpose. A skipped job is the normal
// state of a conditional workflow -- pr-vm-deploy.yaml alone contributes
// several -- and treating them as failures would block every merge.
// githubAPI is a var, not a const, so the merge gate can be tested against an
// httptest server. It is the one call site where that matters: unfinishedChecks
// decides whether a PR merges, and its pagination cannot be proven against the
// real API without a commit carrying 100+ check runs.
var githubAPI = "https://api.github.com"

func unfinishedChecks(owner, repo, sha, token string) ([]string, error) {
	type checkRun struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		StartedAt  string `json:"started_at"`
	}

	// PAGINATE, AND REFUSE RATHER THAN GUESS.
	//
	// The endpoint caps per_page at 100. Reading one page and merging on it is
	// fail-OPEN: a red check on page 2 is invisible, and the merge happens
	// anyway. That is the one direction this gate must never fail in.
	//
	// 100 is not far off. Measured on stuttgart-things@83d6ea08f: 46 check
	// runs from two pushes. Superseded entries count, and every re-run adds a
	// whole suite -- so the number grows with exactly the activity that makes
	// a careful gate matter most.
	const maxPages = 20 // 2000 check runs: a bound, not an expectation
	var all []checkRun
	total := -1
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("check runs for %s: exceeded %d pages (%d of %d collected)",
				sha, maxPages, len(all), total)
		}
		url := fmt.Sprintf(
			githubAPI+"/repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d",
			owner, repo, sha, page,
		)
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close() // in a loop: close per iteration, not via defer
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
		}

		var parsed struct {
			TotalCount int        `json:"total_count"`
			CheckRuns  []checkRun `json:"check_runs"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, err
		}
		all = append(all, parsed.CheckRuns...)
		total = parsed.TotalCount

		// A short page is the last one. Do NOT loop on len(all) < total: a
		// check created while we page raises total_count and would spin us to
		// maxPages against an endpoint whose last page is already empty.
		if len(parsed.CheckRuns) < 100 {
			break
		}
	}

	// Backstop for the case the loop cannot explain: fewer runs collected than
	// the API says exist. Refusing is right on both readings -- either the
	// list is genuinely partial, or a check appeared mid-pagination, and a
	// brand-new check is queued, hence not green, hence a refusal anyway.
	if len(all) < total {
		return nil, fmt.Errorf("check runs for %s: collected %d of %d -- refusing to judge a partial list",
			sha, len(all), total)
	}

	// KEEP ONLY THE NEWEST RUN PER NAME.
	//
	// `filter=latest` (the API default) deduplicates within a check SUITE, not
	// across them -- and re-running a workflow creates a NEW suite. So a check
	// that failed, was re-run and went green comes back twice: once failed,
	// once succeeded. Judging every entry would refuse that merge forever,
	// which turns "re-run the flaky job" into "the automation is stuck".
	//
	// Measured on stuttgart-things@83d6ea08f: 46 check runs, 16 distinct
	// names, three of them carrying mixed conclusions.
	//
	// Keying on the NAME is what GitHub's own required-checks do, and it
	// inherits their caveat: two different workflows with a same-named job
	// share one entry, so the newer green one hides the older red one. Both
	// jobs called `Config` in that measurement -- harmless there (all
	// skipped/success), worth knowing before naming a job something generic.
	newest := map[string]checkRun{}
	for _, c := range all {
		if prev, ok := newest[c.Name]; !ok || c.StartedAt > prev.StartedAt {
			newest[c.Name] = c
		}
	}

	var bad []string
	for _, c := range newest {
		if c.Status != "completed" {
			bad = append(bad, fmt.Sprintf("%s (%s)", c.Name, c.Status))
			continue
		}
		switch c.Conclusion {
		case "success", "neutral", "skipped":
		default:
			bad = append(bad, fmt.Sprintf("%s (%s)", c.Name, c.Conclusion))
		}
	}
	sort.Strings(bad) // map order is random; a stable message is greppable
	slog.Info("pr head checks",
		"sha", sha, "total", total, "distinct", len(newest), "notGreen", len(bad))
	return bad, nil
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

	// 2. EVERY check on the PR head has to be green, not just the one workflow
	//    this run watched.
	//
	//    Watching a single workflowFile is not a gate. A repository can easily
	//    have a second workflow that is the real guard, and the two are
	//    independent: one goes green while the other fails, and merging on the
	//    first alone lands exactly the change the second rejected.
	//
	//    Witnessed on stuttgart-things#2796 (2026-09-07): `pr-vm-deploy.yaml`
	//    built the VM and reported success while `validate-cluster-components`
	//    was RED, because the cluster's OpenBao secrets had never been seeded.
	//    Merging there would have produced a cluster whose External Secrets can
	//    read nothing -- every package Healthy, every Kustomization Ready, and
	//    no credentials -- with nobody having seen the failing gate.
	//
	//    A check that has not finished counts as not-green. Refusing is cheap
	//    (a human merges, or re-runs the workflow); merging early is not.
	if pending, err := unfinishedChecks(in.Owner, in.Repo, pr.Head.SHA, token); err != nil {
		return nil, fmt.Errorf("check runs for %s: %w", pr.Head.SHA, err)
	} else if len(pending) > 0 {
		return nil, fmt.Errorf("refusing to merge PR #%d (%s): %s",
			pr.Number, pr.HTMLURL, strings.Join(pending, ", "))
	}

	// 3. Merge it.
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
	if err := r.AddWorkflowN("GateAndMergeWorkflow", GateAndMergeWorkflow); err != nil {
		log.Fatalf("register GateAndMergeWorkflow: %v", err)
	}
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

// ─────────────────────────────────────────────────────────────────────────────
// SCHEMA DEFAULTS
// ─────────────────────────────────────────────────────────────────────────────

// applySchemaDefaults fills in `default:` values the caller did not supply.
//
// JSON-Schema `default` is a UI hint: a validator does not apply it, and neither
// does Backstage's scaffolder API. The form fills those fields in the browser,
// so a template driven through the UI gets them and the same template driven
// through this worker does NOT -- every unset field arrives as an empty string.
//
// That is not a cosmetic difference. On create-terraform-vm it rendered
//
//	s3     = ""                       (backend.tf, terraform init fails)
//	vsphere_vm_template    = ""
//	vsphere_datastore      = ""
//	vsphere_network        = ""
//
// while bucket and region came out right, because THAT template happens to
// carry its own `| default(...)` on those two lines and on nothing else. 57 of
// its values have a schema default and no such fallback. Adding 57 fallbacks
// would duplicate every default in two places that then drift silently; filling
// them from the schema, which is where they are already written down, does not.
//
// Values the caller set are never overwritten -- an explicit empty string stays
// empty, because "I mean blank" and "I said nothing" are different and only the
// second one is being repaired here.
func applySchemaDefaults(entity map[string]interface{}, values map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range values {
		out[k] = v
	}

	spec, _ := entity["spec"].(map[string]interface{})
	pages, _ := spec["parameters"].([]interface{})

	for _, p := range pages {
		page, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		applyProps(page["properties"], out)

		// Conditional branches. rjsf picks the branch whose discriminator enum
		// contains the current value -- `lab: LabDA` selects the vSphere block
		// and its datastore/network/template defaults. Without this only the
		// unconditional fields would be filled, which is most of the shape but
		// none of the ones that name a machine.
		deps, _ := page["dependencies"].(map[string]interface{})
		for depKey, depVal := range deps {
			applyDependency(depKey, depVal, out)
		}
	}
	return out
}

// applyProps fills defaults from one `properties` object.
func applyProps(props interface{}, out map[string]interface{}) {
	m, ok := props.(map[string]interface{})
	if !ok {
		return
	}
	for name, raw := range m {
		prop, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		def, hasDef := prop["default"]
		if !hasDef {
			continue
		}
		if _, set := out[name]; !set {
			out[name] = def
		}
	}
}

// applyDependency walks one `dependencies.<key>` entry, taking the oneOf branch
// whose own constraint on <key> matches the value already chosen.
func applyDependency(key string, dep interface{}, out map[string]interface{}) {
	d, ok := dep.(map[string]interface{})
	if !ok {
		return
	}
	// A dependency can be a bare properties block rather than a oneOf.
	applyProps(d["properties"], out)

	branches, _ := d["oneOf"].([]interface{})
	for _, b := range branches {
		branch, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		props, _ := branch["properties"].(map[string]interface{})
		if !branchMatches(props, key, out) {
			continue
		}
		applyProps(props, out)
		// Branches nest: the LabUL arm carries its own `dependencies.cloud`.
		if nested, ok := branch["dependencies"].(map[string]interface{}); ok {
			for k, v := range nested {
				applyDependency(k, v, out)
			}
		}
	}
}

// branchMatches reports whether a oneOf arm applies, by testing the caller's
// value for the discriminator against the arm's enum for that same key.
func branchMatches(props map[string]interface{}, key string, out map[string]interface{}) bool {
	disc, ok := props[key].(map[string]interface{})
	if !ok {
		return false
	}
	have, set := out[key]
	if !set {
		return false
	}
	// Two spellings, both in use in the same template. `enum` lists the values
	// an arm covers; `const` pins a single one, which is how boolean
	// discriminators are written -- create-terraform-vm's export_and_encrypt
	// arms are `const: true` / `const: false`. Reading only `enum` silently
	// skips every boolean-gated block, which is most of the optional ones.
	if c, ok := disc["const"]; ok {
		return fmt.Sprintf("%v", c) == fmt.Sprintf("%v", have)
	}
	allowed, ok := disc["enum"].([]interface{})
	if !ok {
		return false
	}
	for _, a := range allowed {
		if fmt.Sprintf("%v", a) == fmt.Sprintf("%v", have) {
			return true
		}
	}
	return false
}
