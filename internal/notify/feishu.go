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

const (
	maxFindingsPerCard    = 8
	maxWebhookBodyBytes   = 20_000
	maxFindingFileRunes   = 240
	maxFindingTitleRunes  = 160
	maxFindingDetailRunes = 1200
	// Leave room for timestamp/sign, which are added only when sending.
	maxUnsignedCardBytes = 19 * 1024
)

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
	if len(body) > maxWebhookBodyBytes {
		return fmt.Errorf("Feishu card is %d bytes, exceeding the %d-byte webhook limit", len(body), maxWebhookBodyBytes)
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
		authorTotal := findingCount(group.commits)
		mention := mentions[strings.ToLower(group.author)]
		parts := packCommitGroups(report, group.author, mention, group.commits, authorTotal)
		for part, commits := range parts {
			cards = append(cards, buildAuthorCard(report, group.author, mention, commits, authorTotal, part+1, len(parts)))
		}
	}
	return cards
}

func packCommitGroups(report model.Report, author string, mention model.AuthorMention, commits []commitFindings, authorTotal int) [][]commitFindings {
	var parts [][]commitFindings
	var current []commitFindings
	currentFindings := 0
	flush := func() {
		if len(current) != 0 {
			parts = append(parts, current)
			current = nil
			currentFindings = 0
		}
	}
	fits := func(candidate []commitFindings) bool {
		// The final part numbers are shorter than these placeholders.
		card := buildAuthorCard(report, author, mention, candidate, authorTotal, 999999, 999999)
		body, err := json.Marshal(card)
		return err == nil && len(body) <= maxUnsignedCardBytes
	}
	for _, commit := range commits {
		candidate := append(append([]commitFindings(nil), current...), commit)
		if len(current) != 0 && (currentFindings+len(commit.findings) > maxFindingsPerCard || !fits(candidate)) {
			flush()
			candidate = []commitFindings{commit}
		}
		if len(commit.findings) <= maxFindingsPerCard && fits(candidate) {
			current = candidate
			currentFindings += len(commit.findings)
			continue
		}

		// Keep a commit together where possible; only split its findings when
		// the entire commit cannot fit in one webhook request.
		for _, finding := range commit.findings {
			candidate = appendFinding(current, commit, finding)
			if len(current) != 0 && (currentFindings+1 > maxFindingsPerCard || !fits(candidate)) {
				flush()
				candidate = appendFinding(nil, commit, finding)
			}
			if !fits(candidate) {
				candidate = []commitFindings{fitSingleFinding(commit, finding, fits)}
			}
			current = candidate
			currentFindings++
		}
	}
	flush()
	return parts
}

