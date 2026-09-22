// Command dummy-agent is a deterministic test double for local integration tests.
// It is not used by the production binary.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	failCount, err := strconv.Atoi(envOr("DUMMY_FAIL_COUNT", "0"))
	if err != nil || failCount < 0 {
		return fmt.Errorf("DUMMY_FAIL_COUNT must be a non-negative integer")
	}
	if failCount > 0 {
		counterPath := os.Getenv("DUMMY_COUNTER_FILE")
		if counterPath == "" {
			return fmt.Errorf("DUMMY_COUNTER_FILE is required when DUMMY_FAIL_COUNT is set")
		}
		attempt := readCounter(counterPath) + 1
		if err := os.WriteFile(counterPath, []byte(strconv.Itoa(attempt)), 0o600); err != nil {
			return fmt.Errorf("write attempt counter: %w", err)
		}
		if attempt <= failCount {
			return fmt.Errorf("intentional failure %d/%d", attempt, failCount)
		}
	}

	if err := runGit("fetch", "--no-tags", envOr("CANCANNEED_REMOTE", "origin"), envOr("CANCANNEED_BRANCH", "main")); err != nil {
		return fmt.Errorf("fetch remote: %w", err)
	}
	if envOr("DUMMY_VERDICT", "approve") == "skip" {
		submit := os.Getenv("CANCANNEED_SUBMIT_SCRIPT")
		if submit == "" {
			return fmt.Errorf("CANCANNEED_SUBMIT_SCRIPT is not set")
		}
		cmd := exec.Command(submit, "skip", "--reason", "dummy skipped all commits")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("submit skipped review: %w", err)
		}
	}
	return nil
}

func runGit(arguments ...string) error {
	cmd := exec.Command("git", arguments...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func readCounter(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return value
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
