package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"cancanneed/internal/config"
	"cancanneed/internal/model"
)

func TestLogLoadedConfigIncludesUsefulFieldsWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	retries := 2
	cfg := config.Config{
		PollInterval:  config.Duration(5 * time.Minute),
		StateDir:      "/data/state",
		RunsDir:       "/data/runs",
		MaxReviewRuns: 10,
		AuthorsFile:   "/config/authors.json",
		AuthorMentions: map[string]model.AuthorMention{
			"alice": {FeishuID: "ou-secret-id", Name: "Alice"},
		},
		Concurrency: 4,
		Repositories: []config.Repository{{
			Name: "api", Path: "/repos/api", Branch: "main",
			Agent: config.Agent{
				Type: "pi", Command: "pi", Env: map[string]string{"API_TOKEN": "super-secret-token"},
				Timeout: config.Duration(30 * time.Minute), Retries: &retries, RetryBackoff: config.Duration(15 * time.Second),
			},
		}},
		Feishu: &config.Feishu{Webhook: "https://example.invalid/secret-hook", Secret: "signing-secret", Timeout: config.Duration(10 * time.Second)},
	}

	logLoadedConfig(logger, "/config/cancanneed.yaml", "run", cfg)
	logged := output.String()
	for _, expected := range []string{"configuration loaded", "repository configured", "api", "/repos/api", "agent_environment_variables=1"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log does not contain %q: %s", expected, logged)
		}
	}
	for _, secret := range []string{"super-secret-token", "secret-hook", "signing-secret", "ou-secret-id"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
}
