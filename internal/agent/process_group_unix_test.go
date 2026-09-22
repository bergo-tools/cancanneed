//go:build darwin || linux

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"cancanneed/internal/config"
)

func TestRunnerTimeoutKillsDescendantProcesses(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	retries := 0
	_, err := (Runner{}).Execute(context.Background(), Request{
		Agent: config.Agent{
			Type:    "pi",
			Command: "/bin/sh",
			Args:    []string{"-c", `sleep 30 & echo $! > "$CHILD_PID_FILE"; wait`, "{prompt}"},
			Env: map[string]string{
				"CHILD_PID_FILE": pidPath,
			},
			Timeout:      config.Duration(500 * time.Millisecond),
			Retries:      &retries,
			RetryBackoff: config.Duration(time.Millisecond),
		},
		WorkingDir: dir,
		RunDir:     dir,
		Prompt:     "review",
		OutputPath: filepath.Join(dir, "review.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unexpected runner error: %v", err)
	}

	pids, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pids)))
	if err != nil {
		t.Fatalf("parse descendant pid: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d survived the agent timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
