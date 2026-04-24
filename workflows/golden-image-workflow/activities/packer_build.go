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
	WorkflowFile  string `json:"workflowFile"`
	OSVersion     string `json:"osVersion"`
	Lab           string `json:"lab"`
	OSFamily      string `json:"osFamily"`
	Provisioning  string `json:"provisioning"`
	Branch        string `json:"branch"`        // branch with rendered packer config
	PackerVersion string `json:"packerVersion"`
	Runner        string `json:"runner"`
	DaggerVersion string `json:"daggerVersion"`
}

type PackerBuildOutput struct {
	RunID        int64  `json:"runID"`
	Conclusion   string `json:"conclusion"`
	RunURL       string `json:"runUrl"`
	TemplateName string `json:"templateName"`
	Message      string `json:"message"`
}

// PackerBuildActivity dispatches a GH Actions workflow for the Packer build and waits for completion.
func PackerBuildActivity(ctx workflow.ActivityContext) (any, error) {
	var input PackerBuildInput
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
		"branch":       input.Branch,
	}

	if input.PackerVersion != "" {
		inputs["packer-version"] = input.PackerVersion
	}
	if input.Runner != "" {
		inputs["runner"] = input.Runner
	}
	if input.DaggerVersion != "" {
		inputs["dagger-version"] = input.DaggerVersion
	}

	runID, err := client.DispatchAndFindRun(bgCtx, gh.DispatchWorkflowInput{
		Owner:        input.Owner,
		Repo:         input.Repo,
		WorkflowFile: input.WorkflowFile,
		Ref:          input.Ref,
		Inputs:       inputs,
	}, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("dispatch packer-build workflow: %w", err)
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

	// Extract template name from job logs
	templateName := ""
	logText, err := client.GetRunLog(bgCtx, input.Owner, input.Repo, runID)
	if err == nil {
		templateName = gh.ExtractFromLog(logText, "Template name")
	}

	return &PackerBuildOutput{
		RunID:        result.RunID,
		Conclusion:   result.Conclusion,
		RunURL:       result.HTMLURL,
		TemplateName: templateName,
		Message:      fmt.Sprintf("packer build completed for %s/%s, template: %s", input.Lab, input.OSVersion, templateName),
	}, nil
}
