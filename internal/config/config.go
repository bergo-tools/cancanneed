package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cancanneed/internal/model"

	"gopkg.in/yaml.v3"
)

const (
	defaultPollInterval = 5 * time.Minute
	defaultAgentTimeout = 30 * time.Minute
	defaultRetryBackoff = 15 * time.Second
)

var feishuIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Duration accepts Go duration strings in YAML, for example "30s" or "5m".
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

type Config struct {
	PollInterval   Duration                       `yaml:"poll_interval"`
	StateDir       string                         `yaml:"state_dir"`
	RunsDir        string                         `yaml:"runs_dir"`
	AuthorsFile    string                         `yaml:"authors_file,omitempty"`
	AuthorMentions map[string]model.AuthorMention `yaml:"-"`
	Concurrency    int                            `yaml:"concurrency"`
	Repositories   []Repository                   `yaml:"repositories"`
	Feishu         *Feishu                        `yaml:"feishu,omitempty"`
}

type Repository struct {
	Name   string `yaml:"name"`
	Path   string `yaml:"path"`
	Remote string `yaml:"remote"`
	Branch string `yaml:"branch,omitempty"`
	Agent  Agent  `yaml:"agent"`
}

type Agent struct {
	Type          string            `yaml:"type"`
	Command       string            `yaml:"command,omitempty"`
	Args          []string          `yaml:"args,omitempty"`
	Env           map[string]string `yaml:"env,omitempty"`
	RecordSession bool              `yaml:"record_session,omitempty"`
	Timeout       Duration          `yaml:"timeout,omitempty"`
	Retries       *int              `yaml:"retries,omitempty"`
	RetryBackoff  Duration          `yaml:"retry_backoff,omitempty"`
}

func (a Agent) RetryCount() int {
	if a.Retries == nil {
		return 0
	}
	return *a.Retries
}

