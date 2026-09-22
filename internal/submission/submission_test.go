package submission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"cancanneed/internal/model"
)

func TestConcurrentFindingSubmissionsProduceOneValidResult(t *testing.T) {
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
				"finding",
				"--author", fmt.Sprintf("author-%d", i),
				"--commit", "1111111111111111111111111111111111111111",
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
