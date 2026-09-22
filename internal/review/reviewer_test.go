package review

import (
	"strings"
	"testing"

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
