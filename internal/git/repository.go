package git

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"cancanneed/internal/model"
)

type Repository struct {
	Path   string
	Remote string
}

func (r Repository) Validate(ctx context.Context) error {
	out, err := r.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("validate repository: %w", err)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%s is not a git work tree", r.Path)
	}
	return nil
}

// ResolveBranch uses the configured branch, then the remote HEAD, then main/master.
func (r Repository) ResolveBranch(ctx context.Context, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	out, err := r.run(ctx, "ls-remote", "--symref", r.Remote, "HEAD")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
				if branch, ok := strings.CutPrefix(fields[1], "refs/heads/"); ok && branch != "" {
					return branch, nil
				}
			}
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, candidateErr := r.RemoteHead(ctx, candidate); candidateErr == nil {
			return candidate, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve default branch from remote HEAD: %w", err)
	}
	return "", fmt.Errorf("remote %q has no HEAD, main, or master branch", r.Remote)
}

func (r Repository) RemoteHead(ctx context.Context, branch string) (string, error) {
	ref := "refs/heads/" + branch
	out, err := r.run(ctx, "ls-remote", "--exit-code", r.Remote, ref)
	if err != nil {
		return "", fmt.Errorf("read %s/%s: %w", r.Remote, branch, err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[1] != ref || !validObjectID(fields[0]) {
		return "", fmt.Errorf("unexpected ls-remote output for %s: %q", ref, out)
	}
	return fields[0], nil
}

// ReviewHead returns the commit left in FETCH_HEAD by the agent's final fetch.
// It deliberately does not contact the remote: this is the exact local snapshot
// the completed review was based on, even if the remote moves immediately after.
func (r Repository) ReviewHead(ctx context.Context) (string, error) {
	head, err := r.run(ctx, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve reviewed FETCH_HEAD: %w", err)
	}
	if !validObjectID(head) {
		return "", fmt.Errorf("unexpected reviewed FETCH_HEAD: %q", head)
	}
	return head, nil
}

// CommitInfo reads display metadata for one commit from the local object store.
func (r Repository) CommitInfo(ctx context.Context, commit string) (model.CommitInfo, error) {
	if !validObjectID(commit) {
		return model.CommitInfo{}, fmt.Errorf("invalid commit object ID %q", commit)
	}
	out, err := r.run(ctx, "show", "-s", "--format=%H%x00%an%x00%ae%x00%cI%x00%s", commit, "--")
	if err != nil {
		return model.CommitInfo{}, fmt.Errorf("read commit info for %s: %w", commit, err)
	}
	fields := strings.SplitN(out, "\x00", 5)
	if len(fields) != 5 || !strings.EqualFold(fields[0], commit) {
		return model.CommitInfo{}, fmt.Errorf("unexpected commit info for %s", commit)
	}
	committedAt, err := time.Parse(time.RFC3339, fields[3])
	if err != nil {
		return model.CommitInfo{}, fmt.Errorf("parse commit time for %s: %w", commit, err)
	}
	return model.CommitInfo{
		Commit:      strings.ToLower(fields[0]),
		Author:      fields[1],
		AuthorEmail: fields[2],
		CommittedAt: committedAt,
		Subject:     fields[4],
	}, nil
}

// PinRemoteHead fetches the initial baseline without touching the index or work tree.
// Keeping the object reachable makes a later force-push review possible.
func (r Repository) PinRemoteHead(ctx context.Context, branch, expected string) error {
	ref := "refs/cancanneed/baselines/" + expected
	refspec := "+refs/heads/" + branch + ":" + ref
	if _, err := r.run(ctx, "fetch", "--no-tags", "--", r.Remote, refspec); err != nil {
		return fmt.Errorf("fetch baseline %s/%s: %w", r.Remote, branch, err)
	}
	actual, err := r.run(ctx, "rev-parse", ref+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve fetched baseline: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("remote head changed while recording baseline: expected %s, got %s", expected, actual)
	}
	return nil
}

// ReviewBase returns the first parent of head. For a root commit it creates and
// returns the repository's empty tree so the root commit can still be diffed.
func (r Repository) ReviewBase(ctx context.Context, head string) (string, error) {
	out, err := r.run(ctx, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return "", fmt.Errorf("resolve review base for %s: %w", head, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != head {
		return "", fmt.Errorf("unexpected rev-list output for %s: %q", head, out)
	}
	if len(fields) > 1 {
		return fields[1], nil
	}
	emptyTree, err := r.runWithInput(ctx, nil, "hash-object", "-t", "tree", "-w", "--stdin")
	if err != nil {
		return "", fmt.Errorf("create empty tree review base: %w", err)
	}
	if !validObjectID(emptyTree) {
		return "", fmt.Errorf("unexpected empty tree object ID: %q", emptyTree)
	}
	return emptyTree, nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (r Repository) run(ctx context.Context, args ...string) (string, error) {
	return r.runWithInput(ctx, nil, args...)
}

func (r Repository) runWithInput(ctx context.Context, input []byte, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", r.Path}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return strings.TrimSpace(stdout.String()), nil
}
