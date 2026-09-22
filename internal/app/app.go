package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"cancanneed/internal/config"
	gitrepo "cancanneed/internal/git"
	"cancanneed/internal/model"
	"cancanneed/internal/notify"
	"cancanneed/internal/review"
	"cancanneed/internal/state"
)

type App struct {
	Config                    config.Config
	State                     *state.Store
	Reviewer                  review.Reviewer
	Notifier                  notify.Notifier
	Logger                    *slog.Logger
	Now                       func() time.Time
	processRepositoryOverride func(context.Context, config.Repository) error
}

const reviewFailureNotificationInterval = 30

func (a *App) Run(ctx context.Context) error {
	var monitors sync.WaitGroup
	for _, repository := range a.Config.Repositories {
		repository := repository
		monitors.Add(1)
		go func() {
			defer monitors.Done()
			a.monitorRepository(ctx, repository)
		}()
	}
	monitors.Wait()
	return nil
}

func (a *App) monitorRepository(ctx context.Context, repository config.Repository) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := a.runRepository(ctx, repository); err != nil && !errors.Is(err, context.Canceled) {
			a.logger().Error("repository poll failed", "repository", repository.Name, "error", err)
		}

		timer := time.NewTimer(a.Config.PollInterval.Value())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *App) RunOnce(ctx context.Context) error {
	limit := a.Config.Concurrency
	if limit > len(a.Config.Repositories) {
		limit = len(a.Config.Repositories)
	}
	semaphore := make(chan struct{}, limit)
	errorsByRepo := make(chan error, len(a.Config.Repositories))
	var workers sync.WaitGroup
	for _, repository := range a.Config.Repositories {
		repository := repository
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				errorsByRepo <- ctx.Err()
				return
			}
			if err := a.runRepository(ctx, repository); err != nil {
				errorsByRepo <- fmt.Errorf("%s: %w", repository.Name, err)
			}
		}()
	}
	workers.Wait()
	close(errorsByRepo)
	var collected []error
	for err := range errorsByRepo {
		collected = append(collected, err)
	}
	return errors.Join(collected...)
}

func (a *App) runRepository(ctx context.Context, repository config.Repository) error {
	if a.processRepositoryOverride != nil {
		return a.processRepositoryOverride(ctx, repository)
	}
	return a.processRepository(ctx, repository)
}

func (a *App) processRepository(ctx context.Context, repository config.Repository) error {
	if err := a.Reviewer.Cleanup(repository.Name); err != nil {
		return fmt.Errorf("clean old review runs: %w", err)
	}
	current, exists := a.State.Get(repository.Name)
	var pendingDeliveryErr error
	if exists && len(current.PendingNotifications) != 0 {
		if err := a.deliverPending(ctx, repository.Name, current); err != nil {
			pendingDeliveryErr = err
			a.logger().Error("review notification delivery failed; repository review will continue", "repository", repository.Name, "error", err)
		}
		current, exists = a.State.Get(repository.Name)
	}
	if exists && len(current.PendingFailureNotifications) != 0 {
		if err := a.deliverPendingReviewFailure(ctx, repository.Name, current); err != nil {
			a.logger().Error("failed review notification delivery failed", "repository", repository.Name, "error", err)
		}
		current, exists = a.State.Get(repository.Name)
	}

	git := gitrepo.Repository{Path: repository.Path, Remote: repository.Remote}
	if err := git.Validate(ctx); err != nil {
		return errors.Join(pendingDeliveryErr, err)
	}
	branch, err := git.ResolveBranch(ctx, repository.Branch)
	if err != nil {
		return errors.Join(pendingDeliveryErr, err)
	}
	head, err := git.RemoteHead(ctx, branch)
	if err != nil {
		return errors.Join(pendingDeliveryErr, err)
	}

	latestOnly := !exists || current.HeadSHA == "" || current.Branch != branch
	reviewFrom := current.HeadSHA
	if latestOnly {
		if err := git.PinRemoteHead(ctx, branch, head); err != nil {
			return errors.Join(pendingDeliveryErr, err)
		}
		var err error
		reviewFrom, err = git.ReviewBase(ctx, head)
		if err != nil {
			return errors.Join(pendingDeliveryErr, err)
		}
		if !exists || current.Branch != branch {
			current = state.RepositoryState{
				Branch:                      branch,
				PendingNotifications:        current.PendingNotifications,
				PendingFailureNotifications: current.PendingFailureNotifications,
			}
		} else {
			current.Branch = branch
		}
	}
	if !latestOnly && current.HeadSHA == head {
		a.logger().Debug("repository unchanged", "repository", repository.Name, "branch", branch, "head", head)
		return pendingDeliveryErr
	}
	if delay := reviewRetryDelay(current.ReviewFailure, head, a.now(), a.Config.PollInterval.Value()); delay > 0 {
		a.logger().Debug("failed review is waiting for retry interval", "repository", repository.Name, "head", head, "retry_in", delay)
		return pendingDeliveryErr
	}

	a.logger().Info("repository update detected", "repository", repository.Name, "branch", branch, "from", reviewFrom, "to", head, "latest_only", latestOnly)
	report, runDir, err := a.Reviewer.Review(ctx, review.Request{
		Repository:  repository,
		Branch:      branch,
		FromSHA:     reviewFrom,
		ObservedSHA: head,
		LatestOnly:  latestOnly,
	})
	if err != nil {
		if ctx.Err() != nil {
			return errors.Join(pendingDeliveryErr, ctx.Err())
		}
		reviewErr := fmt.Errorf("review failed (run directory %s): %w", runDir, err)
		if failureErr := a.recordReviewFailure(ctx, repository, current, branch, reviewFrom, head, reviewErr); failureErr != nil {
			return errors.Join(pendingDeliveryErr, reviewErr, fmt.Errorf("record review failure: %w", failureErr))
		}
		return errors.Join(pendingDeliveryErr, reviewErr)
	}
	current.ReviewFailure = nil
	if report.Verdict == "skip" {
		current.HeadSHA = report.ToSHA
		current.Branch = branch
		current.UpdatedAt = a.now().UTC()
		if err := a.State.Put(repository.Name, current); err != nil {
			return errors.Join(pendingDeliveryErr, fmt.Errorf("save skipped review state: %w", err))
		}
		a.logger().Info("repository update skipped", "repository", repository.Name, "head", report.ToSHA, "reason", report.Summary)
		return pendingDeliveryErr
	}
	pending := model.PendingNotification{ID: repository.Name + ":" + report.ToSHA, Report: report}
	current.HeadSHA = report.ToSHA
	current.Branch = branch
	current.UpdatedAt = a.now().UTC()
	current.PendingNotifications = append(current.PendingNotifications, pending)
	if err := a.State.Put(repository.Name, current); err != nil {
		return errors.Join(pendingDeliveryErr, fmt.Errorf("save review state: %w", err))
	}
	a.logger().Info("review completed", "repository", repository.Name, "verdict", report.Verdict, "findings", len(report.Findings), "run_directory", runDir)
	if pendingDeliveryErr != nil {
		return pendingDeliveryErr
	}
	return a.deliverPending(ctx, repository.Name, current)
}

