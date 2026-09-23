package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"cancanneed/internal/agent"
	"cancanneed/internal/app"
	"cancanneed/internal/config"
	"cancanneed/internal/notify"
	"cancanneed/internal/review"
	"cancanneed/internal/state"
	"cancanneed/internal/submission"
)

func main() {
	if err := run(os.Args[1:]); errors.Is(err, flag.ErrHelp) {
		return
	} else if err != nil {
		slog.Error("cancanneed stopped", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if err := ensureSupportedPlatform(); err != nil {
		return err
	}
	if len(arguments) > 0 && arguments[0] == "__submit" {
		return submission.Run(arguments[1:])
	}
	command := "run"
	if len(arguments) > 0 && (arguments[0] == "run" || arguments[0] == "once") {
		command = arguments[0]
		arguments = arguments[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), usageText()) }
	configPath := flags.String("config", "cancanneed.yaml", "path to YAML configuration")
	debug := flags.Bool("debug", false, "enable debug logs")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	logger.Info("loading configuration", "path", *configPath, "mode", command)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logLoadedConfig(logger, *configPath, command, cfg)
	repositoryNames := make([]string, len(cfg.Repositories))
	for i, repository := range cfg.Repositories {
		repositoryNames[i] = repository.Name
	}
	logger.Info("opening state store", "directory", cfg.StateDir, "repositories", len(repositoryNames))
	store, err := state.Open(cfg.StateDir, repositoryNames...)
	if err != nil {
		return err
	}
	defer store.Close()
	logger.Info("state store opened", "directory", cfg.StateDir)

	var notifier notify.Notifier = notify.Nop{}
	if cfg.Feishu != nil {
		notifier = notify.Feishu{
			Webhook:        cfg.Feishu.Webhook,
			Secret:         cfg.Feishu.Secret,
			AuthorMentions: cfg.AuthorMentions,
			Client:         &http.Client{Timeout: cfg.Feishu.Timeout.Value()},
		}
		logger.Info("Feishu notifications configured",
			"timeout", cfg.Feishu.Timeout.Value(),
			"signature_enabled", cfg.Feishu.Secret != "",
			"author_mappings", len(cfg.AuthorMentions),
		)
	} else {
		logger.Info("Feishu notifications disabled")
	}
	service := &app.App{
		Config:   cfg,
		State:    store,
		Reviewer: review.Reviewer{RunsDir: cfg.RunsDir, MaxRunsPerRepository: cfg.MaxReviewRuns, Runner: agent.Runner{}},
		Notifier: notifier,
		Logger:   logger,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("cancanneed started", "mode", command, "repositories", len(cfg.Repositories), "poll_interval", cfg.PollInterval.Value())
	if command == "once" {
		err := service.RunOnce(ctx)
		if err == nil {
			logger.Info("one-time repository check completed")
		}
		return err
	}
	err = service.Run(ctx)
	if err == nil {
		logger.Info("cancanneed stopped")
	}
	return err
}

func usageText() string {
	return `cancanneed：定期检查 Git 仓库，调用 coding agent 审查新增提交。

用法：
  cancanneed [run] [选项]     持续运行；每个仓库独立轮询（默认）
  cancanneed once [选项]      检查所有仓库一次后退出

选项：
  -config PATH              YAML 配置文件路径（默认 cancanneed.yaml）
  -debug                    输出调试日志
  -h, --help                显示本帮助；也可用于 run 和 once

快速开始：
  cp config.example.yaml cancanneed.yaml
  # 修改仓库路径、agent 和可选的飞书配置
  cancanneed run -config cancanneed.yaml
  cancanneed once -config cancanneed.yaml

最小配置示例：
  repositories:
    - name: backend
      path: /path/to/git-repository
      agent:
        type: pi

配置要点：
  repositories       可配置多个仓库；agent.type 支持 pi、ohmypi、crush
  poll_interval      run 模式的检查间隔，默认 5m；失败后也按此间隔重试
  concurrency        once 模式的并发仓库数，默认 4
  state_dir          每仓库独立保存 HEAD 和通知进度，默认 .cancanneed/state
  runs_dir           review 临时文件和日志目录，默认 .cancanneed/runs
  max_review_runs    每仓库保留的运行目录数，默认 10
  authors_file       可选的作者到飞书用户映射 JSON 文件
  feishu.webhook     可选的飞书群机器人 Webhook；未配置则不发送通知

首次审查只检查监控分支的最新一个 commit；之后从已记录的 HEAD 增量审查。
配置中的相对路径以配置文件目录为基准。完整字段和注释见 config.example.yaml。
`
}

func logLoadedConfig(logger *slog.Logger, path, mode string, cfg config.Config) {
	logger.Info("configuration loaded",
		"path", path,
		"mode", mode,
		"repositories", len(cfg.Repositories),
		"poll_interval", cfg.PollInterval.Value(),
		"state_directory", cfg.StateDir,
		"runs_directory", cfg.RunsDir,
		"max_review_runs", cfg.MaxReviewRuns,
		"once_concurrency", cfg.Concurrency,
		"authors_file", cfg.AuthorsFile,
		"author_mappings", len(cfg.AuthorMentions),
		"feishu_enabled", cfg.Feishu != nil,
	)
	for _, repository := range cfg.Repositories {
		branch := repository.Branch
		if branch == "" {
			branch = "auto"
		}
		logger.Info("repository configured",
			"repository", repository.Name,
			"path", repository.Path,
			"branch", branch,
			"agent", repository.Agent.Type,
			"agent_command", repository.Agent.Command,
			"agent_timeout", repository.Agent.Timeout.Value(),
			"agent_retries", repository.Agent.RetryCount(),
			"agent_retry_backoff", repository.Agent.RetryBackoff.Value(),
			"record_session", repository.Agent.RecordSession,
			"agent_environment_variables", len(repository.Agent.Env),
		)
	}
}
