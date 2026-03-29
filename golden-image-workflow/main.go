package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/dapr/durabletask-go/workflow"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/activities"
	"github.com/stuttgart-things/dapr-workflows/golden-image-workflow/types"
)

func main() {
	// Create workflow registry and register workflow + activities
	r := workflow.NewRegistry()
	if err := r.AddWorkflowN("GoldenImageBuildWorkflow", GoldenImageBuildWorkflow); err != nil {
		log.Fatalf("failed to register workflow: %v", err)
	}

	activityRegistrations := map[string]func(workflow.ActivityContext) (any, error){
		"RenderConfigActivity": activities.RenderConfigActivity,
		"CommitPRActivity":     activities.CommitPRActivity,
		"PackerBuildActivity":  activities.PackerBuildActivity,
		"TestVMActivity":       activities.TestVMActivity,
		"PromoteActivity":      activities.PromoteActivity,
		"NotifyActivity":       activities.NotifyActivity,
	}

	for name, fn := range activityRegistrations {
		if err := r.AddActivityN(name, fn); err != nil {
			log.Fatalf("failed to register activity %s: %v", name, err)
		}
	}

	// Create workflow client via Dapr sidecar
	wfClient, err := dapr.NewWorkflowClient()
	if err != nil {
		log.Fatalf("failed to create workflow client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the workflow worker in background
	go func() {
		if err := wfClient.StartWorker(ctx, r); err != nil {
			log.Fatalf("workflow worker error: %v", err)
		}
	}()

	// Give the worker a moment to connect to the Dapr sidecar
	time.Sleep(2 * time.Second)
	log.Println("workflow worker started")

	// If --run flag is passed, start a test workflow instance and exit
	if len(os.Args) > 1 && os.Args[1] == "--run" {
		if err := runTestWorkflow(ctx, wfClient); err != nil {
			log.Fatalf("test workflow failed: %v", err)
		}
		return
	}

	// Otherwise, start HTTP server for API triggers
	mux := http.NewServeMux()
	mux.HandleFunc("/start", startWorkflowHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		log.Println("golden-image-workflow listening on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Wait for interrupt
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-sigCtx.Done()

	log.Println("shutting down...")
	cancel()
	server.Shutdown(context.Background())
}

func runTestWorkflow(ctx context.Context, wfClient *workflow.Client) error {
	input := types.GoldenImageBuildInput{
		RunID:       "test-001",
		Environment: "labul",
		OSProfile:   "ubuntu24",
		GitHub: types.GitHubConfig{
			Owner: "stuttgart-things",
			Repo:  "stuttgart-things",
			Ref:   "main",
			Token: os.Getenv("GITHUB_TOKEN"),
		},
		Render: types.RenderInput{
			WorkflowFile:  "dispatch-render-packer-config.yaml",
			OSFamily:      "ubuntu",
			Provisioning:  "base-os",
			Overrides:     "vm_name=test-golden",
			CreatePR:      "false",
			RenderOnly:    "true",
			DaggerVersion: "0.20.0",
			Runner:        "dagger-labul",
		},
		Git: types.GitInput{
			BranchName:       "golden/labul-ubuntu24",
			BaseBranch:       "main",
			CommitMessage:    "feat: render golden image config for labul/ubuntu24",
			PullRequestTitle: "Golden image: labul/ubuntu24",
			PullRequestBody:  "Automated golden image build",
		},
		Packer: types.PackerInput{
			ConfigFile:    "packer.pkr.hcl",
			PackerVersion: "1.11",
			Arch:          "amd64",
		},
	}

	instanceID := fmt.Sprintf("test-%d", time.Now().Unix())

	id, err := wfClient.ScheduleWorkflow(ctx, "GoldenImageBuildWorkflow",
		workflow.WithInstanceID(instanceID),
		workflow.WithInput(input),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule workflow: %w", err)
	}
	log.Printf("workflow started: instanceID=%s", id)

	// Wait for completion
	meta, err := wfClient.WaitForWorkflowCompletion(ctx, id)
	if err != nil {
		return fmt.Errorf("failed waiting for workflow: %w", err)
	}

	log.Printf("workflow completed: status=%s", meta.String())

	if meta.FailureDetails != nil {
		log.Printf("failure: %s: %s", meta.FailureDetails.ErrorType, meta.FailureDetails.ErrorMessage)
	}

	if meta.Output != nil {
		var output types.GoldenImageBuildOutput
		if err := json.Unmarshal([]byte(meta.Output.GetValue()), &output); err != nil {
			log.Printf("could not deserialize output: %v", err)
			log.Printf("raw output: %s", meta.Output.GetValue())
		} else {
			result, _ := json.MarshalIndent(output, "", "  ")
			log.Printf("workflow output:\n%s", string(result))
		}
	}

	return nil
}

func startWorkflowHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input types.GoldenImageBuildInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, fmt.Sprintf("invalid input: %v", err), http.StatusBadRequest)
		return
	}

	wfClient, err := dapr.NewWorkflowClient()
	if err != nil {
		http.Error(w, fmt.Sprintf("workflow client error: %v", err), http.StatusInternalServerError)
		return
	}

	instanceID := fmt.Sprintf("%s-%s-%d", input.Environment, input.OSProfile, time.Now().Unix())
	if input.RunID != "" {
		instanceID = input.RunID
	}

	id, err := wfClient.ScheduleWorkflow(r.Context(), "GoldenImageBuildWorkflow",
		workflow.WithInstanceID(instanceID),
		workflow.WithInput(input),
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to start workflow: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"instanceID": id,
		"status":     "started",
	})
}
