package submission

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cancanneed/internal/model"
)

const (
	lockWait       = 30 * time.Second
	staleLockAfter = 2 * time.Minute
	maxResultBytes = 2 << 20
)

// Run implements the hidden process entrypoint used by submit-review.sh.
func Run(arguments []string) error {
	global := flag.NewFlagSet("__submit", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	output := global.String("output", "", "review output path")
	repository := global.String("repository", "", "reviewed Git repository path")
	if err := global.Parse(arguments); err != nil {
		return err
	}
	remaining := global.Args()
	if *output == "" {
		return errors.New("--output is required")
	}
	if len(remaining) == 0 {
		return errors.New("expected finding or skip subcommand")
	}

	switch remaining[0] {
	case "finding":
		finding, err := parseFinding(remaining[1:])
		if err != nil {
			return err
		}
		if strings.TrimSpace(*repository) == "" {
			return errors.New("--repository is required for finding submissions")
		}
		if err := validateCommit(*repository, finding.Commit); err != nil {
			return err
		}
		return update(*output, func(result *model.AgentResult) error {
			if result.Skipped {
				return errors.New("review is already marked as skipped")
			}
			result.Findings = append(result.Findings, finding)
			return nil
		})
	case "skip":
		reason, err := parseSkip(remaining[1:])
		if err != nil {
			return err
		}
		return update(*output, func(result *model.AgentResult) error {
			if result.Skipped {
				return errors.New("review is already marked as skipped")
			}
			if len(result.Findings) != 0 {
				return errors.New("skipped review cannot contain findings")
			}
			result.Skipped = true
			result.SkipReason = reason
			return nil
		})
	default:
		return fmt.Errorf("unknown submission subcommand %q", remaining[0])
	}
}

func validateCommit(repository, commit string) error {
	command := exec.Command("git", "-C", repository, "cat-file", "-e", commit+"^{commit}")
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("--commit %s does not identify an existing commit in repository: %s", commit, detail)
}

func parseFinding(arguments []string) (model.Finding, error) {
	flags := flag.NewFlagSet("finding", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var finding model.Finding
	flags.StringVar(&finding.Author, "author", "", "commit author")
	flags.StringVar(&finding.Commit, "commit", "", "full commit SHA")
	flags.StringVar(&finding.File, "file", "", "repository-relative file")
	flags.IntVar(&finding.Line, "line", 0, "line number in the new file")
	flags.StringVar(&finding.Severity, "severity", "", "finding severity")
	flags.StringVar(&finding.Title, "title", "", "finding title")
	flags.StringVar(&finding.Detail, "detail", "", "finding detail")
	if err := flags.Parse(arguments); err != nil {
		return model.Finding{}, err
	}
	if flags.NArg() != 0 {
		return model.Finding{}, fmt.Errorf("unexpected finding arguments: %v", flags.Args())
	}
	if err := validateFinding(finding); err != nil {
		return model.Finding{}, err
	}
	return finding, nil
}

func parseSkip(arguments []string) (string, error) {
	flags := flag.NewFlagSet("skip", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	reason := flags.String("reason", "", "why all commits were skipped")
	if err := flags.Parse(arguments); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("unexpected skip arguments: %v", flags.Args())
	}
	if strings.TrimSpace(*reason) == "" {
		return "", errors.New("--reason is required")
	}
	return *reason, nil
}

func update(path string, mutate func(*model.AgentResult) error) error {
	return withLock(path, func() error {
		result, err := read(path)
		if err != nil {
			return err
		}
		if err := mutate(&result); err != nil {
			return err
		}
		return writeAtomic(path, result)
	})
}

func read(path string) (model.AgentResult, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return model.AgentResult{Findings: []model.Finding{}}, nil
	}
	if err != nil {
		return model.AgentResult{}, fmt.Errorf("read current review: %w", err)
	}
	if len(b) > maxResultBytes {
		return model.AgentResult{}, errors.New("current review is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	var result model.AgentResult
	if err := decoder.Decode(&result); err != nil {
		return model.AgentResult{}, fmt.Errorf("decode current review: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return model.AgentResult{}, err
	}
	if result.Findings == nil {
		result.Findings = []model.Finding{}
	}
	return result, nil
}

func writeAtomic(path string, result model.AgentResult) error {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode review: %w", err)
	}
	if len(b) > maxResultBytes {
		return errors.New("review result exceeds 2 MiB")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create review directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".review-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary review: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace review: %w", err)
	}
	keep = true
	return nil
}

func withLock(outputPath string, action func() error) error {
	lockPath := outputPath + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("acquire submission lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > staleLockAfter {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for concurrent submission lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer os.Remove(lockPath)
	return action()
}

func validateFinding(finding model.Finding) error {
	if strings.TrimSpace(finding.Author) == "" {
		return errors.New("--author is required")
	}
	if !validObjectID(finding.Commit) {
		return errors.New("--commit must be a full 40- or 64-character commit SHA")
	}
	if strings.TrimSpace(finding.File) == "" {
		return errors.New("--file is required")
	}
	cleanFile := filepath.Clean(finding.File)
	if filepath.IsAbs(finding.File) || cleanFile == ".." || strings.HasPrefix(cleanFile, ".."+string(filepath.Separator)) {
		return errors.New("--file must be repository-relative")
	}
	if finding.Line <= 0 {
		return errors.New("--line must be positive")
	}
	switch finding.Severity {
	case "critical", "high", "medium", "low", "info":
	default:
		return errors.New("--severity must be critical, high, medium, low, or info")
	}
	if strings.TrimSpace(finding.Title) == "" {
		return errors.New("--title is required")
	}
	if strings.TrimSpace(finding.Detail) == "" {
		return errors.New("--detail is required")
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing review data: %w", err)
	}
	return errors.New("review contains multiple JSON values")
}
