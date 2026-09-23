package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cancanneed/internal/model"
)

type Feishu struct {
	Webhook        string
	Secret         string
	AuthorMentions map[string]model.AuthorMention
	Client         *http.Client
	Now            func() time.Time
}

const maxFindingsPerCard = 8

type commitFindings struct {
	commit   string
	info     model.CommitInfo
	findings []model.Finding
}

type authorCommits struct {
	author  string
	commits []commitFindings
}

func (f Feishu) NotificationCount(report model.Report) int {
	return len(buildCards(report, f.AuthorMentions))
}

func (f Feishu) Notify(ctx context.Context, report model.Report, cardIndex int) error {
	cards := buildCards(report, f.AuthorMentions)
	if cardIndex < 0 || cardIndex >= len(cards) {
		return fmt.Errorf("Feishu card index %d out of range [0, %d)", cardIndex, len(cards))
	}
	return f.sendCard(ctx, cards[cardIndex])
}

func (f Feishu) NotifyReviewFailure(ctx context.Context, report model.ReviewFailureReport) error {
	return f.sendCard(ctx, buildFailureCard(report))
}

func (f Feishu) sendCard(ctx context.Context, payload map[string]any) error {
	if f.Client == nil {
		f.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if f.Now == nil {
		f.Now = time.Now
	}
	if f.Secret != "" {
		timestamp := strconv.FormatInt(f.Now().Unix(), 10)
		payload["timestamp"] = timestamp
		payload["sign"] = signature(timestamp, f.Secret)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Feishu card: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Webhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Feishu request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := f.Client.Do(req)
	if err != nil {
		return fmt.Errorf("send Feishu card: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read Feishu response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Feishu returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return fmt.Errorf("decode Feishu response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("Feishu returned code %d: %s", result.Code, result.Msg)
	}
	return nil
}

func signature(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func buildCards(report model.Report, mentions map[string]model.AuthorMention) []map[string]any {
	if len(report.Findings) == 0 {
		return nil
	}

	commitInfo := make(map[string]model.CommitInfo, len(report.Commits))
	for _, info := range report.Commits {
		commitInfo[strings.ToLower(info.Commit)] = info
	}
	commitOrder := make([]string, 0)
	byCommit := make(map[string][]model.Finding)
	for _, finding := range report.Findings {
		commit := strings.ToLower(strings.TrimSpace(finding.Commit))
		if _, exists := byCommit[commit]; !exists {
			commitOrder = append(commitOrder, commit)
		}
		byCommit[commit] = append(byCommit[commit], finding)
	}

	groups := make([]authorCommits, 0)
	groupIndex := make(map[string]int)
	for _, commit := range commitOrder {
		info := commitInfo[commit]
		author := strings.TrimSpace(info.Author)
		if author == "" {
			author = strings.TrimSpace(byCommit[commit][0].Author)
			if author == "" {
				author = "未知作者"
			}
		}
		key := strings.ToLower(author)
		index, exists := groupIndex[key]
		if !exists {
			index = len(groups)
			groupIndex[key] = index
			groups = append(groups, authorCommits{author: author})
		}
		groups[index].commits = append(groups[index].commits, commitFindings{commit: commit, info: info, findings: byCommit[commit]})
	}

	var cards []map[string]any
	for _, group := range groups {
		parts := packCommitGroups(group.commits)
		authorTotal := findingCount(group.commits)
		for part, commits := range parts {
			cards = append(cards, buildAuthorCard(report, group.author, mentions[strings.ToLower(group.author)], commits, authorTotal, part+1, len(parts)))
		}
	}
	return cards
}

func packCommitGroups(commits []commitFindings) [][]commitFindings {
	var parts [][]commitFindings
	var current []commitFindings
	currentFindings := 0
	for _, commit := range commits {
		count := len(commit.findings)
		if len(current) != 0 && currentFindings+count > maxFindingsPerCard {
			parts = append(parts, current)
			current = nil
			currentFindings = 0
		}
		current = append(current, commit)
		currentFindings += count
	}
	if len(current) != 0 {
		parts = append(parts, current)
	}
	return parts
}

func findingCount(commits []commitFindings) int {
	total := 0
	for _, commit := range commits {
		total += len(commit.findings)
	}
	return total
}

func buildAuthorCard(report model.Report, author string, mention model.AuthorMention, commits []commitFindings, authorTotal, part, parts int) map[string]any {
	template := "green"
	switch report.Verdict {
	case "request_changes":
		template = "red"
	case "comment":
		template = "orange"
	}
	title := "Code Review结果通知"
	partLine := ""
	if parts > 1 {
		partLine = fmt.Sprintf("\n**分片：** %d/%d", part, parts)
	}
	authorLabel := escapeMarkdown(author)
	if mention.FeishuID != "" && mention.Name != "" {
		authorLabel = fmt.Sprintf("<at id=%s>%s</at>", mention.FeishuID, escapeMentionText(mention.Name))
		if !strings.EqualFold(strings.TrimSpace(mention.Name), strings.TrimSpace(author)) {
			authorLabel += "（Git: " + escapeMarkdown(author) + "）"
		}
	}
	elements := []any{markdownElement(fmt.Sprintf(
		"**仓库：** %s\n**作者：** %s\n**分支：** %s\n**变更：** `%s` → `%s`\n**Agent：** %s\n**问题：** 本卡 %d 条，该作者共 %d 条%s\n\n%s",
		escapeMarkdown(report.Repository), authorLabel, escapeMarkdown(report.Branch), short(report.FromSHA), short(report.ToSHA), escapeMarkdown(report.Agent),
		findingCount(commits), authorTotal, partLine, escapeMarkdown(truncate(report.Summary, 1200)),
	))}

	for _, commit := range commits {
		elements = append(elements, markdownElement(formatCommitInfo(commit.commit, commit.info)))
		for _, finding := range commit.findings {
			elements = append(elements, markdownElement(formatFinding(finding)))
		}
	}
	return cardEnvelope(template, title, elements)
}

func formatCommitInfo(commit string, info model.CommitInfo) string {
	label := "未知 commit"
	if commit != "" {
		label = "`" + escapeBackticks(short(commit)) + "`"
	}
	content := "**Commit " + label + "**"
	if info.Subject != "" {
		content += "\n**标题：** " + escapeMarkdown(truncate(info.Subject, 300))
	}
	if info.Author != "" {
		content += "\n**Git 作者：** " + escapeMarkdown(info.Author)
		if info.AuthorEmail != "" {
			content += " <`" + escapeBackticks(info.AuthorEmail) + "`>"
		}
	}
	if !info.CommittedAt.IsZero() {
		content += "\n**提交时间：** " + info.CommittedAt.Format("2006-01-02 15:04:05 -07:00")
	}
	return content
}

func buildFailureCard(report model.ReviewFailureReport) map[string]any {
	head := short(report.HeadSHA)
	if head == "" {
		head = "未获取"
	}
	branch := report.Branch
	if branch == "" {
		branch = "未确定"
	}
	from := short(report.FromSHA)
	if from == "" {
		from = "无记录"
	}
	content := fmt.Sprintf(
		"**仓库：** %s\n**分支：** %s\n**待审查 HEAD：** `%s`\n**审查起点：** `%s`\n**Agent：** %s\n**连续失败：** %d 次\n**已重试：** %d 次\n**下次重试：** %s 后\n\n**最近错误：**\n%s",
		escapeMarkdown(report.Repository), escapeMarkdown(branch), head, from, escapeMarkdown(report.Agent),
		report.FailureCount, report.RetryCount, escapeMarkdown(report.RetryAfter), escapeMarkdown(truncate(report.Error, 1500)),
	)
	return cardEnvelope("red", "Code Review失败通知", []any{markdownElement(content)})
}

func formatFinding(finding model.Finding) string {
	location := finding.File
	if finding.Line > 0 {
		location += fmt.Sprintf(":%d", finding.Line)
	}
	content := fmt.Sprintf("**[%s] %s**", strings.ToUpper(finding.Severity), escapeMarkdown(finding.Title))
	if location != "" {
		content += "\n`" + escapeBackticks(location) + "`"
	}
	content += "\n" + escapeMarkdown(truncate(finding.Detail, 1200))
	return content
}

func cardEnvelope(template, title string, elements []any) map[string]any {
	return map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"template": template,
				"title":    map[string]any{"tag": "plain_text", "content": title},
			},
			"elements": elements,
		},
	}
}

func markdownElement(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func escapeMarkdown(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	for _, marker := range []string{"*", "_", "~"} {
		value = strings.ReplaceAll(value, marker, "\\"+marker)
	}
	return value
}

func escapeBackticks(value string) string { return strings.ReplaceAll(value, "`", "'") }

func escapeMentionText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}
