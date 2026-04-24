package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type CommitPRInput struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	Token        string `json:"token"`
	WorkflowFile string `json:"workflowFile"` // e.g. "commit-pr.yaml"
	BranchName   string `json:"branchName"`
	BaseBranch   string `json:"baseBranch"`
	CommitMsg    string `json:"commitMsg"`
	PRTitle      string `json:"prTitle"`
	PRBody       string `json:"prBody"`
}

type CommitPROutput struct {
	RunID      int64  `json:"runID"`
	Conclusion string `json:"conclusion"`
	RunURL     string `json:"runUrl"`
	Message    string `json:"message"`
}

// CommitPRActivity dispatches a GH Actions workflow that commits rendered files and creates a PR.
func CommitPRActivity(ctx workflow.ActivityContext) (any, error) {
	var input CommitPRInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get input: %w", err)
	}

	client := gh.NewClient(input.Token)
	bgCtx := context.Background()

	dispatchTime := time.Now()
	err := client.DispatchWorkflow(bgCtx, gh.DispatchWorkflowInput{
		Owner:        input.Owner,
		Repo:         input.Repo,
		WorkflowFile: input.WorkflowFile,
		Ref:          input.Ref,
		Inputs: map[string]string{
			"branch_name":    input.BranchName,
			"base_branch":    input.BaseBranch,
			"commit_message": input.CommitMsg,
			"pr_title":       input.PRTitle,
			"pr_body":        input.PRBody,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch commit-pr workflow: %w", err)
	}

	runID, err := client.FindRunByDispatch(bgCtx, input.Owner, input.Repo, input.WorkflowFile, dispatchTime, 10*time.Second, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("find commit-pr run: %w", err)
	}

	result, err := client.WaitForCompletion(bgCtx, input.Owner, input.Repo, runID, 15*time.Second, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for commit-pr run: %w", err)
	}

	if result.Conclusion != "success" {
		return &CommitPROutput{
			RunID:      result.RunID,
			Conclusion: result.Conclusion,
			RunURL:     result.HTMLURL,
			Message:    fmt.Sprintf("commit-pr workflow failed with conclusion: %s", result.Conclusion),
		}, fmt.Errorf("commit-pr workflow concluded with: %s", result.Conclusion)
	}

	return &CommitPROutput{
		RunID:      result.RunID,
		Conclusion: result.Conclusion,
		RunURL:     result.HTMLURL,
		Message:    "commit and PR created successfully",
	}, nil
}
