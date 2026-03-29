package main

import (
	"fmt"

	"github.com/dapr/durabletask-go/workflow"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/activities"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/types"
)

// GoldenImageBuildWorkflow orchestrates the golden image build pipeline.
// Currently only the RenderConfig step is wired — more activities will be added incrementally.
func GoldenImageBuildWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var input types.GoldenImageBuildInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get workflow input: %w", err)
	}

	output := types.GoldenImageBuildOutput{
		RunID:  input.RunID,
		Status: "running",
	}

	// Step 1: Render config
	renderInput := activities.RenderConfigInput{
		Environment: input.Environment,
		OSProfile:   input.OSProfile,
		Overrides:   input.Render.Overrides,
	}

	var renderOutput activities.RenderConfigOutput
	if err := ctx.CallActivity(activities.RenderConfigActivity, workflow.WithActivityInput(renderInput)).Await(&renderOutput); err != nil {
		output.Status = "failed"
		output.FailedStep = "RenderConfig"
		output.Error = err.Error()
		return &output, err
	}

	fmt.Printf("render completed: %s\n", renderOutput.Message)

	// TODO: Step 2 — CommitAndPR
	// TODO: Step 3 — PackerBuild
	// TODO: Step 4 — TestVM
	// TODO: Step 5 — PromoteGoldenImage
	// TODO: Step 6 — Notify

	output.Status = "success"
	return &output, nil
}
