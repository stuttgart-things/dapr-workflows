package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type RenderConfigInput struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	Token        string `json:"token"`
	WorkflowFile string `json:"workflowFile"`
	OSVersion    string `json:"osVersion"`    // e.g. "ubuntu24", "rocky9"
	Lab          string `json:"lab"`           // e.g. "labul", "labda"
	OSFamily     string `json:"osFamily"`      // e.g. "ubuntu", "rocky"
	Provisioning string `json:"provisioning"`  // e.g. "base-os", "rke2-node"
	Overrides    string `json:"overrides"`
	CreatePR     string `json:"createPr"`      // "true" / "false"
	RenderOnly   string `json:"renderOnly"`    // "true" / "false"
	DaggerVersion string `json:"daggerVersion"`
	Runner       string `json:"runner"`
}

type RenderConfigOutput struct {
	RunID      int64  `json:"runID"`
	Conclusion string `json:"conclusion"`
	RunURL     string `json:"runUrl"`
	Message    string `json:"message"`
}

// RenderConfigActivity dispatches a GH Actions workflow for config rendering and waits for completion.
func RenderConfigActivity(ctx workflow.ActivityContext) (any, error) {
	var input RenderConfigInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get input: %w", err)
	}

	client := gh.NewClient(input.Token)
	bgCtx := context.Background()

	inputs := map[string]string{
		"os-version":   input.OSVersion,
		"lab":          input.Lab,
		"os-family":    input.OSFamily,
		"provisioning": input.Provisioning,
		"create-pr":    input.CreatePR,
		"render-only":  input.RenderOnly,
	}

	if input.Overrides != "" {
		inputs["overrides"] = input.Overrides
	}
	if input.DaggerVersion != "" {
		inputs["dagger-version"] = input.DaggerVersion
	}
	if input.Runner != "" {
		inputs["runner"] = input.Runner
	}

	dispatchTime := time.Now()
	err := client.DispatchWorkflow(bgCtx, gh.DispatchWorkflowInput{
		Owner:        input.Owner,
		Repo:         input.Repo,
		WorkflowFile: input.WorkflowFile,
		Ref:          input.Ref,
		Inputs:       inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch render workflow: %w", err)
	}

	runID, err := client.FindRunByDispatch(bgCtx, input.Owner, input.Repo, input.WorkflowFile, dispatchTime, 10*time.Second, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("find render run: %w", err)
	}

	result, err := client.WaitForCompletion(bgCtx, input.Owner, input.Repo, runID, 15*time.Second, 30*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for render run: %w", err)
	}

	if result.Conclusion != "success" {
		return &RenderConfigOutput{
			RunID:      result.RunID,
			Conclusion: result.Conclusion,
			RunURL:     result.HTMLURL,
			Message:    fmt.Sprintf("render workflow failed with conclusion: %s", result.Conclusion),
		}, fmt.Errorf("render workflow concluded with: %s", result.Conclusion)
	}

	return &RenderConfigOutput{
		RunID:      result.RunID,
		Conclusion: result.Conclusion,
		RunURL:     result.HTMLURL,
		Message:    fmt.Sprintf("rendered config for %s/%s", input.Lab, input.OSVersion),
	}, nil
}
