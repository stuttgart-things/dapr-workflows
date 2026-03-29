package types

// GoldenImageBuildInput is the top-level input to the Dapr workflow.
type GoldenImageBuildInput struct {
	RunID       string         `json:"runID"`
	Environment string         `json:"environment"` // "labul", "labda"
	OSProfile   string         `json:"osProfile"`   // "ubuntu24", "rocky9"
	Render      RenderInput    `json:"render"`
	Git         GitInput       `json:"git"`
	Packer      PackerInput    `json:"packer"`
	TestVM      TestVMInput    `json:"testVM"`
	Promotion   PromotionInput `json:"promotion"`
	Notify      NotifyInput    `json:"notify"`
	Secrets     SecretsInput   `json:"secrets"`
}

type RenderInput struct {
	PackerTemplatesRepo string `json:"packerTemplatesRepo"`
	PackerTemplatesPath string `json:"packerTemplatesPath"`
	PackerTemplates     string `json:"packerTemplates"`
	TestVMTemplatesPath string `json:"testVmTemplatesPath"`
	TestVMTemplates     string `json:"testVmTemplates"`
	BuildPath           string `json:"buildPath"`
	EnvPath             string `json:"envPath"`
	VariablesFiles      string `json:"variablesFiles"`
	Overrides           string `json:"overrides"`
}

type GitInput struct {
	Repository       string `json:"repository"`
	BranchName       string `json:"branchName"`
	BaseBranch       string `json:"baseBranch"`
	CommitMessage    string `json:"commitMessage"`
	PackerDestPath   string `json:"packerDestPath"`
	TestVMDestPath   string `json:"testVmDestPath"`
	PullRequestTitle string `json:"pullRequestTitle"`
	PullRequestBody  string `json:"pullRequestBody"`
}

type PackerInput struct {
	ConfigFile    string `json:"configFile"`
	PackerVersion string `json:"packerVersion"`
	Arch          string `json:"arch"`
}

type TestVMInput struct {
	Enabled              bool   `json:"enabled"`
	VMName               string `json:"vmName"`
	AnsiblePlaybooks     string `json:"ansiblePlaybooks"`
	AnsibleParameters    string `json:"ansibleParameters"`
	AnsibleInventoryType string `json:"ansibleInventoryType"`
	AnsibleWaitTimeout   int    `json:"ansibleWaitTimeout"`
}

type PromotionInput struct {
	Enabled              bool   `json:"enabled"`
	GoldenTemplateName   string `json:"goldenTemplateName"`
	GoldenTemplateFolder string `json:"goldenTemplateFolder"`
}

type NotifyInput struct {
	Channels []string `json:"channels"`
	System   string   `json:"system"`
	Tags     string   `json:"tags"`
}

type SecretsInput struct {
	VaultAddr         string `json:"vaultAddr"`
	VaultAuthMethod   string `json:"vaultAuthMethod"`
	VaultRoleIDPath   string `json:"vaultRoleIdPath"`
	VaultSecretIDPath string `json:"vaultSecretIdPath"`
	VaultTokenPath    string `json:"vaultTokenPath"`
	GithubTokenPath   string `json:"githubTokenPath"`
	VCenterPath       string `json:"vcenterPath"`
	VCenterUserPath   string `json:"vcenterUserPath"`
	VCenterPassPath   string `json:"vcenterPassPath"`
	SSHUserPath       string `json:"sshUserPath"`
	SSHPasswordPath   string `json:"sshPasswordPath"`
}
