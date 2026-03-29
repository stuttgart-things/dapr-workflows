package activities

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type NotifyInput struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	Token        string `json:"token"`
	WorkflowFile string `json:"workflowFile"` // e.g. "notify.yaml"
	Channels     string `json:"channels"`      // comma-separated
	System       string `json:"system"`
	Tags         string `json:"tags"`
	Status       string `json:"status"` // overall workflow status
	RunURL       string `json:"runUrl"` // link to the main workflow for context
}

type NotifyOutput struct {
	RunID      int64  `json:"runID"`
	Conclusion string `json:"conclusion"`
	RunURL     string `json:"runUrl"`
	Message    string `json:"message"`
}

// NotifyActivity dispatches a GH Actions workflow that sends notifications about the build result.
func NotifyActivity(ctx workflow.ActivityContext) (any, error) {
	var input NotifyInput
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
			"channels": input.Channels,
			"system":   input.System,
			"tags":     input.Tags,
			"status":   input.Status,
			"run_url":  input.RunURL,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch notify workflow: %w", err)
	}

	runID, err := client.FindRunByDispatch(bgCtx, input.Owner, input.Repo, input.WorkflowFile, dispatchTime, 10*time.Second, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("find notify run: %w", err)
	}

	result, err := client.WaitForCompletion(bgCtx, input.Owner, input.Repo, runID, 10*time.Second, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for notify run: %w", err)
	}

	if result.Conclusion != "success" {
		return &NotifyOutput{
			RunID:      result.RunID,
			Conclusion: result.Conclusion,
			RunURL:     result.HTMLURL,
			Message:    fmt.Sprintf("notify workflow failed with conclusion: %s", result.Conclusion),
		}, fmt.Errorf("notify workflow concluded with: %s", result.Conclusion)
	}

	return &NotifyOutput{
		RunID:      result.RunID,
		Conclusion: result.Conclusion,
		RunURL:     result.HTMLURL,
		Message:    fmt.Sprintf("notifications sent to: %s", strings.Join(strings.Split(input.Channels, ","), ", ")),
	}, nil
}