// fitSingleFinding trims only this card's display copy. The full finding remains
// in the persisted report, and rebuilding the cards yields the same boundaries.
func fitSingleFinding(commit commitFindings, finding model.Finding, fits func([]commitFindings) bool) commitFindings {
	fragment := commitFindings{commit: commit.commit, info: commit.info, findings: []model.Finding{finding}}
	reduced := finding
	fitsReduced := func() bool {
		fragment.findings[0] = reduced
		return fits([]commitFindings{fragment})
	}
	for _, field := range []struct {
		source string
		limit  int
		target *string
	}{
		{finding.Detail, maxFindingDetailRunes, &reduced.Detail},
		{finding.Title, maxFindingTitleRunes, &reduced.Title},
		{finding.File, maxFindingFileRunes, &reduced.File},
	} {
		runes := []rune(field.source)
		high := min(len(runes), field.limit)
		*field.target = prefixWithEllipsis(runes, high)
		if fitsReduced() {
			return fragment
		}
		low, best := 0, -1
		for low <= high {
			middle := low + (high-low)/2
			*field.target = prefixWithEllipsis(runes, middle)
			if fitsReduced() {
				best = middle
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if best >= 0 {
			*field.target = prefixWithEllipsis(runes, best)
			fragment.findings[0] = reduced
			return fragment
		}
		*field.target = ""
		if fitsReduced() {
			return fragment
		}
	}
	// The bounded header and commit fields normally make this unreachable. Keep
	// a compact notice rather than leaving an oversized card in the queue.
	fragment.info = model.CommitInfo{}
	fragment.findings[0] = model.Finding{
		Severity: finding.Severity,
		Commit:   finding.Commit,
		Title:    "审查结果过长",
		Detail:   "完整 finding 已保存在本地 review.json",
	}
	return fragment
}

func prefixWithEllipsis(runes []rune, count int) string {
	if count >= len(runes) {
		return string(runes)
	}
	return string(runes[:count]) + "…"
}

func appendFinding(current []commitFindings, commit commitFindings, finding model.Finding) []commitFindings {
	candidate := append([]commitFindings(nil), current...)
	if len(candidate) != 0 && candidate[len(candidate)-1].commit == commit.commit {
		last := candidate[len(candidate)-1]
		last.findings = append(append([]model.Finding(nil), last.findings...), finding)
		candidate[len(candidate)-1] = last
		return candidate
	}
	return append(candidate, commitFindings{commit: commit.commit, info: commit.info, findings: []model.Finding{finding}})
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
	authorLabel := escapeMarkdown(truncate(author, 80))
	if mention.FeishuID != "" && len(mention.FeishuID) <= 128 && mention.Name != "" {
		authorLabel = fmt.Sprintf("<at id=%s>%s</at>", mention.FeishuID, escapeMentionText(truncate(mention.Name, 80)))
		if !strings.EqualFold(strings.TrimSpace(mention.Name), strings.TrimSpace(author)) {
			authorLabel += "（Git: " + escapeMarkdown(truncate(author, 80)) + "）"
		}
	}
	elements := []any{markdownElement(fmt.Sprintf(
		"**仓库：** %s\n**作者：** %s\n**分支：** %s\n**变更：** `%s` → `%s`\n**Agent：** %s\n**问题：** 本卡 %d 条，该作者共 %d 条%s\n\n%s",
		escapeMarkdown(truncate(report.Repository, 80)), authorLabel, escapeMarkdown(truncate(report.Branch, 80)), short(report.FromSHA), short(report.ToSHA), escapeMarkdown(truncate(report.Agent, 80)),
		findingCount(commits), authorTotal, partLine, escapeMarkdown(truncate(report.Summary, 300)),
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
		content += "\n**标题：** " + escapeMarkdown(truncate(info.Subject, 200))
	}
	if info.Author != "" {
		content += "\n**Git 作者：** " + escapeMarkdown(truncate(info.Author, 80))
		if info.AuthorEmail != "" {
			content += " <`" + escapeBackticks(truncate(info.AuthorEmail, 120)) + "`>"
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
		escapeMarkdown(truncate(report.Repository, 80)), escapeMarkdown(truncate(branch, 80)), head, from, escapeMarkdown(truncate(report.Agent, 80)),
		report.FailureCount, report.RetryCount, escapeMarkdown(truncate(report.RetryAfter, 80)), escapeMarkdown(truncate(report.Error, 1000)),
	)
	return cardEnvelope("red", "Code Review失败通知", []any{markdownElement(content)})
}

func formatFinding(finding model.Finding) string {
	location := truncate(finding.File, maxFindingFileRunes)
	if finding.Line > 0 {
		location += fmt.Sprintf(":%d", finding.Line)
	}
	content := fmt.Sprintf("**[%s] %s**", strings.ToUpper(finding.Severity), escapeMarkdown(truncate(finding.Title, maxFindingTitleRunes)))
	if location != "" {
		content += "\n`" + escapeBackticks(location) + "`"
	}
	content += "\n" + escapeMarkdown(truncate(finding.Detail, maxFindingDetailRunes))
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
	value = escapeMentionText(value)
	value = strings.ReplaceAll(value, "\\", "\\\\")
	for _, marker := range []string{"*", "_", "~"} {
		value = strings.ReplaceAll(value, marker, "\\"+marker)
	}
	return value
}

func escapeBackticks(value string) string {
	return escapeMentionText(strings.ReplaceAll(value, "`", "'"))
}

func escapeMentionText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}
