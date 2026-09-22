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