type Feishu struct {
	Webhook string   `yaml:"webhook"`
	Secret  string   `yaml:"secret,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Config{}, errors.New("decode config: multiple YAML documents are not supported")
	} else if !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode trailing config: %w", err)
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("resolve config directory: %w", err)
	}
	authorsConfigured := strings.TrimSpace(cfg.AuthorsFile) != ""
	cfg.applyDefaults(baseDir)
	if authorsConfigured {
		if cfg.AuthorsFile == "" {
			return Config{}, errors.New("authors_file cannot expand to an empty path")
		}
		mentions, err := loadAuthorMentions(cfg.AuthorsFile)
		if err != nil {
			return Config{}, err
		}
		cfg.AuthorMentions = mentions
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults(baseDir string) {
	if c.PollInterval == 0 {
		c.PollInterval = Duration(defaultPollInterval)
	}
	if c.StateDir == "" {
		c.StateDir = ".cancanneed/state"
	}
	if c.RunsDir == "" {
		c.RunsDir = ".cancanneed/runs"
	}
	c.StateDir = expandedPath(baseDir, c.StateDir)
	c.RunsDir = expandedPath(baseDir, c.RunsDir)
	if c.AuthorsFile != "" {
		c.AuthorsFile = expandedPath(baseDir, c.AuthorsFile)
	}
	if c.Concurrency == 0 {
		c.Concurrency = 4
	}

	for i := range c.Repositories {
		repo := &c.Repositories[i]
		if repo.Path != "" {
			expanded := os.ExpandEnv(repo.Path)
			if expanded == "" {
				repo.Path = ""
			} else {
				repo.Path = absoluteFrom(baseDir, expanded)
			}
		}
		if repo.Remote == "" {
			repo.Remote = "origin"
		}
		repo.Agent.Type = strings.ToLower(repo.Agent.Type)
		if repo.Agent.Timeout == 0 {
			repo.Agent.Timeout = Duration(defaultAgentTimeout)
		}
		if repo.Agent.RetryBackoff == 0 {
			repo.Agent.RetryBackoff = Duration(defaultRetryBackoff)
		}
		if repo.Agent.Retries == nil {
			retries := 2
			repo.Agent.Retries = &retries
		}
		for key, value := range repo.Agent.Env {
			repo.Agent.Env[key] = os.ExpandEnv(value)
		}
		applyAgentCommandDefaults(&repo.Agent)
		applyAgentAutomationDefaults(&repo.Agent)
	}
	if c.Feishu != nil {
		c.Feishu.Webhook = os.ExpandEnv(c.Feishu.Webhook)
		c.Feishu.Secret = os.ExpandEnv(c.Feishu.Secret)
		if c.Feishu.Timeout == 0 {
			c.Feishu.Timeout = Duration(10 * time.Second)
		}
	}
}

func loadAuthorMentions(path string) (map[string]model.AuthorMention, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open authors file: %w", err)
	}
	defer f.Close()

	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var decoded map[string]model.AuthorMention
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode authors file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("decode authors file: multiple JSON values are not supported")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode authors file trailing data: %w", err)
	}
	if decoded == nil {
		return nil, errors.New("decode authors file: top-level value must be an object")
	}

	mentions := make(map[string]model.AuthorMention, len(decoded))
	for author, mention := range decoded {
		author = strings.TrimSpace(author)
		mention.FeishuID = strings.TrimSpace(mention.FeishuID)
		mention.Name = strings.TrimSpace(mention.Name)
		if author == "" {
			return nil, errors.New("authors file contains an empty author")
		}
		if mention.FeishuID == "" || mention.Name == "" {
			return nil, fmt.Errorf("authors file entry %q requires feishu_id and name", author)
		}
		if !feishuIDPattern.MatchString(mention.FeishuID) {
			return nil, fmt.Errorf("authors file entry %q has invalid feishu_id", author)
		}
		normalized := strings.ToLower(author)
		if _, exists := mentions[normalized]; exists {
			return nil, fmt.Errorf("authors file contains duplicate author %q ignoring case", author)
		}
		mentions[normalized] = mention
	}
	return mentions, nil
}

func applyAgentCommandDefaults(agent *Agent) {
	if agent.Command == "" {
		switch agent.Type {
		case "pi":
			agent.Command = "pi"
		case "ohmypi":
			agent.Command = "omp"
		case "crush":
			agent.Command = "crush"
		}
	}
	if len(agent.Args) != 0 {
		return
	}
	switch agent.Type {
	case "pi", "ohmypi":
		agent.Args = []string{"-p", "{prompt}"}
	case "crush":
		agent.Args = []string{"run", "{prompt}"}
	}
}

func applyAgentAutomationDefaults(agent *Agent) {
	args := append([]string(nil), agent.Args...)
	switch agent.Type {
	case "pi":
		args = removeBooleanFlags(args, "--approve", "-a", "--no-approve", "-na")
		args = append([]string{"--approve"}, args...)
		args = configureNoSession(args, agent.RecordSession)
	case "ohmypi":
		args = removeFlagWithValue(args, "--approval-mode")
		args = removeBooleanFlags(args, "--auto-approve", "--yolo")
		args = append([]string{"--auto-approve"}, args...)
		args = configureNoSession(args, agent.RecordSession)
	case "crush":
		args = removeBooleanFlags(args, "--yolo", "-y")
		args = append([]string{"--yolo"}, args...)
	}
	agent.Args = args
}

func configureNoSession(args []string, record bool) []string {
	args = removeBooleanFlags(args, "--no-session")
	if !record {
		args = append([]string{"--no-session"}, args...)
	}
	return args
}

func removeBooleanFlags(args []string, flags ...string) []string {
	blocked := make(map[string]struct{}, len(flags))
	for _, flag := range flags {
		blocked[flag] = struct{}{}
	}
	result := make([]string, 0, len(args))
	for _, argument := range args {
		if _, remove := blocked[argument]; !remove {
			result = append(result, argument)
		}
	}
	return result
}

func removeFlagWithValue(args []string, flags ...string) []string {
	blocked := make(map[string]struct{}, len(flags))
	for _, flag := range flags {
		blocked[flag] = struct{}{}
	}
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if _, remove := blocked[argument]; remove {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		remove := false
		for flag := range blocked {
			if strings.HasPrefix(argument, flag+"=") {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, argument)
		}
	}
	return result
}

func (c Config) validate() error {
	var problems []error
	if c.PollInterval.Value() <= 0 {
		problems = append(problems, errors.New("poll_interval must be positive"))
	}
	if c.Concurrency < 1 {
		problems = append(problems, errors.New("concurrency must be at least 1"))
	}
	if c.StateDir == "" {
		problems = append(problems, errors.New("state_dir cannot expand to an empty path"))
	}
	if c.RunsDir == "" {
		problems = append(problems, errors.New("runs_dir cannot expand to an empty path"))
	}
	if len(c.Repositories) == 0 {
		problems = append(problems, errors.New("at least one repository is required"))
	}
	names := make(map[string]struct{}, len(c.Repositories))
	for i, repo := range c.Repositories {
		prefix := fmt.Sprintf("repositories[%d]", i)
		if strings.TrimSpace(repo.Name) == "" {
			problems = append(problems, fmt.Errorf("%s.name is required", prefix))
		} else if _, exists := names[repo.Name]; exists {
			problems = append(problems, fmt.Errorf("duplicate repository name %q", repo.Name))
		} else {
			names[repo.Name] = struct{}{}
		}
		if repo.Path == "" {
			problems = append(problems, fmt.Errorf("%s.path is required", prefix))
		}
		if repo.Agent.Type != "pi" && repo.Agent.Type != "ohmypi" && repo.Agent.Type != "crush" {
			problems = append(problems, fmt.Errorf("%s.agent.type must be pi, ohmypi, or crush", prefix))
		}
		if repo.Agent.Command == "" {
			problems = append(problems, fmt.Errorf("%s.agent.command is required", prefix))
		}
		if repo.Agent.Timeout.Value() <= 0 {
			problems = append(problems, fmt.Errorf("%s.agent.timeout must be positive", prefix))
		}
		if repo.Agent.RetryCount() < 0 {
			problems = append(problems, fmt.Errorf("%s.agent.retries cannot be negative", prefix))
		}
		if repo.Agent.RetryBackoff.Value() < 0 {
			problems = append(problems, fmt.Errorf("%s.agent.retry_backoff cannot be negative", prefix))
		}
		for key := range repo.Agent.Env {
			if strings.TrimSpace(key) == "" || strings.Contains(key, "=") {
				problems = append(problems, fmt.Errorf("%s.agent.env contains invalid name %q", prefix, key))
			}
		}
	}
	if c.Feishu != nil {
		if c.Feishu.Webhook == "" {
			problems = append(problems, errors.New("feishu.webhook is required when feishu is configured"))
		}
		if c.Feishu.Timeout.Value() <= 0 {
			problems = append(problems, errors.New("feishu.timeout must be positive"))
		}
	}
	return errors.Join(problems...)
}

func absoluteFrom(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}

func expandedPath(baseDir, path string) string {
	path = os.ExpandEnv(path)
	if path == "" {
		return ""
	}
	return absoluteFrom(baseDir, path)
}
