package activities

import (
	"fmt"

	"github.com/dapr/durabletask-go/workflow"
)

type RenderConfigInput struct {
	Environment string `json:"environment"`
	OSProfile   string `json:"osProfile"`
	Overrides   string `json:"overrides"`
}

type RenderConfigOutput struct {
	RenderedDir string `json:"renderedDir"`
	Message     string `json:"message"`
}

// RenderConfigActivity is a placeholder that will later wrap dagger call vmtemplate render-build-config.
func RenderConfigActivity(ctx workflow.ActivityContext) (any, error) {
	var input RenderConfigInput
	if err := ctx.GetInput(&input); err != nil {
		return nil, fmt.Errorf("failed to get input: %w", err)
	}

	// TODO: Replace with actual dagger call via Executor
	msg := fmt.Sprintf("rendered config for %s/%s (overrides: %s)",
		input.Environment, input.OSProfile, input.Overrides)

	return &RenderConfigOutput{
		RenderedDir: fmt.Sprintf("/tmp/rendered/%s-%s", input.Environment, input.OSProfile),
		Message:     msg,
	}, nil
}
