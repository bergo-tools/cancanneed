package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cancanneed/internal/agent"
	"cancanneed/internal/config"
	gitrepo "cancanneed/internal/git"
	"cancanneed/internal/model"
)

type Request struct {
	Repository  config.Repository
	Branch      string
	FromSHA     string
	ObservedSHA string
	LatestOnly  bool
}

type Reviewer struct {
	RunsDir              string
	MaxRunsPerRepository int
	Runner               agent.Runner
	SubmitCommand        []string
	Now                  func() time.Time
}

func (r Reviewer) Review(ctx context.Context, request Request) (model.Report, string, error) {
	if r.Now == nil {
		r.Now = time.Now
	}
	runID := fmt.Sprintf("%s-%s-%d", safeName(request.Repository.Name), shortSHA(request.ObservedSHA), r.Now().UnixNano())
	runDir := filepath.Join(r.RunsDir, runID)
	if err := os.MkdirAll(r.RunsDir, 0o700); err != nil {
		return model.Report{}, "", fmt.Errorf("create runs directory: %w", err)
	}
	if err := pruneRepositoryRuns(r.RunsDir, request.Repository.Name, r.maxRuns()-1); err != nil {
		return model.Report{}, "", err
	}
	if err := os.Mkdir(runDir, 0o700); err != nil {
		return model.Report{}, "", fmt.Errorf("create run directory: %w", err)
	}
	if err := writeRequestMetadata(filepath.Join(runDir, "request.json"), request); err != nil {
		_ = os.RemoveAll(runDir)
		return model.Report{}, "", err
	}

	submitPath := filepath.Join(runDir, "submit-review.sh")
	outputPath := filepath.Join(runDir, "review.json")
	submitCommand := r.SubmitCommand
	if len(submitCommand) == 0 {
		executable, err := os.Executable()
		if err != nil {
			return model.Report{}, runDir, fmt.Errorf("resolve cancanneed executable: %w", err)
		}
		submitCommand = []string{executable, "__submit"}
	}
	if err := os.WriteFile(submitPath, []byte(submitScript(outputPath, request.Repository.Path, submitCommand)), 0o700); err != nil {
		return model.Report{}, runDir, fmt.Errorf("write submit script: %w", err)
	}
	prompt := buildPrompt(request, submitPath)

	result, err := r.Runner.Execute(ctx, agent.Request{
		Agent:      request.Repository.Agent,
		WorkingDir: request.Repository.Path,
		RunDir:     runDir,
		Prompt:     prompt,
		OutputPath: outputPath,
		Environment: map[string]string{
			"CANCANNEED_FROM_SHA":      request.FromSHA,
			"CANCANNEED_REMOTE":        request.Repository.Remote,
			"CANCANNEED_BRANCH":        request.Branch,
			"CANCANNEED_SUBMIT_SCRIPT": submitPath,
		},
		Template: map[string]string{
			"repo":          request.Repository.Path,
			"branch":        request.Branch,
			"from_sha":      request.FromSHA,
			"submit_script": submitPath,
			"output":        outputPath,
		},
	})
	if err != nil {
		return model.Report{}, runDir, err
	}
	git := gitrepo.Repository{Path: request.Repository.Path, Remote: request.Repository.Remote}
	reviewedHead, err := git.ReviewHead(ctx)
	if err != nil {
		return model.Report{}, runDir, fmt.Errorf("read completed review head: %w", err)
	}
	reviewFrom := request.FromSHA
	if request.LatestOnly {
		reviewFrom, err = git.ReviewBase(ctx, reviewedHead)
		if err != nil {
			return model.Report{}, runDir, fmt.Errorf("resolve completed review base: %w", err)
		}
	}
	commits, err := collectCommitInfo(ctx, git, result.Findings)
	if err != nil {
		return model.Report{}, runDir, err
	}
	verdict := "approve"
	summary := "未发现需要上报的重要问题"
	if len(result.Findings) > 0 {
		verdict = "request_changes"
		summary = fmt.Sprintf("发现 %d 个需要处理的重要问题", len(result.Findings))
	}
	if result.Skipped {
		verdict = "skip"
		summary = result.SkipReason
	}
	report := model.Report{
		Repository:  request.Repository.Name,
		Branch:      request.Branch,
		FromSHA:     reviewFrom,
		ToSHA:       reviewedHead,
		Agent:       request.Repository.Agent.Type,
		Verdict:     verdict,
		Summary:     summary,
		Findings:    result.Findings,
		Commits:     commits,
		GeneratedAt: r.Now().UTC(),
	}
	return report, runDir, nil
}

