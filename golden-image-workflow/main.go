package main

import (
	"bytes"
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
	if err := r.AddWorkflowN("VsphereTemplateWorkflow", VsphereTemplateWorkflow); err != nil {
		log.Fatalf("failed to register workflow: %v", err)
	}

	activityRegistrations := map[string]func(workflow.ActivityContext) (any, error){
		"RenderConfigActivity": activities.RenderConfigActivity,
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

	// If --run flag is passed, start a workflow from JSON input file and exit.
	// Usage:
	//   --run --input <file.json>                               (defaults to GoldenImageBuildWorkflow)
	//   --run --workflow <Name> --input <file.json>
	if len(os.Args) > 1 && os.Args[1] == "--run" {
		inputFile := ""
		workflowName := "GoldenImageBuildWorkflow"
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--input":
				if i+1 < len(args) {
					inputFile = args[i+1]
					i++
				}
			case "--workflow":
				if i+1 < len(args) {
					workflowName = args[i+1]
					i++
				}
			default:
				// legacy positional form: --run <file.json>
				if inputFile == "" {
					inputFile = args[i]
				}
			}
		}

		if inputFile == "" {
			log.Fatal("usage: golden-image-workflow --run [--workflow <Name>] --input <file.json>")
		}

		if err := runWorkflowFromFile(ctx, wfClient, workflowName, inputFile); err != nil {
			log.Fatalf("workflow failed: %v", err)
		}
		return
	}

	// Otherwise, start HTTP server for API triggers
	mux := http.NewServeMux()
	mux.HandleFunc("/start", startWorkflowHandler)
	mux.HandleFunc("/start-vsphere-template", startVsphereTemplateHandler)
	mux.HandleFunc("/status/", statusWorkflowHandler)
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

func runWorkflowFromFile(ctx context.Context, wfClient *workflow.Client, workflowName, inputFile string) error {
	data, err := os.ReadFile(inputFile)
	if err != nil {
		return fmt.Errorf("read input file %s: %w", inputFile, err)
	}

	var (
		inputPayload any
		instanceID   string
	)

	switch workflowName {
	case "GoldenImageBuildWorkflow":
		var input types.GoldenImageBuildInput
		if err := json.Unmarshal(data, &input); err != nil {
			return fmt.Errorf("parse input file: %w", err)
		}
		if input.GitHub.Token == "" {
			input.GitHub.Token = os.Getenv("GITHUB_TOKEN")
		}
		if input.GitHub.Token == "" {
			return fmt.Errorf("no GitHub token: set 'github.token' in JSON or GITHUB_TOKEN env var")
		}
		instanceID = fmt.Sprintf("%s-%s-%d", input.Environment, input.OSProfile, time.Now().Unix())
		if input.RunID != "" {
			instanceID = input.RunID
		}
		inputPayload = input

	case "VsphereTemplateWorkflow":
		var input types.VsphereTemplateInput
		if err := json.Unmarshal(data, &input); err != nil {
			return fmt.Errorf("parse input file: %w", err)
		}
		if input.GitHub.Token == "" {
			input.GitHub.Token = os.Getenv("GITHUB_TOKEN")
		}
		if input.GitHub.Token == "" {
			return fmt.Errorf("no GitHub token: set 'github.token' in JSON or GITHUB_TOKEN env var")
		}
		instanceID = fmt.Sprintf("vsphere-%s-%s-%d", input.Environment, input.OSProfile, time.Now().Unix())
		if input.RunID != "" {
			instanceID = input.RunID
		}
		inputPayload = input

	default:
		return fmt.Errorf("unknown workflow name: %s", workflowName)
	}

	id, err := wfClient.ScheduleWorkflow(ctx, workflowName,
		workflow.WithInstanceID(instanceID),
		workflow.WithInput(inputPayload),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule workflow: %w", err)
	}
	log.Printf("workflow started: name=%s instanceID=%s", workflowName, id)

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
		raw := meta.Output.GetValue()
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(raw), "", "  "); err == nil {
			log.Printf("workflow output:\n%s", pretty.String())
		} else {
			log.Printf("raw output: %s", raw)
		}
	}

	return nil
}

func statusWorkflowHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract instance ID from path: /status/{instanceID}
	instanceID := r.URL.Path[len("/status/"):]
	if instanceID == "" {
		http.Error(w, "instance ID required: /status/{instanceID}", http.StatusBadRequest)
		return
	}

	wfClient, err := dapr.NewWorkflowClient()
	if err != nil {
		http.Error(w, fmt.Sprintf("workflow client error: %v", err), http.StatusInternalServerError)
		return
	}

	meta, err := wfClient.FetchWorkflowMetadata(r.Context(), instanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch workflow: %v", err), http.StatusNotFound)
		return
	}

	response := map[string]any{
		"instanceID":    meta.InstanceId,
		"runtimeStatus": meta.RuntimeStatus.String(),
		"createdAt":     meta.CreatedAt,
		"lastUpdatedAt": meta.LastUpdatedAt,
	}

	if meta.Output != nil {
		var out json.RawMessage
		if err := json.Unmarshal([]byte(meta.Output.GetValue()), &out); err == nil {
			response["output"] = out
		}
	}

	if meta.FailureDetails != nil {
		response["failure"] = map[string]string{
			"errorType":    meta.FailureDetails.ErrorType,
			"errorMessage": meta.FailureDetails.ErrorMessage,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func startVsphereTemplateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input types.VsphereTemplateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, fmt.Sprintf("invalid input: %v", err), http.StatusBadRequest)
		return
	}

	if input.GitHub.Token == "" {
		input.GitHub.Token = os.Getenv("GITHUB_TOKEN")
	}

	wfClient, err := dapr.NewWorkflowClient()
	if err != nil {
		http.Error(w, fmt.Sprintf("workflow client error: %v", err), http.StatusInternalServerError)
		return
	}

	instanceID := fmt.Sprintf("vsphere-%s-%s-%d", input.Environment, input.OSProfile, time.Now().Unix())
	if input.RunID != "" {
		instanceID = input.RunID
	}

	id, err := wfClient.ScheduleWorkflow(r.Context(), "VsphereTemplateWorkflow",
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

	// Allow GITHUB_TOKEN from env if not in payload
	if input.GitHub.Token == "" {
		input.GitHub.Token = os.Getenv("GITHUB_TOKEN")
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
