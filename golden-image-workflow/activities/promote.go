package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type PromoteInput struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	Token        string `json:"token"`
	WorkflowFile string `json:"workflowFile"`
	TemplateName string `json:"templateName"` // source template from packer build
	TargetName   string `json:"targetName"`   // golden image name
	BuildFolder  string `json:"buildFolder"`  // vSphere packer build folder
	GoldenFolder string `json:"goldenFolder"` // vSphere golden image folder
	Runner       string `json:"runner"`
}

type PromoteOutput struct {
	RunID      int64  `json:"runID"`
	Conclusion string `json:"conclusion"`
	RunURL     string `json:"runUrl"`
	Message    string `json:"message"`
}

// PromoteActivity dispatches a GH Actions workflow that promotes the built image to golden status.
func PromoteActivity(ctx workflow.ActivityContext) (any, error) {
	var input PromoteInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get input: %w", err)
	}

	client := gh.NewClient(input.Token)
	bgCtx := context.Background()

	inputs := map[string]string{
		"template-name": input.TemplateName,
		"target-name":   input.TargetName,
	}

	if input.BuildFolder != "" {
		inputs["build-folder"] = input.BuildFolder
	}
	if input.GoldenFolder != "" {
		inputs["golden-folder"] = input.GoldenFolder
	}
	if input.Runner != "" {
		inputs["runner"] = input.Runner
	}

	runID, err := client.DispatchAndFindRun(bgCtx, gh.DispatchWorkflowInput{
		Owner:        input.Owner,
		Repo:         input.Repo,
		WorkflowFile: input.WorkflowFile,
		Ref:          input.Ref,
		Inputs:       inputs,
	}, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("dispatch promote workflow: %w", err)
	}

	result, err := client.WaitForCompletion(bgCtx, input.Owner, input.Repo, runID, 15*time.Second, 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for promote run: %w", err)
	}

	if result.Conclusion != "success" {
		return &PromoteOutput{
			RunID:      result.RunID,
			Conclusion: result.Conclusion,
			RunURL:     result.HTMLURL,
			Message:    fmt.Sprintf("promote workflow failed with conclusion: %s", result.Conclusion),
		}, fmt.Errorf("promote workflow concluded with: %s", result.Conclusion)
	}

	return &PromoteOutput{
		RunID:      result.RunID,
		Conclusion: result.Conclusion,
		RunURL:     result.HTMLURL,
		Message:    fmt.Sprintf("promoted %s to %s", input.TemplateName, input.TargetName),
	}, nil
}
