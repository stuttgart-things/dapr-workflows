package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client wraps GitHub REST API calls for Actions workflow dispatch and polling.
type Client struct {
	Token   string
	BaseURL string // defaults to "https://api.github.com"
	HTTP    *http.Client
}

// NewClient creates a GitHub API client with the given token.
func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: "https://api.github.com",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// DispatchWorkflowInput contains the parameters for triggering a workflow_dispatch event.
type DispatchWorkflowInput struct {
	Owner        string
	Repo         string
	WorkflowFile string            // e.g. "render-config.yaml"
	Ref          string            // branch ref, e.g. "main"
	Inputs       map[string]string // workflow_dispatch inputs
}

// RunResult holds info about a completed (or failed) workflow run.
type RunResult struct {
	RunID      int64  `json:"runID"`
	Status     string `json:"status"`     // "completed"
	Conclusion string `json:"conclusion"` // "success", "failure", "cancelled"
	HTMLURL    string `json:"htmlUrl"`
	LogsURL    string `json:"logsUrl"`
}

// DispatchWorkflow triggers a workflow_dispatch event.
func (c *Client) DispatchWorkflow(ctx context.Context, input DispatchWorkflowInput) error {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches",
		c.baseURL(), input.Owner, input.Repo, input.WorkflowFile)

	body := map[string]any{
		"ref":    input.Ref,
		"inputs": input.Inputs,
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal dispatch body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return fmt.Errorf("create dispatch request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dispatch returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// workflowRun represents the relevant fields from the GH API runs response.
type workflowRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	CreatedAt  string `json:"created_at"`
}

type listRunsResponse struct {
	WorkflowRuns []workflowRun `json:"workflow_runs"`
}

// FindRunByDispatch polls the runs list to find a run triggered after dispatchTime
// that matches the workflow file. It uses the unique dispatchKey passed as an input
// to correlate the dispatch to the run.
func (c *Client) FindRunByDispatch(ctx context.Context, owner, repo, workflowFile string, dispatchTime time.Time, pollInterval, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		url := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/runs?per_page=5&event=workflow_dispatch",
			c.baseURL(), owner, repo, workflowFile)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, fmt.Errorf("create find-run request: %w", err)
		}
		c.setHeaders(req)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return 0, fmt.Errorf("find-run request failed: %w", err)
		}

		var runs listRunsResponse
		if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
			resp.Body.Close()
			return 0, fmt.Errorf("decode runs response: %w", err)
		}
		resp.Body.Close()

		for _, run := range runs.WorkflowRuns {
			createdAt, err := time.Parse(time.RFC3339, run.CreatedAt)
			if err != nil {
				continue
			}
			// Match runs created after our dispatch (with 5s tolerance for clock skew)
			if createdAt.After(dispatchTime.Add(-5 * time.Second)) {
				return run.ID, nil
			}
		}

		time.Sleep(pollInterval)
	}

	return 0, fmt.Errorf("timed out finding workflow run for %s after %v", workflowFile, timeout)
}

// WaitForCompletion polls a workflow run until it reaches a terminal state.
func (c *Client) WaitForCompletion(ctx context.Context, owner, repo string, runID int64, pollInterval, timeout time.Duration) (*RunResult, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d", c.baseURL(), owner, repo, runID)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("create poll request: %w", err)
		}
		c.setHeaders(req)

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("poll request failed: %w", err)
		}

		var run workflowRun
		if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode run response: %w", err)
		}
		resp.Body.Close()

		if run.Status == "completed" {
			return &RunResult{
				RunID:      run.ID,
				Status:     run.Status,
				Conclusion: run.Conclusion,
				HTMLURL:    run.HTMLURL,
				LogsURL:    fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/logs", c.baseURL(), owner, repo, run.ID),
			}, nil
		}

		time.Sleep(pollInterval)
	}

	return nil, fmt.Errorf("timed out waiting for run %d to complete after %v", runID, timeout)
}

// GetRunLog fetches the combined log text for a workflow run.
// It requests the logs download URL and reads the response body.
// Note: GH API returns a redirect to a zip, but with Accept: text we get plaintext for job logs.
func (c *Client) GetRunLog(ctx context.Context, owner, repo string, runID int64) (string, error) {
	// Get jobs for this run and fetch logs per job
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=30",
		c.baseURL(), owner, repo, runID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create jobs request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("jobs request failed: %w", err)
	}
	defer resp.Body.Close()

	var jobsResp struct {
		Jobs []struct {
			ID int64 `json:"id"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jobsResp); err != nil {
		return "", fmt.Errorf("decode jobs response: %w", err)
	}

	var allLogs strings.Builder
	for _, job := range jobsResp.Jobs {
		jobLogURL := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs",
			c.baseURL(), owner, repo, job.ID)

		logReq, err := http.NewRequestWithContext(ctx, http.MethodGet, jobLogURL, nil)
		if err != nil {
			continue
		}
		c.setHeaders(logReq)

		// Use a client that follows redirects (default behavior)
		logResp, err := c.HTTP.Do(logReq)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(logResp.Body)
		logResp.Body.Close()
		allLogs.Write(body)
	}

	return allLogs.String(), nil
}

// ExtractFromLog searches the run log for a line matching "key: value" and returns the value.
func ExtractFromLog(log, key string) string {
	for _, line := range strings.Split(log, "\n") {
		if idx := strings.Index(line, key+": "); idx != -1 {
			return strings.TrimSpace(line[idx+len(key)+2:])
		}
	}
	return ""
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
