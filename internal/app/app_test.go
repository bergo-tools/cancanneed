package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cancanneed/internal/agent"
	"cancanneed/internal/config"
	"cancanneed/internal/model"
	"cancanneed/internal/review"
	"cancanneed/internal/state"
	"cancanneed/internal/submission"
)

func TestRunMonitorsRepositoriesIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slowStarted := make(chan struct{})
	fastPolledTwice := make(chan struct{})
	var slowStartOnce sync.Once
	var fastTwiceOnce sync.Once
	var fastPolls atomic.Int32

	service := &App{
		Config: config.Config{
			PollInterval: config.Duration(10 * time.Millisecond),
			Repositories: []config.Repository{{Name: "slow"}, {Name: "fast"}},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	service.processRepositoryOverride = func(ctx context.Context, repository config.Repository) error {
		if repository.Name == "slow" {
			slowStartOnce.Do(func() { close(slowStarted) })
			<-ctx.Done()
			return ctx.Err()
		}
		if fastPolls.Add(1) >= 2 {
			fastTwiceOnce.Do(func() { close(fastPolledTwice) })
		}
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow repository monitor did not start")
	}
	select {
	case <-fastPolledTwice:
	case <-time.After(time.Second):
		t.Fatal("fast repository was blocked by the slow repository")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunOnceReviewsLatestCommitWithoutBaseline(t *testing.T) {
	if os.Getenv("GO_WANT_APP_DUMMY") == "1" {
		appDummyAgent()
		return
	}
	ctx := context.Background()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	monitored := filepath.Join(dir, "monitored")
	runGit(t, dir, "init", "--bare", "--initial-branch=main", remote)
	runGit(t, dir, "init", "--initial-branch=main", seed)
	writeFile(t, filepath.Join(seed, "message.txt"), "initial\n")
	runGit(t, seed, "add", "message.txt")
	runGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, dir, "clone", remote, monitored)

	retries := 0
	cfg := config.Config{
		PollInterval: config.Duration(time.Minute),
		StateFile:    filepath.Join(dir, "state.json"),
		RunsDir:      filepath.Join(dir, "runs"),
		Concurrency:  1,
		Repositories: []config.Repository{{
			Name:   "demo",
			Path:   monitored,
			Remote: "origin",
			Agent: config.Agent{
				Type:         "pi",
				Command:      os.Args[0],
				Args:         []string{"-test.run=TestRunOnceReviewsLatestCommitWithoutBaseline", "--", "{prompt}"},
				Env:          map[string]string{"GO_WANT_APP_DUMMY": "1"},
				Timeout:      config.Duration(10 * time.Second),
				Retries:      &retries,
				RetryBackoff: config.Duration(time.Millisecond),
			},
		}},
	}
	store, err := state.Open(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notifier := &failOnceNotifier{failuresRemaining: 1}
	service := &App{
		Config: cfg,
		State:  store,
		Reviewer: review.Reviewer{
			RunsDir:       cfg.RunsDir,
			Runner:        agent.Runner{},
			SubmitCommand: []string{os.Args[0], "-test.run=TestSubmissionCLIHelper", "--"},
		},
		Notifier: notifier,
	}
	initialHead := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	if err := service.RunOnce(ctx); err == nil {
		t.Fatal("expected the initial review notification delivery to fail")
	}
	initialState, ok := store.Get("demo")
	if !ok || initialState.HeadSHA != initialHead || initialState.Branch != "main" || len(initialState.PendingNotifications) != 1 {
		t.Fatalf("initial review state: %#v", initialState)
	}
	initialRuns, err := filepath.Glob(filepath.Join(cfg.RunsDir, "*"))
	if err != nil || len(initialRuns) != 1 {
		t.Fatalf("initial run directories = %v, err = %v", initialRuns, err)
	}
	var requestMetadata struct {
		FromSHA     string `json:"from_sha"`
		ObservedSHA string `json:"observed_sha"`
		LatestOnly  bool   `json:"latest_only"`
	}
	requestBytes, err := os.ReadFile(filepath.Join(initialRuns[0], "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(requestBytes, &requestMetadata); err != nil {
		t.Fatal(err)
	}
	if !requestMetadata.LatestOnly || requestMetadata.ObservedSHA != initialHead || requestMetadata.FromSHA == "" || requestMetadata.FromSHA == initialHead {
		t.Fatalf("initial request metadata = %#v", requestMetadata)
	}
	if objectType := strings.TrimSpace(runGit(t, monitored, "cat-file", "-t", requestMetadata.FromSHA)); objectType != "tree" {
		t.Fatalf("root commit review base type = %q, want tree", objectType)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatalf("retry initial review notification: %v", err)
	}
	initialState, _ = store.Get("demo")
	if len(initialState.PendingNotifications) != 0 || notifier.calls != 2 {
		t.Fatalf("initial pending = %#v, calls = %d", initialState.PendingNotifications, notifier.calls)
	}

	writeFile(t, filepath.Join(seed, "message.txt"), "initial\nupdated\n")
	runGit(t, seed, "add", "message.txt")
	runGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "update")
	runGit(t, seed, "push", "origin", "main")
	observedHead := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	withFailure, _ := store.Get("demo")
	withFailure.ReviewFailure = &state.ReviewFailureState{HeadSHA: observedHead, Count: 7, LastError: "old failure"}
	if err := store.Put("demo", withFailure); err != nil {
		t.Fatal(err)
	}

	service.Config.Repositories[0].Agent.Env["DUMMY_ADVANCE_REPO"] = seed
	if err := service.RunOnce(ctx); err != nil {
		t.Fatalf("review update: %v", err)
	}
	delete(service.Config.Repositories[0].Agent.Env, "DUMMY_ADVANCE_REPO")
	newHead := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	if newHead == observedHead {
		t.Fatal("dummy agent did not advance the remote after the poll")
	}
	updated, _ := store.Get("demo")
	if updated.HeadSHA != newHead {
		t.Fatalf("head = %s, want %s", updated.HeadSHA, newHead)
	}
	if len(updated.PendingNotifications) != 0 {
		t.Fatalf("notification should be delivered: %#v", updated.PendingNotifications)
	}
	if updated.ReviewFailure != nil {
		t.Fatalf("successful review did not clear failure count: %#v", updated.ReviewFailure)
	}
	if notifier.calls != 3 {
		t.Fatalf("notification calls = %d, want 3", notifier.calls)
	}

	// A notification outage must not prevent a later remote HEAD from being reviewed.
	notifier.failuresRemaining = 2
	writeFile(t, filepath.Join(seed, "message.txt"), "queued notification one\n")
	runGit(t, seed, "add", "message.txt")
	runGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "queued notification one")
	runGit(t, seed, "push", "origin", "main")
	queuedHeadOne := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	if err := service.RunOnce(ctx); err == nil {
		t.Fatal("expected first queued notification delivery to fail")
	}
	queued, _ := store.Get("demo")
	if queued.HeadSHA != queuedHeadOne || len(queued.PendingNotifications) != 1 {
		t.Fatalf("first queued review state = %#v", queued)
	}

	writeFile(t, filepath.Join(seed, "message.txt"), "queued notification two\n")
	runGit(t, seed, "add", "message.txt")
	runGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "queued notification two")
	runGit(t, seed, "push", "origin", "main")
	queuedHeadTwo := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	if err := service.RunOnce(ctx); err == nil {
		t.Fatal("expected pending delivery to fail while the newer HEAD was still reviewed")
	}
	queued, _ = store.Get("demo")
	if queued.HeadSHA != queuedHeadTwo || len(queued.PendingNotifications) != 2 {
		t.Fatalf("second queued review state = %#v", queued)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatalf("flush queued notifications: %v", err)
	}
	queued, _ = store.Get("demo")
	if len(queued.PendingNotifications) != 0 {
		t.Fatalf("queued notifications were not drained: %#v", queued.PendingNotifications)
	}

	service.Config.Repositories[0].Agent.Env["DUMMY_VERDICT"] = "skip"
	writeFile(t, filepath.Join(seed, "message.txt"), "initial\nupdated\nskipped\n")
	runGit(t, seed, "add", "message.txt")
	runGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "chore: noreview")
	runGit(t, seed, "push", "origin", "main")
	skippedHead := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	if err := service.RunOnce(ctx); err != nil {
		t.Fatalf("skip update: %v", err)
	}
	updated, _ = store.Get("demo")
	if updated.HeadSHA != skippedHead || len(updated.PendingNotifications) != 0 || notifier.calls != 7 {
		t.Fatalf("skipped state = %#v, notification calls = %d", updated, notifier.calls)
	}

	writeFile(t, filepath.Join(seed, "message.txt"), "cancel this review\n")
	runGit(t, seed, "add", "message.txt")
	runGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "cancel review")
	runGit(t, seed, "push", "origin", "main")
	readyPath := filepath.Join(dir, "agent-ready")
	service.Config.Repositories[0].Agent.Env["DUMMY_READY_FILE"] = readyPath
	reviewCtx, cancelReview := context.WithCancel(context.Background())
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			if _, err := os.Stat(readyPath); err == nil {
				cancelReview()
				return
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				cancelReview()
				return
			}
		}
	}()
	err = service.RunOnce(reviewCtx)
	<-cancelDone
	cancelReview()
	delete(service.Config.Repositories[0].Agent.Env, "DUMMY_READY_FILE")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled review error = %v", err)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("agent did not start before cancellation: %v", err)
	}
	updated, _ = store.Get("demo")
	if updated.HeadSHA != skippedHead || updated.ReviewFailure != nil {
		t.Fatalf("canceled review changed persistent review state: %#v", updated)
	}

	matches, err := filepath.Glob(filepath.Join(cfg.RunsDir, "*", "review.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("review artifacts = %v, err = %v", matches, err)
	}
	runDirs, err := filepath.Glob(filepath.Join(cfg.RunsDir, "*"))
	if err != nil || len(runDirs) != 6 {
		t.Fatalf("run directories = %v, err = %v", runDirs, err)
	}
	for _, runDir := range runDirs {
		for _, removedArtifact := range []string{"prepare-review.sh", "prompt.md"} {
			if _, err := os.Stat(filepath.Join(runDir, removedArtifact)); !os.IsNotExist(err) {
				t.Fatalf("%s should not be generated", removedArtifact)
			}
		}
		if _, err := os.Stat(filepath.Join(runDir, "submit-review.sh")); err != nil {
			t.Fatalf("submit-review.sh missing: %v", err)
		}
	}
}

