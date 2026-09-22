package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewBaseUsesFirstParentOrEmptyTree(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "message.txt"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "message.txt")
	runTestGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	root := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))
	repository := Repository{Path: dir}

	base, err := repository.ReviewBase(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if base == root || strings.TrimSpace(runTestGit(t, dir, "cat-file", "-t", base)) != "tree" {
		t.Fatalf("root review base = %q, want an empty tree", base)
	}

	if err := os.WriteFile(filepath.Join(dir, "message.txt"), []byte("updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "message.txt")
	runTestGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "update")
	head := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))
	base, err = repository.ReviewBase(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if base != root {
		t.Fatalf("review base = %s, want first parent %s", base, root)
	}

	runTestGit(t, dir, "fetch", ".", "main")
	reviewed, err := repository.ReviewHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reviewed != head {
		t.Fatalf("review head = %s, want fetched head %s", reviewed, head)
	}
	info, err := repository.CommitInfo(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if info.Commit != head || info.Author != "Test" || info.AuthorEmail != "test@example.com" || info.Subject != "update" || info.CommittedAt.IsZero() {
		t.Fatalf("commit info = %#v", info)
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
