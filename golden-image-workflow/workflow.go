package main

import (
	"fmt"
	"strings"

	"github.com/dapr/durabletask-go/workflow"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/activities"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/types"
)

// GoldenImageBuildWorkflow orchestrates the golden image build pipeline.
// Each step dispatches a GitHub Actions workflow and waits for completion.
func GoldenImageBuildWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var input types.GoldenImageBuildInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get workflow input: %w", err)
	}

	output := types.GoldenImageBuildOutput{
		RunID:  input.RunID,
		Status: "running",
	}

	gh := input.GitHub

	// Step 1: Render config + commit + PR (all handled by the render GH Action)
	renderInput := activities.RenderConfigInput{
		Owner:         gh.Owner,
		Repo:          gh.Repo,
		Ref:           gh.Ref,
		Token:         gh.Token,
		WorkflowFile:  input.Render.WorkflowFile,
		OSVersion:     input.OSProfile,
		Lab:           input.Environment,
		OSFamily:      input.Render.OSFamily,
		Provisioning:  input.Render.Provisioning,
		Overrides:     input.Render.Overrides,
		CreatePR:      input.Render.CreatePR,
		RenderOnly:    input.Render.RenderOnly,
		DaggerVersion: input.Render.DaggerVersion,
		Runner:        input.Render.Runner,
	}

	var renderOutput activities.RenderConfigOutput
	if err := ctx.CallActivity(activities.RenderConfigActivity, workflow.WithActivityInput(renderInput)).Await(&renderOutput); err != nil {
		output.Status = "failed"
		output.FailedStep = "RenderConfig"
		output.Error = err.Error()
		output.StepRunURLs.Render = renderOutput.RunURL
		return &output, err
	}
	output.StepRunURLs.Render = renderOutput.RunURL
	fmt.Printf("render + commit completed: %s\n", renderOutput.Message)

	// Step 2: Packer build
	// The branch name comes from the render step's convention:
	// feat/rendered-{os-version}-{lab}-{provisioning}
	packerBranch := fmt.Sprintf("feat/rendered-%s-%s-%s", input.OSProfile, input.Environment, input.Render.Provisioning)
	packerInput := activities.PackerBuildInput{
		Owner:         gh.Owner,
		Repo:          gh.Repo,
		Ref:           gh.Ref,
		Token:         gh.Token,
		WorkflowFile:  input.Packer.WorkflowFile,
		OSVersion:     input.OSProfile,
		Lab:           input.Environment,
		OSFamily:      input.Render.OSFamily,
		Provisioning:  input.Render.Provisioning,
		Branch:        packerBranch,
		PackerVersion: input.Packer.PackerVersion,
		Runner:        input.Packer.Runner,
		DaggerVersion: input.Packer.DaggerVersion,
	}

	var packerOutput activities.PackerBuildOutput
	if err := ctx.CallActivity(activities.PackerBuildActivity, workflow.WithActivityInput(packerInput)).Await(&packerOutput); err != nil {
		output.Status = "failed"
		output.FailedStep = "PackerBuild"
		output.Error = err.Error()
		output.StepRunURLs.PackerBuild = packerOutput.RunURL
		return &output, err
	}
	output.StepRunURLs.PackerBuild = packerOutput.RunURL
	output.TemplateName = packerOutput.TemplateName
	fmt.Printf("packer build completed: %s\n", packerOutput.Message)

	// Step 3: Test VM (optional)
	if input.TestVM.Enabled {
		testVMInput := activities.TestVMInput{
			Owner:         gh.Owner,
			Repo:          gh.Repo,
			Ref:           gh.Ref,
			Token:         gh.Token,
			WorkflowFile:  input.TestVM.WorkflowFile,
			TemplateName:  packerOutput.TemplateName,
			OSVersion:     input.OSProfile,
			Lab:           input.Environment,
			OSFamily:      input.Render.OSFamily,
			TestPlaybooks: input.TestVM.TestPlaybooks,
			Overrides:     input.TestVM.Overrides,
			Runner:        input.TestVM.Runner,
			DaggerVersion: input.TestVM.DaggerVersion,
		}

		var testVMOutput activities.TestVMOutput
		if err := ctx.CallActivity(activities.TestVMActivity, workflow.WithActivityInput(testVMInput)).Await(&testVMOutput); err != nil {
			output.Status = "failed"
			output.FailedStep = "TestVM"
			output.Error = err.Error()
			output.StepRunURLs.TestVM = testVMOutput.RunURL
			return &output, err
		}
		output.StepRunURLs.TestVM = testVMOutput.RunURL
		output.TestResults.Passed = true
		output.TestResults.VMName = packerOutput.TemplateName
		fmt.Printf("test VM completed: %s\n", testVMOutput.Message)
	}

	// Step 4: Promote golden image (optional)
	if input.Promotion.Enabled {
		promoteInput := activities.PromoteInput{
			Owner:        gh.Owner,
			Repo:         gh.Repo,
			Ref:          gh.Ref,
			Token:        gh.Token,
			WorkflowFile: input.Promotion.WorkflowFile,
			TemplateName: packerOutput.TemplateName,
			TargetName:   input.Promotion.TargetName,
			Lab:          input.Environment,
			BuildFolder:  input.Promotion.BuildFolder,
			GoldenFolder: input.Promotion.GoldenFolder,
			Runner:       input.Promotion.Runner,
		}

		var promoteOutput activities.PromoteOutput
		if err := ctx.CallActivity(activities.PromoteActivity, workflow.WithActivityInput(promoteInput)).Await(&promoteOutput); err != nil {
			output.Status = "failed"
			output.FailedStep = "Promote"
			output.Error = err.Error()
			output.StepRunURLs.Promote = promoteOutput.RunURL
			return &output, err
		}
		output.StepRunURLs.Promote = promoteOutput.RunURL
		output.PromotionStatus = "promoted"
		output.GoldenTemplateName = input.Promotion.TargetName
		fmt.Printf("promotion completed: %s\n", promoteOutput.Message)
	}

	// Step 6: Notify
	if len(input.Notify.Channels) > 0 {
		notifyInput := activities.NotifyInput{
			Owner:        gh.Owner,
			Repo:         gh.Repo,
			Ref:          gh.Ref,
			Token:        gh.Token,
			WorkflowFile: "notify.yaml",
			Channels:     strings.Join(input.Notify.Channels, ","),
			System:       input.Notify.System,
			Tags:         input.Notify.Tags,
			Status:       "success",
		}

		var notifyOutput activities.NotifyOutput
		if err := ctx.CallActivity(activities.NotifyActivity, workflow.WithActivityInput(notifyInput)).Await(&notifyOutput); err != nil {
			// Notification failure is non-fatal — log but continue
			fmt.Printf("notify failed (non-fatal): %s\n", err.Error())
			output.StepRunURLs.Notify = notifyOutput.RunURL
		} else {
			output.StepRunURLs.Notify = notifyOutput.RunURL
			fmt.Printf("notification completed: %s\n", notifyOutput.Message)
		}
	}

	output.Status = "success"
	return &output, nil
}