type failOnceNotifier struct {
	calls             int
	failuresRemaining int
}

func (n *failOnceNotifier) NotificationCount(model.Report) int { return 1 }

func (n *failOnceNotifier) Notify(context.Context, model.Report, int) error {
	n.calls++
	if n.failuresRemaining > 0 {
		n.failuresRemaining--
		return errors.New("intentional notification failure")
	}
	return nil
}

func (*failOnceNotifier) NotifyReviewFailure(context.Context, model.ReviewFailureReport) error {
	return nil
}

func TestDeliverPendingResumesFromFirstUnsentCard(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	want := state.RepositoryState{
		HeadSHA: "new-head",
		Branch:  "main",
		PendingNotifications: []model.PendingNotification{{
			ID:     "api:new-head",
			Report: model.Report{Repository: "api"},
		}},
	}
	if err := store.Put("api", want); err != nil {
		t.Fatal(err)
	}
	notifier := &failSecondCardOnceNotifier{}
	service := &App{
		State:    store,
		Notifier: notifier,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	current, _ := store.Get("api")
	if err := service.deliverPending(context.Background(), "api", current); err == nil {
		t.Fatal("expected second card to fail")
	}
	current, _ = store.Get("api")
	if len(current.PendingNotifications) != 1 || current.PendingNotifications[0].NextCard != 1 {
		t.Fatalf("next card after failure = %#v, want 1", current.PendingNotifications)
	}
	if err := service.deliverPending(context.Background(), "api", current); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get("api")
	if len(current.PendingNotifications) != 0 {
		t.Fatalf("notification still pending: %#v", current.PendingNotifications)
	}
	wantCalls := []int{0, 1, 1, 2}
	if len(notifier.calls) != len(wantCalls) {
		t.Fatalf("card calls = %v, want %v", notifier.calls, wantCalls)
	}
	for i := range wantCalls {
		if notifier.calls[i] != wantCalls[i] {
			t.Fatalf("card calls = %v, want %v", notifier.calls, wantCalls)
		}
	}
}

type failSecondCardOnceNotifier struct {
	calls  []int
	failed bool
}

func (*failSecondCardOnceNotifier) NotificationCount(model.Report) int { return 3 }

func (n *failSecondCardOnceNotifier) Notify(_ context.Context, _ model.Report, card int) error {
	n.calls = append(n.calls, card)
	if card == 1 && !n.failed {
		n.failed = true
		return errors.New("intentional second-card failure")
	}
	return nil
}

func (*failSecondCardOnceNotifier) NotifyReviewFailure(context.Context, model.ReviewFailureReport) error {
	return nil
}

func TestReviewFailuresNotifyFirstAndEveryThirtyForSameHead(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	initial := state.RepositoryState{HeadSHA: "old-head", Branch: "main"}
	if err := store.Put("api", initial); err != nil {
		t.Fatal(err)
	}
	notifier := &recordingNotifier{}
	service := &App{
		Config:   config.Config{PollInterval: config.Duration(2 * time.Minute)},
		State:    store,
		Notifier: notifier,
		Now:      func() time.Time { return time.Unix(1700000000, 0) },
	}
	repository := config.Repository{Name: "api", Agent: config.Agent{Type: "pi"}}
	for i := 0; i < 30; i++ {
		current, _ := store.Get("api")
		if err := service.recordReviewFailure(context.Background(), repository, current, "main", "old-head", "head-one", errors.New("agent failed")); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := store.Get("api")
	if current.ReviewFailure == nil || current.ReviewFailure.Count != 30 {
		t.Fatalf("failure state = %#v", current.ReviewFailure)
	}
	if len(notifier.failures) != 2 || notifier.failures[0].FailureCount != 1 || notifier.failures[1].FailureCount != 30 {
		t.Fatalf("failure notifications = %#v", notifier.failures)
	}
	if notifier.failures[1].RetryCount != 29 || notifier.failures[1].RetryAfter != "2m0s" {
		t.Fatalf("30th failure notification = %#v", notifier.failures[1])
	}

	if err := service.recordReviewFailure(context.Background(), repository, current, "main", "old-head", "head-two", errors.New("new head failed")); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get("api")
	if current.ReviewFailure == nil || current.ReviewFailure.Count != 1 || current.ReviewFailure.HeadSHA != "head-two" {
		t.Fatalf("new HEAD failure state = %#v", current.ReviewFailure)
	}
	if len(notifier.failures) != 3 || notifier.failures[2].FailureCount != 1 {
		t.Fatalf("new HEAD did not get an initial notification: %#v", notifier.failures)
	}
}

func TestReviewRetryDelayOnlyAppliesToTheSameHead(t *testing.T) {
	failedAt := time.Unix(1700000000, 0)
	failure := &state.ReviewFailureState{HeadSHA: "head-one", LastFailedAt: failedAt}
	if got := reviewRetryDelay(failure, "head-one", failedAt.Add(30*time.Second), 2*time.Minute); got != 90*time.Second {
		t.Fatalf("retry delay = %s, want 1m30s", got)
	}
	if got := reviewRetryDelay(failure, "head-two", failedAt.Add(30*time.Second), 2*time.Minute); got != 0 {
		t.Fatalf("new HEAD retry delay = %s, want 0", got)
	}
	if got := reviewRetryDelay(failure, "head-one", failedAt.Add(2*time.Minute), 2*time.Minute); got != 0 {
		t.Fatalf("elapsed retry delay = %s, want 0", got)
	}
}

func TestFailedReviewNotificationSurvivesARecoveredReview(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notifier := &failFailureOnceNotifier{}
	service := &App{
		Config:   config.Config{PollInterval: config.Duration(time.Minute)},
		State:    store,
		Notifier: notifier,
		Now:      func() time.Time { return time.Unix(1700000000, 0) },
	}
	repository := config.Repository{Name: "api", Agent: config.Agent{Type: "pi"}}
	if err := service.recordReviewFailure(context.Background(), repository, state.RepositoryState{}, "main", "old", "new", errors.New("agent failed")); err == nil {
		t.Fatal("expected initial failure notification delivery to fail")
	}
	current, _ := store.Get("api")
	if len(current.PendingFailureNotifications) != 1 {
		t.Fatalf("pending failure notifications = %#v", current.PendingFailureNotifications)
	}

	// A later successful review clears the retry counter, not its unsent notice.
	current.ReviewFailure = nil
	if err := store.Put("api", current); err != nil {
		t.Fatal(err)
	}
	if err := service.deliverPendingReviewFailure(context.Background(), "api", current); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Get("api")
	if len(current.PendingFailureNotifications) != 0 || notifier.calls != 2 {
		t.Fatalf("failure queue = %#v, calls = %d", current.PendingFailureNotifications, notifier.calls)
	}
}

type failFailureOnceNotifier struct{ calls int }

func (*failFailureOnceNotifier) NotificationCount(model.Report) int { return 0 }
func (*failFailureOnceNotifier) Notify(context.Context, model.Report, int) error {
	return nil
}
func (n *failFailureOnceNotifier) NotifyReviewFailure(context.Context, model.ReviewFailureReport) error {
	n.calls++
	if n.calls == 1 {
		return errors.New("intentional failure notification outage")
	}
	return nil
}

type recordingNotifier struct {
	failures []model.ReviewFailureReport
}

func (*recordingNotifier) NotificationCount(model.Report) int { return 0 }
func (*recordingNotifier) Notify(context.Context, model.Report, int) error {
	return nil
}
func (n *recordingNotifier) NotifyReviewFailure(_ context.Context, report model.ReviewFailureReport) error {
	n.failures = append(n.failures, report)
	return nil
}

func appDummyAgent() {
	if repository := os.Getenv("DUMMY_ADVANCE_REPO"); repository != "" {
		if err := os.WriteFile(filepath.Join(repository, "message.txt"), []byte("advanced while agent started\n"), 0o600); err != nil {
			os.Exit(18)
		}
		commands := [][]string{
			{"-C", repository, "add", "message.txt"},
			{"-C", repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "advance during review"},
			{"-C", repository, "push", "origin", "main"},
		}
		for _, arguments := range commands {
			if err := exec.Command("git", arguments...).Run(); err != nil {
				os.Exit(19)
			}
		}
	}
	if err := exec.Command("git", "fetch", "--no-tags", "origin", "main").Run(); err != nil {
		os.Exit(20)
	}
	if ready := os.Getenv("DUMMY_READY_FILE"); ready != "" {
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(21)
		}
		time.Sleep(30 * time.Second)
	}
	verdict := os.Getenv("DUMMY_VERDICT")
	if verdict == "skip" {
		submit := exec.Command(os.Getenv("CANCANNEED_SUBMIT_SCRIPT"), "skip", "--reason", "all commits opted out")
		if err := submit.Run(); err != nil {
			os.Exit(22)
		}
	}
	os.Exit(0)
}

func TestSubmissionCLIHelper(t *testing.T) {
	separator := -1
	for i, argument := range os.Args {
		if argument == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	if err := submission.Run(os.Args[separator+1:]); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	cmd := exec.Command("git", arguments...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
