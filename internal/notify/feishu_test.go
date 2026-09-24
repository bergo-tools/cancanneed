package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cancanneed/internal/model"
)

func TestFeishuSendsInteractiveCard(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer server.Close()

	now := time.Unix(1700000000, 0)
	err := (Feishu{Webhook: server.URL, Secret: "secret", Client: server.Client(), Now: func() time.Time { return now }}).Notify(context.Background(), model.Report{
		Repository: "api", Branch: "main", FromSHA: "11111111111111111111", ToSHA: "22222222222222222222",
		Agent: "pi", Verdict: "request_changes", Summary: "one problem",
		Findings: []model.Finding{{
			Severity: "high", Author: "Alice", Commit: "1111111111111111111111111111111111111111",
			File: "main.go", Line: 10, Title: "bug", Detail: "details",
		}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if received["msg_type"] != "interactive" || received["timestamp"] != "1700000000" || received["sign"] == "" {
		t.Fatalf("unexpected payload: %#v", received)
	}
	if title := cardTitle(t, received); title != "Code Review结果通知" {
		t.Fatalf("title = %q", title)
	}
}

func TestFeishuOmitsCardWithoutFindings(t *testing.T) {
	report := model.Report{Verdict: "approve"}
	feishu := Feishu{}
	if count := feishu.NotificationCount(report); count != 0 {
		t.Fatalf("card count = %d, want 0", count)
	}
	if err := feishu.Notify(context.Background(), report, 0); err == nil {
		t.Fatal("empty review should not have a sendable card")
	}
}

func TestFeishuRejectsApplicationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001,"msg":"bad webhook"}`))
	}))
	defer server.Close()
	if err := (Feishu{Webhook: server.URL, Client: server.Client()}).Notify(context.Background(), model.Report{
		Findings: []model.Finding{{Author: "Alice", Commit: "1111111111111111111111111111111111111111"}},
	}, 0); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFeishuRejectsOversizedCardBeforeHTTP(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	feishu := Feishu{Webhook: server.URL, Client: server.Client()}
	card := cardEnvelope("red", "oversized", []any{markdownElement(strings.Repeat("x", maxWebhookBodyBytes))})
	if err := feishu.sendCard(context.Background(), card); err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("oversized card error = %v", err)
	}
	if called.Load() {
		t.Fatal("oversized card was sent")
	}
}

func TestFeishuGroupsCardsByAuthorAndCommitAndSplitsLargeGroups(t *testing.T) {
	report := model.Report{
		Repository: "api", Branch: "main", Verdict: "request_changes",
		Commits: []model.CommitInfo{
			{Commit: "aaaaaaaaaaaaaaaa", Author: "Alice", AuthorEmail: "alice@example.com", Subject: "fix transaction rollback", CommittedAt: time.Date(2026, 9, 22, 10, 30, 0, 0, time.FixedZone("CST", 8*60*60))},
			{Commit: "bbbbbbbbbbbbbbbb", Author: "Alice", AuthorEmail: "alice@example.com", Subject: "guard concurrent writes", CommittedAt: time.Date(2026, 9, 22, 11, 0, 0, 0, time.FixedZone("CST", 8*60*60))},
			{Commit: "cccccccccccccccc", Author: "Bob", AuthorEmail: "bob@example.com", Subject: "handle request errors", CommittedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))},
		},
	}
	for i := 0; i < maxFindingsPerCard+1; i++ {
		commit := "aaaaaaaaaaaaaaaa"
		if i >= 4 {
			commit = "bbbbbbbbbbbbbbbb"
		}
		report.Findings = append(report.Findings, model.Finding{
			Author: "Alice", Commit: commit, Severity: "high", Title: fmt.Sprintf("alice-%d", i), Detail: "detail",
		})
	}
	report.Findings = append(report.Findings, model.Finding{
		Author: "Bob", Commit: "cccccccccccccccc", Severity: "medium", Title: "bob-0", Detail: "detail",
	})

	cards := buildCards(report, nil)
	if len(cards) != 3 {
		t.Fatalf("card count = %d, want 3", len(cards))
	}
	first := cardText(t, cards[0])
	second := cardText(t, cards[1])
	third := cardText(t, cards[2])
	if !strings.Contains(first, "Alice") || !strings.Contains(first, "aaaaaaaaaaaa") || strings.Contains(first, "bbbbbbbbbbbb") || strings.Contains(first, "Bob") {
		t.Fatalf("first Alice card split a commit group incorrectly: %s", first)
	}
	for _, want := range []string{"fix transaction rollback", "alice@example.com", "2026-09-22 10:30:00 +08:00", "alice-0", "alice-3"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first commit card does not contain %q: %s", want, first)
		}
	}
	if !strings.Contains(second, "Alice") || !strings.Contains(second, "bbbbbbbbbbbb") || !strings.Contains(second, "alice-4") || !strings.Contains(second, "alice-8") || !strings.Contains(second, "分片") || !strings.Contains(second, "2/2") || strings.Contains(second, "Bob") {
		t.Fatalf("second Alice card is not the split continuation: %s", second)
	}
	if !strings.Contains(third, "Bob") || strings.Contains(third, "Alice") {
		t.Fatalf("Bob card contains another author: %s", third)
	}
}

func TestFeishuSplitsLargeCommitByWebhookBytes(t *testing.T) {
	report := model.Report{
		Repository: "api", Branch: "main", Agent: "pi", Verdict: "request_changes",
		Commits: []model.CommitInfo{{Commit: strings.Repeat("a", 40), Author: "Alice", Subject: "large review"}},
	}
	for i := 0; i < maxFindingsPerCard; i++ {
		report.Findings = append(report.Findings, model.Finding{
			Author: "Alice", Commit: strings.Repeat("a", 40), Severity: "high",
			File: "main.go", Line: i + 1, Title: fmt.Sprintf("issue-%d", i),
			Detail: strings.Repeat("问题", 600),
		})
	}
	cards := buildCards(report, nil)
	if len(cards) < 2 {
		t.Fatalf("large commit produced %d card(s), want multiple", len(cards))
	}
	var allContents strings.Builder
	for _, card := range cards {
		allContents.WriteString(cardMarkdownContents(t, card))
	}
	for i := 0; i < maxFindingsPerCard; i++ {
		if title := fmt.Sprintf("issue-%d", i); strings.Count(allContents.String(), title) != 1 {
			t.Fatalf("finding %q was lost or duplicated", title)
		}
	}
	var delivered atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode card: %v", err)
		} else if len(payload) > maxWebhookBodyBytes {
			t.Errorf("sent card is %d bytes", len(payload))
		}
		delivered.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer server.Close()
	feishu := Feishu{Webhook: server.URL, Secret: "secret", Client: server.Client()}
	for i, card := range cards {
		body, err := json.Marshal(card)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > maxUnsignedCardBytes {
			t.Fatalf("card %d is %d bytes, limit %d", i, len(body), maxUnsignedCardBytes)
		}
		if err := feishu.Notify(context.Background(), report, i); err != nil {
			t.Fatalf("send card %d: %v", i, err)
		}
	}
	if got := delivered.Load(); got != int32(len(cards)) {
		t.Fatalf("delivered %d cards, want %d", got, len(cards))
	}
}

func TestFeishuSingleLongFindingFitsWebhook(t *testing.T) {
	long := strings.Repeat("\u2028", 3000)
	commit := strings.Repeat("a", 40)
	report := model.Report{
		Repository: long, Branch: long, Agent: long, Summary: long, Verdict: "request_changes",
		Commits: []model.CommitInfo{{Commit: commit, Author: long, AuthorEmail: long, Subject: long}},
		Findings: []model.Finding{{Author: long, Commit: commit, File: long, Line: 1,
			Severity: "high", Title: long, Detail: long}},
	}
	cards := buildCards(report, map[string]model.AuthorMention{
		strings.ToLower(long): {FeishuID: strings.Repeat("a", 128), Name: long},
	})
	if len(cards) != 1 {
		t.Fatalf("single finding produced %d cards", len(cards))
	}
	body, err := json.Marshal(cards[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > maxUnsignedCardBytes {
		t.Fatalf("single-finding card is %d bytes, limit %d", len(body), maxUnsignedCardBytes)
	}
}

func TestFeishuEscapesUntrustedMentions(t *testing.T) {
	commit := strings.Repeat("a", 40)
	malicious := "<at id=all></at>"
	report := model.Report{
		Repository: malicious, Branch: malicious, Agent: "pi", Verdict: "request_changes",
		Commits: []model.CommitInfo{{Commit: commit, Author: "Alice", Subject: malicious, AuthorEmail: malicious}},
		Findings: []model.Finding{{Author: "Alice", Commit: commit, Severity: "high",
			File: malicious, Line: 1, Title: malicious, Detail: malicious}},
	}
	cards := buildCards(report, map[string]model.AuthorMention{"alice": {FeishuID: "ou_alice", Name: "张三"}})
	if len(cards) != 1 {
		t.Fatalf("card count = %d", len(cards))
	}
	contents := cardMarkdownContents(t, cards[0])
	if strings.Contains(contents, malicious) {
		t.Fatalf("untrusted mention was not escaped: %s", contents)
	}
	if !strings.Contains(contents, "&lt;at id=all&gt;&lt;/at&gt;") {
		t.Fatalf("escaped mention text missing: %s", contents)
	}
	if !strings.Contains(contents, "<at id=ou_alice>张三</at>") {
		t.Fatalf("trusted author mention was removed: %s", contents)
	}
}

func TestFeishuMentionsMappedAuthor(t *testing.T) {
	report := model.Report{
		Repository: "api", Branch: "main", Verdict: "request_changes",
		Findings: []model.Finding{{Author: "Alice", Commit: "aaaaaaaaaaaaaaaa", Severity: "high", Title: "bug", Detail: "detail"}},
	}
	cards := buildCards(report, map[string]model.AuthorMention{
		"alice": {FeishuID: "ou_alice", Name: "张三"},
	})
	content := cardText(t, cards[0])
	for _, want := range []string{"ou_alice", "张三", "Git: Alice"} {
		if !strings.Contains(content, want) {
			t.Fatalf("mentioned author card does not contain %q: %s", want, content)
		}
	}
}

func TestFeishuBuildsFailureNotification(t *testing.T) {
	card := buildFailureCard(model.ReviewFailureReport{
		Repository: "api", Branch: "main", FromSHA: "1111111111111111", HeadSHA: "2222222222222222",
		Agent: "pi", Error: "agent exited with status 1", FailureCount: 30, RetryCount: 29, RetryAfter: "2m0s",
	})
	if title := cardTitle(t, card); title != "Code Review失败通知" {
		t.Fatalf("title = %q", title)
	}
	content := cardText(t, card)
	for _, want := range []string{"api", "222222222222", "30", "29", "2m0s", "agent exited with status 1"} {
		if !strings.Contains(content, want) {
			t.Fatalf("failure card does not contain %q: %s", want, content)
		}
	}
	unknownHead := cardText(t, buildFailureCard(model.ReviewFailureReport{Repository: "api", Error: "read origin/main: permission denied"}))
	for _, want := range []string{"未获取", "未确定", "无记录", "permission denied"} {
		if !strings.Contains(unknownHead, want) {
			t.Fatalf("unknown-head failure card does not contain %q: %s", want, unknownHead)
		}
	}
	long := strings.Repeat("\u2028", 3000)
	largeFailure := buildFailureCard(model.ReviewFailureReport{
		Repository: long, Branch: long, Agent: long, Error: long, RetryAfter: long,
	})
	body, err := json.Marshal(largeFailure)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > maxUnsignedCardBytes {
		t.Fatalf("failure card is %d bytes, limit %d", len(body), maxUnsignedCardBytes)
	}
}

func cardText(t *testing.T, card map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func cardTitle(t *testing.T, payload map[string]any) string {
	t.Helper()
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("missing card: %#v", payload)
	}
	header, ok := card["header"].(map[string]any)
	if !ok {
		t.Fatalf("missing header: %#v", card)
	}
	title, ok := header["title"].(map[string]any)
	if !ok {
		t.Fatalf("missing title: %#v", header)
	}
	content, ok := title["content"].(string)
	if !ok {
		t.Fatalf("missing title content: %#v", title)
	}
	return content
}

func cardMarkdownContents(t *testing.T, payload map[string]any) string {
	t.Helper()
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("missing card: %#v", payload)
	}
	elements, ok := card["elements"].([]any)
	if !ok {
		t.Fatalf("missing elements: %#v", card)
	}
	var contents []string
	for _, element := range elements {
		entry := element.(map[string]any)
		text := entry["text"].(map[string]any)
		contents = append(contents, text["content"].(string))
	}
	return strings.Join(contents, "\n")
}
