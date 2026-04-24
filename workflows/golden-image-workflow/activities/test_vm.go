package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type TestVMInput struct {
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	Ref           string `json:"ref"`
	Token         string `json:"token"`
	WorkflowFile  string `json:"workflowFile"`
	TemplateName  string `json:"templateName"`  // from packer build output
	OSVersion     string `json:"osVersion"`
	Lab           string `json:"lab"`
	OSFamily      string `json:"osFamily"`
	TestPlaybooks string `json:"testPlaybooks"` // comma-separated
	Overrides     string `json:"overrides"`
	Runner        string `json:"runner"`
	DaggerVersion string `json:"daggerVersion"`
}

type TestVMOutput struct {
	RunID      int64  `json:"runID"`
	Conclusion string `json:"conclusion"`
	RunURL     string `json:"runUrl"`
	Message    string `json:"message"`
}

// TestVMActivity dispatches a GH Actions workflow that spins up a test VM and validates the image.
func TestVMActivity(ctx workflow.ActivityContext) (any, error) {
	var input TestVMInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get input: %w", err)
	}

	client := gh.NewClient(input.Token)
	bgCtx := context.Background()

	inputs := map[string]string{
		"template-name": input.TemplateName,
		"os-version":    input.OSVersion,
		"lab":           input.Lab,
		"os-family":     input.OSFamily,
	}

	if input.TestPlaybooks != "" {
		inputs["test-playbooks"] = input.TestPlaybooks
	}
	if input.Overrides != "" {
		inputs["overrides"] = input.Overrides
	}
	if input.Runner != "" {
		inputs["runner"] = input.Runner
	}
	if input.DaggerVersion != "" {
		inputs["dagger-version"] = input.DaggerVersion
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
		return nil, fmt.Errorf("dispatch test-vm workflow: %w", err)
	}

	runID, err := client.FindRunByDispatch(bgCtx, input.Owner, input.Repo, input.WorkflowFile, dispatchTime, 10*time.Second, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("find test-vm run: %w", err)
	}

	result, err := client.WaitForCompletion(bgCtx, input.Owner, input.Repo, runID, 30*time.Second, 45*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for test-vm run: %w", err)
	}

	if result.Conclusion != "success" {
		return &TestVMOutput{
			RunID:      result.RunID,
			Conclusion: result.Conclusion,
			RunURL:     result.HTMLURL,
			Message:    fmt.Sprintf("test-vm workflow failed with conclusion: %s", result.Conclusion),
		}, fmt.Errorf("test-vm workflow concluded with: %s", result.Conclusion)
	}

	return &TestVMOutput{
		RunID:      result.RunID,
		Conclusion: result.Conclusion,
		RunURL:     result.HTMLURL,
		Message:    fmt.Sprintf("test VM passed for %s/%s (template: %s)", input.Lab, input.OSVersion, input.TemplateName),
	}, nil
}
