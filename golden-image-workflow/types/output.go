package types

// GoldenImageBuildOutput is the workflow result.
type GoldenImageBuildOutput struct {
	RunID              string     `json:"runID"`
	Status             string     `json:"status"`
	TemplateName       string     `json:"templateName"`
	GoldenTemplateName string     `json:"goldenTemplateName"`
	PRUrl              string     `json:"prUrl"`
	TestResults        TestResult `json:"testResults"`
	PromotionStatus    string     `json:"promotionStatus"`
	FailedStep         string     `json:"failedStep"`
	Error              string     `json:"error"`
	StartedAt          string     `json:"startedAt"`
	CompletedAt        string     `json:"completedAt"`
	DurationSeconds    int        `json:"durationSeconds"`
	StepRunURLs        StepRunURLs `json:"stepRunUrls"`
}

// StepRunURLs holds the GitHub Actions run URLs for each pipeline step (for observability).
type StepRunURLs struct {
	Render     string `json:"render,omitempty"`
	CommitPR   string `json:"commitPr,omitempty"`
	PackerBuild string `json:"packerBuild,omitempty"`
	TestVM     string `json:"testVm,omitempty"`
	Promote    string `json:"promote,omitempty"`
	Notify     string `json:"notify,omitempty"`
}

type TestResult struct {
	Passed      bool   `json:"passed"`
	VMName      string `json:"vmName"`
	VMDestroyed bool   `json:"vmDestroyed"`
	AnsibleLog  string `json:"ansibleLog"`
}
