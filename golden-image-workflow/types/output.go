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
}

type TestResult struct {
	Passed      bool   `json:"passed"`
	VMName      string `json:"vmName"`
	VMDestroyed bool   `json:"vmDestroyed"`
	AnsibleLog  string `json:"ansibleLog"`
}