func collectCommitInfo(ctx context.Context, repository gitrepo.Repository, findings []model.Finding) ([]model.CommitInfo, error) {
	commits := make([]model.CommitInfo, 0)
	seen := make(map[string]struct{})
	for _, finding := range findings {
		commit := strings.ToLower(finding.Commit)
		if _, exists := seen[commit]; exists {
			continue
		}
		info, err := repository.CommitInfo(ctx, commit)
		if err != nil {
			return nil, err
		}
		seen[commit] = struct{}{}
		commits = append(commits, info)
	}
	return commits, nil
}

// Cleanup removes old completed run directories for one repository.
func (r Reviewer) Cleanup(repository string) error {
	if err := os.MkdirAll(r.RunsDir, 0o700); err != nil {
		return fmt.Errorf("create runs directory: %w", err)
	}
	return pruneRepositoryRuns(r.RunsDir, repository, r.maxRuns())
}

func (r Reviewer) maxRuns() int {
	if r.MaxRunsPerRepository < 1 {
		return 10
	}
	return r.MaxRunsPerRepository
}

type runDirectory struct {
	path       string
	name       string
	modifiedAt time.Time
}

func pruneRepositoryRuns(runsDir, repository string, keep int) error {
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list review run directories: %w", err)
	}
	var candidates []runDirectory
	prefix := safeName(repository) + "-"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(runsDir, entry.Name())
		belongs, err := runBelongsToRepository(filepath.Join(path, "request.json"), repository)
		if err != nil {
			return err
		}
		if !belongs {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("read review run directory %s: %w", path, err)
		}
		candidates = append(candidates, runDirectory{path: path, name: entry.Name(), modifiedAt: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modifiedAt.Equal(candidates[j].modifiedAt) {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].modifiedAt.Before(candidates[j].modifiedAt)
	})
	removeCount := len(candidates) - keep
	for i := 0; i < removeCount; i++ {
		if err := os.RemoveAll(candidates[i].path); err != nil {
			return fmt.Errorf("remove old review run directory %s: %w", candidates[i].path, err)
		}
	}
	return nil
}

func runBelongsToRepository(path, repository string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect review request metadata %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read review request metadata %s: %w", path, err)
	}
	var metadata struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(b, &metadata); err != nil || metadata.Repository == "" {
		return false, nil
	}
	return metadata.Repository == repository, nil
}

func submitScript(outputPath, repositoryPath string, command []string) string {
	quotedCommand := make([]string, len(command))
	for i, argument := range command {
		quotedCommand[i] = shellQuote(argument)
	}
	return fmt.Sprintf(`#!/bin/sh
set -eu
exec %s --output %s --repository %s "$@"
`, strings.Join(quotedCommand, " "), shellQuote(outputPath), shellQuote(repositoryPath))
}

