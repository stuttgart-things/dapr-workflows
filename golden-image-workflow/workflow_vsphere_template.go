package main

import (
	"fmt"
	"time"

	"github.com/dapr/durabletask-go/workflow"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/activities"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/types"
)

// VsphereTemplateWorkflow dispatches a Packer build GH Actions workflow,
// captures the resulting template name, then dispatches the promote
// (rename/delete/move) workflow with that name as input.
//
// If input.ExistingTemplateName is set, the Packer build step is skipped
// and the workflow promotes the given template. Use this to recover from
// orphaned GH runs where a previous workflow failed after dispatching
// (e.g. GH dispatch returned 5xx after queuing, or the worker crashed).
func VsphereTemplateWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var input types.VsphereTemplateInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get workflow input: %w", err)
	}

	branch := input.Branch
	if branch == "" {
		branch = "main"
	}

	output := types.VsphereTemplateOutput{
		RunID:  input.RunID,
		Status: "running",
	}

	gh := input.GitHub

	// Step 1: Packer build (unless we're adopting an existing template).
	var templateName string
	if input.ExistingTemplateName != "" {
		fmt.Printf("resume mode: skipping packer build, promoting existing template %q\n", input.ExistingTemplateName)
		templateName = input.ExistingTemplateName
		output.TemplateName = templateName
	} else {
		packerInput := activities.PackerBuildInput{
			Owner:         gh.Owner,
			Repo:          gh.Repo,
			Ref:           gh.Ref,
			Token:         gh.Token,
			WorkflowFile:  input.Packer.WorkflowFile,
			OSVersion:     input.OSProfile,
			Lab:           input.Environment,
			OSFamily:      input.OSFamily,
			Provisioning:  input.Provisioning,
			Branch:        branch,
			PackerVersion: input.Packer.PackerVersion,
			Runner:        input.Packer.Runner,
			DaggerVersion: input.Packer.DaggerVersion,
		}

		// Activity retry guards worker-level blips. MaxAttempts=2 so we don't
		// redundantly kick off a second long-running packer build unless the
		// first attempt failed before any real work — the activity's internal
		// dispatch-with-recovery is the primary defense.
		packerRetry := &workflow.RetryPolicy{
			MaxAttempts:          2,
			InitialRetryInterval: 30 * time.Second,
			BackoffCoefficient:   2.0,
			MaxRetryInterval:     2 * time.Minute,
			RetryTimeout:         5 * time.Minute,
		}

		var packerOutput activities.PackerBuildOutput
		if err := ctx.CallActivity(activities.PackerBuildActivity,
			workflow.WithActivityInput(packerInput),
			workflow.WithActivityRetryPolicy(packerRetry),
		).Await(&packerOutput); err != nil {
			output.Status = "failed"
			output.FailedStep = "PackerBuild"
			output.Error = err.Error()
			output.StepRunURLs.PackerBuild = packerOutput.RunURL
			return &output, err
		}
		output.StepRunURLs.PackerBuild = packerOutput.RunURL
		output.TemplateName = packerOutput.TemplateName
		fmt.Printf("packer build completed: %s\n", packerOutput.Message)

		if packerOutput.TemplateName == "" {
			output.Status = "failed"
			output.FailedStep = "PackerBuild"
			output.Error = "template name not found in packer build log"
			return &output, fmt.Errorf("template name not found in packer build log")
		}
		templateName = packerOutput.TemplateName
	}

	// Step 2: Promote (rename → delete existing → move).
	// Promote is short-running and safe to retry on transient errors.
	promoteInput := activities.PromoteInput{
		Owner:        gh.Owner,
		Repo:         gh.Repo,
		Ref:          gh.Ref,
		Token:        gh.Token,
		WorkflowFile: input.Promotion.WorkflowFile,
		TemplateName: templateName,
		TargetName:   input.Promotion.TargetName,
		BuildFolder:  input.Promotion.BuildFolder,
		GoldenFolder: input.Promotion.GoldenFolder,
		Runner:       input.Promotion.Runner,
	}

	promoteRetry := &workflow.RetryPolicy{
		MaxAttempts:          3,
		InitialRetryInterval: 10 * time.Second,
		BackoffCoefficient:   2.0,
		MaxRetryInterval:     60 * time.Second,
		RetryTimeout:         10 * time.Minute,
	}

	var promoteOutput activities.PromoteOutput
	if err := ctx.CallActivity(activities.PromoteActivity,
		workflow.WithActivityInput(promoteInput),
		workflow.WithActivityRetryPolicy(promoteRetry),
	).Await(&promoteOutput); err != nil {
		output.Status = "failed"
		output.FailedStep = "Promote"
		output.Error = err.Error()
		output.StepRunURLs.Promote = promoteOutput.RunURL
		return &output, err
	}
	output.StepRunURLs.Promote = promoteOutput.RunURL
	output.GoldenTemplateName = input.Promotion.TargetName
	output.Status = "success"
	fmt.Printf("promotion completed: %s\n", promoteOutput.Message)

	return &output, nil
}
