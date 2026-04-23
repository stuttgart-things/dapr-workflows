package types

// VsphereTemplateInput is the input to VsphereTemplateWorkflow — a minimal
// pipeline that builds a Packer vSphere template and promotes it to a
// golden image folder. Assumes the Packer config already exists on the
// target branch (no render/commit step).
type VsphereTemplateInput struct {
	RunID        string              `json:"runID"`
	Environment  string              `json:"environment"`  // "labul" | "labda"
	OSProfile    string              `json:"osProfile"`    // "ubuntu24" | "ubuntu26" | "rocky9" ...
	OSFamily     string              `json:"osFamily"`     // "ubuntu" | "rocky"
	Provisioning string              `json:"provisioning"` // "base-os" | "rke2-node"
	Branch       string              `json:"branch"`       // branch holding the packer build dir; defaults to "main"
	// ExistingTemplateName skips the Packer build step and promotes an
	// already-built template. Use this to recover when a previous run
	// orphaned a GH Actions build (e.g. Dapr crashed or GH dispatch 5xx'd
	// after queuing). Set to the timestamped Packer artifact name
	// (e.g. "ubuntu26-base-os-20260423-1428") and the workflow jumps
	// straight to the promote step.
	ExistingTemplateName string              `json:"existingTemplateName,omitempty"`
	GitHub               GitHubConfig        `json:"github"`
	Packer               VspherePackerRef    `json:"packer"`
	Promotion            VspherePromotionRef `json:"promotion"`
}

type VspherePackerRef struct {
	WorkflowFile  string `json:"workflowFile"` // e.g. "dispatch-packer-build-dagger.yaml"
	PackerVersion string `json:"packerVersion"`
	Runner        string `json:"runner"`
	DaggerVersion string `json:"daggerVersion"`
}

type VspherePromotionRef struct {
	WorkflowFile string `json:"workflowFile"` // e.g. "dispatch-packer-movetemplate.yaml"
	TargetName   string `json:"targetName"`   // required: golden image name, e.g. "sthings-u26"
	BuildFolder  string `json:"buildFolder"`  // optional override; empty → GH workflow derives from lab
	GoldenFolder string `json:"goldenFolder"` // optional override; empty → GH workflow derives from lab
	Runner       string `json:"runner"`
}

type VsphereTemplateOutput struct {
	RunID              string                  `json:"runID"`
	Status             string                  `json:"status"` // "running" | "success" | "failed"
	TemplateName       string                  `json:"templateName"`       // from packer build step
	GoldenTemplateName string                  `json:"goldenTemplateName"` // == promotion.targetName on success
	FailedStep         string                  `json:"failedStep,omitempty"`
	Error              string                  `json:"error,omitempty"`
	StepRunURLs        VsphereTemplateStepURLs `json:"stepRunUrls"`
}

type VsphereTemplateStepURLs struct {
	PackerBuild string `json:"packerBuild,omitempty"`
	Promote     string `json:"promote,omitempty"`
}
