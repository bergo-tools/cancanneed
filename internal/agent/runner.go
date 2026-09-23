package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cancanneed/internal/config"
	"cancanneed/internal/model"
)

const maxResultBytes = 2 << 20

type Request struct {
	Agent       config.Agent
	WorkingDir  string
	RunDir      string
	Prompt      string
	OutputPath  string
	Environment map[string]string
	Template    map[string]string
}

// Runner launches an agent as a child process and validates its submitted JSON.
type Runner struct{}

func (Runner) Execute(ctx context.Context, request Request) (model.AgentResult, error) {
	var failures []error
	totalAttempts := request.Agent.RetryCount() + 1
	attempted := 0
	for attempt := 1; attempt <= totalAttempts; attempt++ {
		attempted++
		_ = os.Remove(request.OutputPath)
		result, err := runAttempt(ctx, request, attempt)
		if err == nil {
			return result, nil
		}
		failures = append(failures, fmt.Errorf("attempt %d: %w", attempt, err))
		if attempt < totalAttempts {
			if err := wait(ctx, request.Agent.RetryBackoff.Value()); err != nil {
				failures = append(failures, err)
				break
			}
		}
	}
	return model.AgentResult{}, fmt.Errorf("agent failed after %d attempt(s): %w", attempted, errors.Join(failures...))
}

func runAttempt(parent context.Context, request Request, attempt int) (model.AgentResult, error) {
	ctx, cancel := context.WithTimeout(parent, request.Agent.Timeout.Value())
	defer cancel()

	values := make(map[string]string, len(request.Template)+1)
	for key, value := range request.Template {
		values[key] = value
	}
	values["prompt"] = request.Prompt
	args, containsPrompt := expandArgs(request.Agent.Args, values)
	if !containsPrompt {
		args = append(args, request.Prompt)
	}

	stdoutPath := filepath.Join(request.RunDir, fmt.Sprintf("agent-attempt-%d.stdout.log", attempt))
	stderrPath := filepath.Join(request.RunDir, fmt.Sprintf("agent-attempt-%d.stderr.log", attempt))
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return model.AgentResult{}, fmt.Errorf("open stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return model.AgentResult{}, fmt.Errorf("open stderr log: %w", err)
	}
	defer stderr.Close()

	cmd := exec.CommandContext(ctx, request.Agent.Command, args...)
	configureProcessGroup(cmd)
	cmd.Dir = request.WorkingDir
	cmd.Env = mergedEnvironment(request.Agent.Env, request.Environment)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if result, readErr := readResult(request.OutputPath); readErr == nil && result.FetchFailed {
			return result, nil
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return model.AgentResult{}, fmt.Errorf("timed out after %s", request.Agent.Timeout.Value())
		}
		return model.AgentResult{}, fmt.Errorf("run %s: %w (logs: %s, %s)", request.Agent.Command, err, stdoutPath, stderrPath)
	}

	result, err := readResult(request.OutputPath)
	if err != nil {
		return model.AgentResult{}, fmt.Errorf("validate submitted review: %w (logs: %s, %s)", err, stdoutPath, stderrPath)
	}
	return result, nil
}

func expandArgs(args []string, values map[string]string) ([]string, bool) {
	result := make([]string, len(args))
	containsPrompt := false
	for i, arg := range args {
		for key, value := range values {
			placeholder := "{" + key + "}"
			if strings.Contains(arg, placeholder) {
				arg = strings.ReplaceAll(arg, placeholder, value)
				if key == "prompt" {
					containsPrompt = true
				}
			}
		}
		result[i] = arg
	}
	return result, containsPrompt
}

func mergedEnvironment(groups ...map[string]string) []string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		if key, value, ok := strings.Cut(item, "="); ok {
			values[key] = value
		}
	}
	for _, group := range groups {
		for key, value := range group {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func readResult(path string) (model.AgentResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return model.AgentResult{Findings: []model.Finding{}}, nil
	}
	if err != nil {
		return model.AgentResult{}, err
	}
	defer f.Close()

	limited := io.LimitReader(f, maxResultBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return model.AgentResult{}, err
	}
	if len(b) > maxResultBytes {
		return model.AgentResult{}, fmt.Errorf("result exceeds %d bytes", maxResultBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var result model.AgentResult
	if err := decoder.Decode(&result); err != nil {
		return model.AgentResult{}, fmt.Errorf("decode JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return model.AgentResult{}, err
	}
	if err := validateResult(result); err != nil {
		return model.AgentResult{}, err
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return errors.New("multiple JSON values are not allowed")
}

func validateResult(result model.AgentResult) error {
	if result.Findings == nil {
		return errors.New("findings must be a JSON array")
	}
	if result.FetchFailed {
		if strings.TrimSpace(result.FetchError) == "" {
			return errors.New("fetch_error is required when fetch_failed is true")
		}
		if result.Skipped || result.SkipReason != "" || len(result.Findings) != 0 {
			return errors.New("fetch-failed result cannot contain skipped review or findings")
		}
	} else if result.FetchError != "" {
		return errors.New("fetch_error requires fetch_failed=true")
	}
	if result.Skipped {
		if strings.TrimSpace(result.SkipReason) == "" {
			return errors.New("skip_reason is required when skipped is true")
		}
		if len(result.Findings) != 0 {
			return errors.New("skipped result cannot contain findings")
		}
	} else if result.SkipReason != "" {
		return errors.New("skip_reason requires skipped=true")
	}
	for i, finding := range result.Findings {
		if strings.TrimSpace(finding.Author) == "" {
			return fmt.Errorf("findings[%d].author is required", i)
		}
		if !validObjectID(finding.Commit) {
			return fmt.Errorf("findings[%d].commit must be a full commit SHA", i)
		}
		switch finding.Severity {
		case "critical", "high", "medium", "low", "info":
		default:
			return fmt.Errorf("findings[%d].severity %q is invalid", i, finding.Severity)
		}
		if strings.TrimSpace(finding.File) == "" || strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Detail) == "" {
			return fmt.Errorf("findings[%d].file, title, and detail are required", i)
		}
		if finding.Line <= 0 {
			return fmt.Errorf("findings[%d].line must be positive", i)
		}
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
