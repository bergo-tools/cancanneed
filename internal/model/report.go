package model

import "time"

// Finding is one actionable issue found by an agent.
type Finding struct {
	Severity string `json:"severity"`
	Author   string `json:"author"`
	Commit   string `json:"commit"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// AuthorMention maps a Git author to a Feishu user mention.
type AuthorMention struct {
	FeishuID string `json:"feishu_id"`
	Name     string `json:"name"`
}

// AgentResult is the JSON contract an agent must submit.
type AgentResult struct {
	Skipped    bool      `json:"skipped,omitempty"`
	SkipReason string    `json:"skip_reason,omitempty"`
	Findings   []Finding `json:"findings"`
}

// Report adds trusted run metadata to an agent result.
type Report struct {
	Repository  string    `json:"repository"`
	Branch      string    `json:"branch"`
	FromSHA     string    `json:"from_sha"`
	ToSHA       string    `json:"to_sha"`
	Agent       string    `json:"agent"`
	Verdict     string    `json:"verdict"`
	Summary     string    `json:"summary"`
	Findings    []Finding `json:"findings"`
	GeneratedAt time.Time `json:"generated_at"`
}

// ReviewFailureReport contains the stable fields shown in a failed-review notification.
type ReviewFailureReport struct {
	Repository   string    `json:"repository"`
	Branch       string    `json:"branch"`
	FromSHA      string    `json:"from_sha"`
	HeadSHA      string    `json:"head_sha"`
	Agent        string    `json:"agent"`
	Error        string    `json:"error"`
	FailureCount int       `json:"failure_count"`
	RetryCount   int       `json:"retry_count"`
	RetryAfter   string    `json:"retry_after"`
	GeneratedAt  time.Time `json:"generated_at"`
}

// PendingNotification is one durable item in a repository's delivery queue.
type PendingNotification struct {
	ID       string `json:"id"`
	NextCard int    `json:"next_card,omitempty"`
	Report   Report `json:"report"`
}