func (a *App) deliverPending(ctx context.Context, name string, current state.RepositoryState) error {
	for len(current.PendingNotifications) != 0 {
		pending := &current.PendingNotifications[0]
		total := a.Notifier.NotificationCount(pending.Report)
		if pending.NextCard < 0 || pending.NextCard > total {
			return fmt.Errorf("pending notification %s has invalid next card %d of %d", pending.ID, pending.NextCard, total)
		}
		for pending.NextCard < total {
			if err := a.Notifier.Notify(ctx, pending.Report, pending.NextCard); err != nil {
				return fmt.Errorf("send pending notification %s card %d/%d: %w", pending.ID, pending.NextCard+1, total, err)
			}
			pending.NextCard++
			if err := a.State.Put(name, current); err != nil {
				return fmt.Errorf("save notification progress: %w", err)
			}
		}
		current.PendingNotifications = current.PendingNotifications[1:]
		if err := a.State.Put(name, current); err != nil {
			return fmt.Errorf("mark notification %s delivered: %w", pending.ID, err)
		}
	}
	a.logger().Info("review notification delivered", "repository", name)
	return nil
}

func (a *App) recordReviewFailure(
	ctx context.Context,
	repository config.Repository,
	current state.RepositoryState,
	branch string,
	fromSHA string,
	head string,
	reviewErr error,
) error {
	failure := state.ReviewFailureState{HeadSHA: head}
	if current.ReviewFailure != nil && current.ReviewFailure.HeadSHA == head {
		failure = *current.ReviewFailure
	}
	failure.Count++
	failure.LastError = truncateText(reviewErr.Error(), 4000)
	failure.LastFailedAt = a.now().UTC()

	notificationDue := failure.Count == 1 || failure.Count%reviewFailureNotificationInterval == 0
	if notificationDue {
		current.PendingFailureNotifications = append(current.PendingFailureNotifications, model.ReviewFailureReport{
			Repository:   repository.Name,
			Branch:       branch,
			FromSHA:      fromSHA,
			HeadSHA:      head,
			Agent:        repository.Agent.Type,
			Error:        failure.LastError,
			FailureCount: failure.Count,
			RetryCount:   failure.Count - 1,
			RetryAfter:   a.Config.PollInterval.Value().String(),
			GeneratedAt:  failure.LastFailedAt,
		})
	}
	current.ReviewFailure = &failure
	if err := a.State.Put(repository.Name, current); err != nil {
		return fmt.Errorf("save failed review state: %w", err)
	}
	if notificationDue {
		return a.deliverPendingReviewFailure(ctx, repository.Name, current)
	}
	return nil
}

func (a *App) deliverPendingReviewFailure(ctx context.Context, name string, current state.RepositoryState) error {
	for len(current.PendingFailureNotifications) != 0 {
		report := current.PendingFailureNotifications[0]
		if err := a.Notifier.NotifyReviewFailure(ctx, report); err != nil {
			return fmt.Errorf("send failed review notification for %s: %w", report.HeadSHA, err)
		}
		current.PendingFailureNotifications = current.PendingFailureNotifications[1:]
		if err := a.State.Put(name, current); err != nil {
			return fmt.Errorf("mark failed review notification delivered: %w", err)
		}
	}
	return nil
}

func truncateText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func reviewRetryDelay(failure *state.ReviewFailureState, head string, now time.Time, interval time.Duration) time.Duration {
	if failure == nil || failure.HeadSHA != head || failure.LastFailedAt.IsZero() {
		return 0
	}
	delay := failure.LastFailedAt.Add(interval).Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func (a *App) logger() *slog.Logger {
	if a.Logger == nil {
		return slog.Default()
	}
	return a.Logger
}

func (a *App) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}
