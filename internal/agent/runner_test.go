package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cancanneed/internal/config"
	"cancanneed/internal/model"
)

func TestRunnerRetriesAProcessAndReadsStructuredResult(t *testing.T) {
	if os.Getenv("GO_WANT_RUNNER_HELPER") == "1" {
		runnerHelperProcess()
		return
	}
	dir := t.TempDir()
	retries := 1
	result, err := (Runner{}).Execute(context.Background(), Request{
		Agent: config.Agent{
			Type:         "pi",
			Command:      os.Args[0],
			Args:         []string{"-test.run=TestRunnerRetriesAProcessAndReadsStructuredResult", "--", "{prompt}"},
			Env:          map[string]string{"GO_WANT_RUNNER_HELPER": "1", "DUMMY_MARKER": filepath.Join(dir, "failed-once")},
			Timeout:      config.Duration(5 * time.Second),
			Retries:      &retries,
			RetryBackoff: config.Duration(time.Millisecond),
		},
		WorkingDir: dir,
		RunDir:     dir,
		Prompt:     "review this change",
		OutputPath: filepath.Join(dir, "review.json"),
		Environment: map[string]string{
			"CANCANNEED_REVIEW_OUTPUT": filepath.Join(dir, "review.json"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped || len(result.Findings) != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-attempt-2.stdout.log")); err != nil {
		t.Fatalf("second attempt log missing: %v", err)
	}
}

func TestReadResultRequiresFindingsArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readResult(path)
	if err == nil || !strings.Contains(err.Error(), "findings must be a JSON array") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMissingResultMeansSuccessfulReviewWithNoFindings(t *testing.T) {
	result, err := readResult(filepath.Join(t.TempDir(), "missing-review.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped || result.Findings == nil || len(result.Findings) != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunnerKeepsReportedFetchFailureWhenAgentExitsNonzero(t *testing.T) {
	if os.Getenv("GO_WANT_FETCH_FAILURE_HELPER") == "1" {
		_ = os.WriteFile(os.Getenv("FETCH_FAILURE_OUTPUT"), []byte(`{"fetch_failed":true,"fetch_error":"remote access denied","findings":[]}`), 0o600)
		os.Exit(7)
	}
	dir := t.TempDir()
	retries := 2
	result, err := (Runner{}).Execute(context.Background(), Request{
		Agent: config.Agent{
			Type: "pi", Command: os.Args[0],
			Args:    []string{"-test.run=TestRunnerKeepsReportedFetchFailureWhenAgentExitsNonzero", "--", "{prompt}"},
			Env:     map[string]string{"GO_WANT_FETCH_FAILURE_HELPER": "1", "FETCH_FAILURE_OUTPUT": filepath.Join(dir, "review.json")},
			Timeout: config.Duration(5 * time.Second), Retries: &retries,
		},
		WorkingDir: dir, RunDir: dir, Prompt: "review", OutputPath: filepath.Join(dir, "review.json"),
	})
	if err != nil || !result.FetchFailed || result.FetchError != "remote access denied" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-attempt-2.stdout.log")); !os.IsNotExist(err) {
		t.Fatalf("fetch failure unexpectedly retried: %v", err)
	}
}

func TestFetchFailedResultRequiresReasonAndNoFindings(t *testing.T) {
	for _, result := range []model.AgentResult{
		{FetchFailed: true, Findings: []model.Finding{}},
		{FetchFailed: true, FetchError: "remote unavailable", Findings: []model.Finding{{Title: "partial"}}},
		{FetchError: "remote unavailable", Findings: []model.Finding{}},
	} {
		if err := validateResult(result); err == nil {
			t.Fatalf("invalid fetch-failed result accepted: %#v", result)
		}
	}
}

func TestRunnerHonorsExecutionTimeout(t *testing.T) {
	if os.Getenv("GO_WANT_TIMEOUT_HELPER") == "1" {
		time.Sleep(5 * time.Second)
		return
	}
	dir := t.TempDir()
	retries := 0
	_, err := (Runner{}).Execute(context.Background(), Request{
		Agent: config.Agent{
			Type: "pi", Command: os.Args[0],
			Args: []string{"-test.run=TestRunnerHonorsExecutionTimeout", "--", "{prompt}"},
			Env:  map[string]string{"GO_WANT_TIMEOUT_HELPER": "1"}, Timeout: config.Duration(50 * time.Millisecond),
			Retries: &retries,
		},
		WorkingDir: dir,
		RunDir:     dir,
		Prompt:     "timeout",
		OutputPath: filepath.Join(dir, "review.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("unexpected timeout error: %v", err)
	}
}

func TestSkipResultCannotContainFindings(t *testing.T) {
	err := validateResult(model.AgentResult{
		Skipped:    true,
		SkipReason: "all commits opted out",
		Findings: []model.Finding{{
			Severity: "high", Author: "Alice", Commit: "1111111111111111111111111111111111111111",
			File: "main.go", Line: 1, Title: "bug", Detail: "details",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot contain findings") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func runnerHelperProcess() {
	marker := os.Getenv("DUMMY_MARKER")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		_ = os.WriteFile(marker, []byte("1"), 0o600)
		os.Exit(9)
	}
	result := `{"findings":[]}`
	if err := os.WriteFile(os.Getenv("CANCANNEED_REVIEW_OUTPUT"), []byte(result), 0o600); err != nil {
		os.Exit(10)
	}
	os.Exit(0)
}
