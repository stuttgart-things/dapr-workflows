package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type TestVMInput struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	Token        string `json:"token"`
	WorkflowFile string `json:"workflowFile"` // e.g. "test-vm.yaml"
	VMName       string `json:"vmName"`
	Playbooks    string `json:"playbooks"`
	Parameters   string `json:"parameters"`
	Environment  string `json:"environment"`
	OSProfile    string `json:"osProfile"`
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

	dispatchTime := time.Now()
	err := client.DispatchWorkflow(bgCtx, gh.DispatchWorkflowInput{
		Owner:        input.Owner,
		Repo:         input.Repo,
		WorkflowFile: input.WorkflowFile,
		Ref:          input.Ref,
		Inputs: map[string]string{
			"vm_name":     input.VMName,
			"playbooks":   input.Playbooks,
			"parameters":  input.Parameters,
			"environment": input.Environment,
			"os_profile":  input.OSProfile,
		},
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
		Message:    fmt.Sprintf("VM test passed for %s/%s", input.Environment, input.OSProfile),
	}, nil
}