func buildPrompt(request Request, submitPath string) string {
	introduction := fmt.Sprintf(
		"上一次已经审查的提交是 %s。请在当前仓库中自行拉取远端 %q 的 %q 分支，确定该分支最新的 HEAD，并审查从上次已审查提交到最新 HEAD 之间的全部变更。",
		request.FromSHA, request.Repository.Remote, request.Branch,
	)
	coverage := "必须逐个检查范围内的所有新增 commit，不要只看最新几个，也不要只看改动最大的 commit。除下面明确允许跳过的 commit 外，一个都不要遗漏。"
	if request.LatestOnly {
		introduction = fmt.Sprintf(
			"当前没有这个分支的历史审查 HEAD。请在当前仓库中自行 fetch 远端 %q 的 %q 分支，只审查 FETCH_HEAD 对应的一个 commit。普通提交以 FETCH_HEAD 的第一父提交为对比起点，根提交以空树为起点；不要使用更早的历史审查起点，也不代表更早的提交已经审查过。",
			request.Repository.Remote, request.Branch,
		)
		coverage = "只检查最新 HEAD 对应的这个 commit，不要回溯审查更早的 commit。仍需完整检查该 commit 的所有人工改动，除下面明确允许跳过的情况外不要遗漏。"
	}
	return fmt.Sprintf(`# 自动化代码审查

%s

审查范围：
1. %s
2. 如果仓库根目录存在名为 review.md 的文件（忽略文件名大小写），开始审查前必须先读取并遵守其中针对本仓库的审查要求。

只允许跳过以下 commit：
1. commit 的标题或正文包含 noreview（忽略大小写）。
2. 整个 commit 都是 vendor/第三方依赖更新或明显的自动化批量修改，例如几百上千个文件的生成代码同步、第三方代码同步、脚本批量替换等。只有部分文件属于这些类型时，仍然要审查其余人工修改。
3. 对跳过的 commit 不要检查其具体改动，也不要提交 finding，更不要在摘要中逐项通知跳过情况。
4. 如果范围内所有 commit 都可以跳过，调用一次 %s skip --reason "<全部跳过的原因>" 并正常退出。cancanneed 会推进审查 HEAD，但不发送通知。

问题上报标准：
1. 只上报真正重要且可操作的问题：逻辑 bug、崩溃或异常、数据损坏、并发竞态、安全问题、资源泄漏和明显的接口误用。
2. 忽略代码风格、命名、注释、格式以及纯重构偏好等小问题。
3. 必须结合改动上下文，包括调用方、被调用方和相关函数，确认问题确实存在并尽量避免误报。证据不足时不要上报。
4. 每条 finding 的标题和详情尽量使用简洁、精准的中文：标题直指具体问题；详情只说明触发条件、实际后果和必要的修复方向，避免空泛措辞、重复背景和冗长推测。

执行规则：
1. 不要编辑仓库文件、创建提交、切换分支或推送任何内容。
2. 自行使用 git fetch 拉取并检查变更，以 FETCH_HEAD 作为本次审查的最新终点。按需阅读相关上下文代码，不要运行测试，也不要写入，只进行review。
3. 每发现一个符合上述上报标准的问题，调用一次下面的工具；多个问题可以并发提交：
   %s finding --author "<git show -s --format=%%an 得到的提交作者名称>" --commit "<完整提交 SHA>" --file "<仓库相对文件路径>" --line <新文件中的行号> --severity "<critical|high|medium|low|info>" --title "<问题标题>" --detail "<问题原因和修复建议>"
4. 每次调用提交工具都必须检查退出码。如果工具报告参数错误或 commit 不存在，重新 fetch、确认完整 SHA、修正参数后再次调用；只有退出码为 0 才表示该 finding 提交成功。
5. 等待所有 finding 命令成功执行完成后直接正常退出。没有 finding 时也直接正常退出。

提交工具会创建并安全更新结构化 JSON 结果。不要自行创建或编辑 review.json
`, introduction, coverage, shellCommand(submitPath), shellCommand(submitPath))
}

func writeRequestMetadata(path string, request Request) error {
	metadata := struct {
		Repository  string `json:"repository"`
		Path        string `json:"path"`
		Remote      string `json:"remote"`
		Branch      string `json:"branch"`
		FromSHA     string `json:"from_sha"`
		ObservedSHA string `json:"observed_sha"`
		Agent       string `json:"agent"`
		LatestOnly  bool   `json:"latest_only"`
	}{request.Repository.Name, request.Repository.Path, request.Repository.Remote, request.Branch, request.FromSHA, request.ObservedSHA, request.Repository.Agent.Type, request.LatestOnly}
	b, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode request metadata: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write request metadata: %w", err)
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(value string) string {
	value = strings.Trim(unsafeName.ReplaceAllString(value, "-"), "-.")
	if value == "" {
		return "review"
	}
	return value
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellCommand(path string) string { return shellQuote(path) }
