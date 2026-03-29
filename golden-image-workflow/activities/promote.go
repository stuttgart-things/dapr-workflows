package activities

import (
	"context"
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	gh "github.com/stuttgart-things/dapr-workflows/golden-image-workflow/github"
)

type PromoteInput struct {
	Owner              string `json:"owner"`
	Repo               string `json:"repo"`
	Ref                string `json:"ref"`
	Token              string `json:"token"`
	WorkflowFile       string `json:"workflowFile"` // e.g. "promote.yaml"
	TemplateName       string `json:"templateName"`
	GoldenTemplateName string `json:"goldenTemplateName"`
	TemplateFolder     string `json:"templateFolder"`
	Environment        string `json:"environment"`
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

	dispatchTime := time.Now()
	err := client.DispatchWorkflow(bgCtx, gh.DispatchWorkflowInput{
		Owner:        input.Owner,
		Repo:         input.Repo,
		WorkflowFile: input.WorkflowFile,
		Ref:          input.Ref,
		Inputs: map[string]string{
			"template_name":        input.TemplateName,
			"golden_template_name": input.GoldenTemplateName,
			"template_folder":      input.TemplateFolder,
			"environment":          input.Environment,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch promote workflow: %w", err)
	}

	runID, err := client.FindRunByDispatch(bgCtx, input.Owner, input.Repo, input.WorkflowFile, dispatchTime, 10*time.Second, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("find promote run: %w", err)
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
		Message:    fmt.Sprintf("promoted %s to %s", input.TemplateName, input.GoldenTemplateName),
	}, nil
}
