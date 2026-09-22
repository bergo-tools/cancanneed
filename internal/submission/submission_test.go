package submission

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cancanneed/internal/model"
)

func TestConcurrentFindingSubmissionsProduceOneValidResult(t *testing.T) {
	repository, commit := createTestRepository(t)
	output := filepath.Join(t.TempDir(), "review.json")
	const count = 32
	errorsByCall := make(chan error, count)
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsByCall <- Run([]string{
				"--output", output,
				"--repository", repository,
				"finding",
				"--author", fmt.Sprintf("author-%d", i),
				"--commit", commit,
				"--file", fmt.Sprintf("file-%d.go", i),
				"--line", fmt.Sprintf("%d", i+1),
				"--severity", "medium",
				"--title", fmt.Sprintf("finding-%d", i),
				"--detail", "details",
			})
		}()
	}
	workers.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result model.AgentResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	if result.Skipped {
		t.Fatalf("review unexpectedly skipped: %#v", result)
	}
	if len(result.Findings) != count {
		t.Fatalf("findings = %d, want %d", len(result.Findings), count)
	}
	seen := make(map[string]bool, count)
	for _, finding := range result.Findings {
		seen[finding.Title] = true
	}
	for i := 0; i < count; i++ {
		if !seen[fmt.Sprintf("finding-%d", i)] {
			t.Fatalf("finding-%d was lost", i)
		}
	}
}

func TestFindingRejectsCommitMissingFromRepository(t *testing.T) {
	repository, _ := createTestRepository(t)
	output := filepath.Join(t.TempDir(), "review.json")
	err := Run([]string{
		"--output", output,
		"--repository", repository,
		"finding",
		"--author", "Alice",
		"--commit", strings.Repeat("f", 40),
		"--file", "main.go",
		"--line", "1",
		"--severity", "high",
		"--title", "title",
		"--detail", "detail",
	})
	if err == nil || !strings.Contains(err.Error(), "does not identify an existing commit") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("invalid finding created output: %v", err)
	}
}

func TestFindingRequiresCommitMetadata(t *testing.T) {
	err := Run([]string{
		"--output", filepath.Join(t.TempDir(), "review.json"),
		"finding", "--severity", "high", "--title", "title", "--detail", "detail",
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSkipMarksResultWithoutFindings(t *testing.T) {
	output := filepath.Join(t.TempDir(), "review.json")
	if err := Run([]string{
		"--output", output,
		"skip",
		"--reason", "all commits contain noreview",
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result model.AgentResult
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Skipped || result.SkipReason == "" || len(result.Findings) != 0 {
		t.Fatalf("unexpected skip result: %#v", result)
	}
}

func createTestRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := t.TempDir()
	runTestGit(t, repository, "init")
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "main.go")
	runTestGit(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	return repository, strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
}

func runTestGit(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = repository
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
