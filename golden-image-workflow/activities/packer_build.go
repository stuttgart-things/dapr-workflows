package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type PackerBuildInput struct {
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	Ref           string `json:"ref"`
	Token         string `json:"token"`
	WorkflowFile  string `json:"workflowFile"` // e.g. "packer-build.yaml"
	ConfigFile    string `json:"configFile"`
	PackerVersion string `json:"packerVersion"`
	Arch          string `json:"arch"`
	Environment   string `json:"environment"`
	OSProfile     string `json:"osProfile"`
}

type PackerBuildOutput struct {
	RunID      int64  `json:"runID"`
	Conclusion string `json:"conclusion"`
	RunURL     string `json:"runUrl"`
	Message    string `json:"message"`
}

// PackerBuildActivity dispatches a GH Actions workflow for the Packer build and waits for completion.
func PackerBuildActivity(ctx workflow.ActivityContext) (any, error) {
	var input PackerBuildInput
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
			"config_file":    input.ConfigFile,
			"packer_version": input.PackerVersion,
			"arch":           input.Arch,
			"environment":    input.Environment,
			"os_profile":     input.OSProfile,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch packer-build workflow: %w", err)
	}

	runID, err := client.FindRunByDispatch(bgCtx, input.Owner, input.Repo, input.WorkflowFile, dispatchTime, 10*time.Second, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("find packer-build run: %w", err)
	}

	result, err := client.WaitForCompletion(bgCtx, input.Owner, input.Repo, runID, 30*time.Second, 60*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for packer-build run: %w", err)
	}

	if result.Conclusion != "success" {
		return &PackerBuildOutput{
			RunID:      result.RunID,
			Conclusion: result.Conclusion,
			RunURL:     result.HTMLURL,
			Message:    fmt.Sprintf("packer-build workflow failed with conclusion: %s", result.Conclusion),
		}, fmt.Errorf("packer-build workflow concluded with: %s", result.Conclusion)
	}

	return &PackerBuildOutput{
		RunID:      result.RunID,
		Conclusion: result.Conclusion,
		RunURL:     result.HTMLURL,
		Message:    fmt.Sprintf("packer build completed for %s/%s", input.Environment, input.OSProfile),
	}, nil
}
