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
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	repositoryNames := make([]string, len(cfg.Repositories))
	for i, repository := range cfg.Repositories {
		repositoryNames[i] = repository.Name
	}
	store, err := state.Open(cfg.StateDir, repositoryNames...)
	if err != nil {
		return err
	}
	defer store.Close()

	var notifier notify.Notifier = notify.Nop{}
	if cfg.Feishu != nil {
		notifier = notify.Feishu{
			Webhook:        cfg.Feishu.Webhook,
			Secret:         cfg.Feishu.Secret,
			AuthorMentions: cfg.AuthorMentions,
			Client:         &http.Client{Timeout: cfg.Feishu.Timeout.Value()},
		}
	}
	service := &app.App{
		Config:   cfg,
		State:    store,
		Reviewer: review.Reviewer{RunsDir: cfg.RunsDir, Runner: agent.Runner{}},
		Notifier: notifier,
		Logger:   logger,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "once" {
		return service.RunOnce(ctx)
	}
	logger.Info("cancanneed started", "repositories", len(cfg.Repositories), "poll_interval", cfg.PollInterval.Value())
	return service.Run(ctx)
}
