package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cancanneed/internal/config"
)

func TestBuildPromptUsesChineseInstructions(t *testing.T) {
	prompt := buildPrompt(Request{
		Repository: config.Repository{Name: "api", Remote: "origin"},
		Branch:     "main",
		FromSHA:    "1111111111111111111111111111111111111111",
	}, "/tmp/submit-review.sh")
	for _, required := range []string{
		"自动化代码审查",
		"上一次已经审查的提交",
		"review.md",
		"noreview",
		"逐个检查范围内的所有新增 commit",
		"只上报真正重要且可操作的问题",
		"skip --reason",
		"等待所有 finding 命令执行完成后直接正常退出",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt does not contain %q:\n%s", required, prompt)
		}
	}
	if strings.Contains(prompt, "Automated commit review") {
		t.Fatalf("prompt still contains the old English heading:\n%s", prompt)
	}
	if strings.Contains(strings.ToLower(prompt), "complete") {
		t.Fatalf("prompt must not mention the removed complete command:\n%s", prompt)
	}
}

func TestCleanupKeepsNewestRunsForEachRepository(t *testing.T) {
	runsDir := t.TempDir()
	createRuns := func(repository string, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			runDir := filepath.Join(runsDir, fmt.Sprintf("%s-%02d", repository, i))
			if err := os.Mkdir(runDir, 0o700); err != nil {
				t.Fatal(err)
			}
			request := Request{Repository: config.Repository{Name: repository}}
			if err := writeRequestMetadata(filepath.Join(runDir, "request.json"), request); err != nil {
				t.Fatal(err)
			}
			modified := time.Unix(int64(i+1), 0)
			if err := os.Chtimes(runDir, modified, modified); err != nil {
				t.Fatal(err)
			}
		}
	}
	createRuns("api", 12)
	createRuns("web", 3)
	foreign := filepath.Join(runsDir, "unrelated")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	reviewer := Reviewer{RunsDir: runsDir, MaxRunsPerRepository: 10}
	if err := reviewer.Cleanup("api"); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"api-00", "api-01"} {
		if _, err := os.Stat(filepath.Join(runsDir, removed)); !os.IsNotExist(err) {
			t.Fatalf("old run %s was not removed", removed)
		}
	}
	for _, retained := range []string{"api-02", "api-11", "web-00", "web-02", "unrelated"} {
		if _, err := os.Stat(filepath.Join(runsDir, retained)); err != nil {
			t.Fatalf("run %s should be retained: %v", retained, err)
		}
	}

	if err := pruneRepositoryRuns(runsDir, "api", 9); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runsDir, "api-02")); !os.IsNotExist(err) {
		t.Fatal("cleanup before a new review did not make room for the next run")
	}
}

func TestBuildPromptReviewsOnlyLatestCommitWithoutHistory(t *testing.T) {
	prompt := buildPrompt(Request{
		Repository: config.Repository{Name: "api", Remote: "origin"},
		Branch:     "main",
		FromSHA:    "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		LatestOnly: true,
	}, "/tmp/submit-review.sh")
	for _, required := range []string{
		"当前没有这个分支的历史审查 HEAD",
		"只审查 FETCH_HEAD 对应的一个 commit",
		"根提交以空树为起点",
		"不要回溯审查更早的 commit",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt does not contain %q:\n%s", required, prompt)
		}
	}
	if strings.Contains(prompt, "上一次已经审查的提交") {
		t.Fatalf("latest-only prompt claims a historical review exists:\n%s", prompt)
	}
}
