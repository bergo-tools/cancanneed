package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadAppliesDefaultsAndExpandsEnvironment(t *testing.T) {
	t.Setenv("TEST_AGENT_TOKEN", "secret-value")
	dir := t.TempDir()
	path := filepath.Join(dir, "cancanneed.yaml")
	contents := `
poll_interval: 30s
repositories:
  - name: api
    path: ./repo
    agent:
      type: ohmypi
      env:
        TOKEN: ${TEST_AGENT_TOKEN}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval.Value() != 30*time.Second {
		t.Fatalf("poll interval = %s", cfg.PollInterval.Value())
	}
	if cfg.Repositories[0].Path != filepath.Join(dir, "repo") {
		t.Fatalf("path = %q", cfg.Repositories[0].Path)
	}
	if cfg.Repositories[0].Agent.Command != "omp" {
		t.Fatalf("command = %q", cfg.Repositories[0].Agent.Command)
	}
	if !reflect.DeepEqual(cfg.Repositories[0].Agent.Args, []string{"--no-session", "--auto-approve", "-p", "{prompt}"}) {
		t.Fatalf("args = %#v", cfg.Repositories[0].Agent.Args)
	}
	if cfg.Repositories[0].Agent.RecordSession {
		t.Fatal("record_session should default to false")
	}
	if cfg.Repositories[0].Agent.Timeout.Value() != 30*time.Minute {
		t.Fatalf("timeout = %s", cfg.Repositories[0].Agent.Timeout.Value())
	}
	if cfg.Repositories[0].Agent.RetryCount() != 2 {
		t.Fatalf("retries = %d", cfg.Repositories[0].Agent.RetryCount())
	}
	if cfg.Repositories[0].Agent.Env["TOKEN"] != "secret-value" {
		t.Fatal("environment was not expanded")
	}
}

func TestAgentAutomationArguments(t *testing.T) {
	tests := []struct {
		name          string
		agentType     string
		recordSession bool
		args          []string
		want          []string
	}{
		{"pi ephemeral", "pi", false, []string{"-p", "{prompt}"}, []string{"--no-session", "--approve", "-p", "{prompt}"}},
		{"pi recorded", "pi", true, []string{"-p", "{prompt}"}, []string{"--approve", "-p", "{prompt}"}},
		{"ohmypi ephemeral", "ohmypi", false, []string{"-p", "{prompt}"}, []string{"--no-session", "--auto-approve", "-p", "{prompt}"}},
		{"ohmypi recorded", "ohmypi", true, []string{"-p", "{prompt}"}, []string{"--auto-approve", "-p", "{prompt}"}},
		{"crush session setting unsupported", "crush", false, []string{"run", "{prompt}"}, []string{"--yolo", "run", "{prompt}"}},
		{"crush recorded", "crush", true, []string{"run", "{prompt}"}, []string{"--yolo", "run", "{prompt}"}},
		{"crush moves yolo before subcommand", "crush", true, []string{"run", "--yolo", "{prompt}"}, []string{"--yolo", "run", "{prompt}"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := Agent{Type: test.agentType, Args: test.args, RecordSession: test.recordSession}
			applyAgentAutomationDefaults(&agent)
			if !reflect.DeepEqual(agent.Args, test.want) {
				t.Fatalf("args = %#v, want %#v", agent.Args, test.want)
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("unknown: true\nrepositories: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("FEISHU_WEBHOOK", "https://open.feishu.cn/open-apis/bot/v2/hook/test")
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if len(cfg.Repositories) != 1 {
		t.Fatalf("example repositories = %d, want 1", len(cfg.Repositories))
	}
	mention, ok := cfg.AuthorMentions["alice"]
	if !ok || mention.FeishuID == "" || mention.Name != "张三" {
		t.Fatalf("example author mention = %#v", mention)
	}
}

func TestLoadAuthorMentionsFromExternalJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authors.json"), []byte(`{
  " Alice ": {"feishu_id":"ou_alice","name":" 张三 "}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configData := `
authors_file: authors.json
repositories:
  - name: api
    path: ./repo
    agent:
      type: pi
`
	configPath := filepath.Join(dir, "cancanneed.yaml")
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mention, ok := cfg.AuthorMentions["alice"]
	if !ok || mention.FeishuID != "ou_alice" || mention.Name != "张三" {
		t.Fatalf("author mention = %#v", mention)
	}
}

func TestLoadRejectsInvalidAuthorMention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authors.json"), []byte(`{"Alice":{"feishu_id":"ou_alice"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configData := "authors_file: authors.json\nrepositories:\n  - name: api\n    path: ./repo\n    agent:\n      type: pi\n"
	configPath := filepath.Join(dir, "cancanneed.yaml")
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configPath); err == nil {
		t.Fatal("expected invalid author mention to be rejected")
	}
}
