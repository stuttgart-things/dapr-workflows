package dagger

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Executor wraps dagger CLI subprocess calls.
type Executor struct {
	WorkDir string
}

// CallOpts defines parameters for a dagger call invocation.
type CallOpts struct {
	Module     string
	Function   string
	Args       map[string]string
	SecretEnvs map[string]string // env var name → secret value
	ExportPath string
	Timeout    time.Duration
}

// Call executes a dagger call command and returns stdout.
func (e *Executor) Call(ctx context.Context, opts CallOpts) (string, error) {
	args := []string{"call", "-m", opts.Module, opts.Function}

	for k, v := range opts.Args {
		args = append(args, fmt.Sprintf("--%s=%s", k, v))
	}

	if opts.ExportPath != "" {
		args = append(args, "export", "--path="+opts.ExportPath)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "dagger", args...)
	cmd.Dir = e.WorkDir

	// Inherit current environment
	cmd.Env = os.Environ()

	// Inject secret environment variables
	for envName, envVal := range opts.SecretEnvs {
		cmd.Env = append(cmd.Env, envName+"="+envVal)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dagger call %s %s failed: %w\nstderr: %s",
			opts.Module, opts.Function, err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}
