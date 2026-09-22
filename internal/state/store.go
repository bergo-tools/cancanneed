package state

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"cancanneed/internal/model"
)

type RepositoryState struct {
	HeadSHA                     string                      `json:"head_sha"`
	Branch                      string                      `json:"branch"`
	UpdatedAt                   time.Time                   `json:"updated_at"`
	PendingNotifications        []model.PendingNotification `json:"pending_notifications,omitempty"`
	PendingFailureNotifications []model.ReviewFailureReport `json:"pending_failure_notifications,omitempty"`
	ReviewFailure               *ReviewFailureState         `json:"review_failure,omitempty"`
}

// ReviewFailureState tracks consecutive failed review cycles for one observed HEAD.
type ReviewFailureState struct {
	HeadSHA      string    `json:"head_sha"`
	Count        int       `json:"count"`
	LastError    string    `json:"last_error"`
	LastFailedAt time.Time `json:"last_failed_at"`
}

type diskRepositoryState struct {
	Version    int             `json:"version"`
	Repository string          `json:"repository"`
	State      RepositoryState `json:"state"`
}

type repositoryFile struct {
	name     string
	path     string
	state    RepositoryState
	exists   bool
	fileLock *fileLock
}

// Store keeps one independently locked JSON file per configured repository.
type Store struct {
	mu           sync.Mutex
	directory    string
	repositories map[string]*repositoryFile
	paths        map[string]string
	closed       bool
}

func Open(directory string, repositoryNames ...string) (*Store, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	s := &Store{
		directory:    directory,
		repositories: make(map[string]*repositoryFile, len(repositoryNames)),
		paths:        make(map[string]string, len(repositoryNames)),
	}
	for _, name := range repositoryNames {
		if _, err := s.openLocked(name); err != nil {
			_ = s.closeLocked()
			return nil, err
		}
	}
	return s, nil
}

// Close releases every repository state lock held by this process.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *Store) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErrors []error
	for _, repository := range s.repositories {
		if err := repository.fileLock.close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close state for %s: %w", repository.name, err))
		}
	}
	return errors.Join(closeErrors...)
}

func (s *Store) Get(name string) (RepositoryState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repository, ok := s.repositories[name]
	if !ok || !repository.exists {
		return RepositoryState{}, false
	}
	return cloneRepositoryState(repository.state), true
}

func (s *Store) Put(name string, value RepositoryState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	repository, err := s.openLocked(name)
	if err != nil {
		return err
	}
	value = cloneRepositoryState(value)
	if err := saveRepository(repository.path, name, value); err != nil {
		return err
	}
	repository.state = value
	repository.exists = true
	return nil
}

func (s *Store) openLocked(name string) (*repositoryFile, error) {
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("repository name is required for state")
	}
	if repository, exists := s.repositories[name]; exists {
		return repository, nil
	}
	path := filepath.Join(s.directory, RepositoryFileName(name))
	if owner, exists := s.paths[path]; exists && owner != name {
		return nil, fmt.Errorf("repository names %q and %q resolve to the same state file", owner, name)
	}
	lock, err := acquireFileLock(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock state for repository %q: %w", name, err)
	}
	repository := &repositoryFile{name: name, path: path, fileLock: lock}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.repositories[name] = repository
		s.paths[path] = name
		return repository, nil
	}
	if err != nil {
		_ = lock.close()
		return nil, fmt.Errorf("read state for repository %q: %w", name, err)
	}
	var disk diskRepositoryState
	if err := json.Unmarshal(b, &disk); err != nil {
		_ = lock.close()
		return nil, fmt.Errorf("decode state for repository %q: %w", name, err)
	}
	if disk.Version != 1 {
		_ = lock.close()
		return nil, fmt.Errorf("unsupported state version %d for repository %q", disk.Version, name)
	}
	if disk.Repository != name {
		_ = lock.close()
		return nil, fmt.Errorf("state file %s belongs to repository %q, not %q", path, disk.Repository, name)
	}
	repository.state = cloneRepositoryState(disk.State)
	repository.exists = true
	s.repositories[name] = repository
	s.paths[path] = name
	return repository, nil
}

var unsafeFileName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// RepositoryFileName returns the stable JSON filename used for a repository.
func RepositoryFileName(name string) string {
	base := strings.Trim(unsafeFileName.ReplaceAllString(name, "-"), "-.")
	changed := base != name
	if base == "" {
		base = "repository"
		changed = true
	}
	if len(base) > 80 {
		base = strings.TrimRight(base[:80], "-.")
		changed = true
	}
	if changed {
		hash := sha256.Sum256([]byte(name))
		base += fmt.Sprintf("-%x", hash[:8])
	}
	return base + ".json"
}

func cloneRepositoryState(value RepositoryState) RepositoryState {
	if value.PendingNotifications != nil {
		value.PendingNotifications = append([]model.PendingNotification(nil), value.PendingNotifications...)
		for i := range value.PendingNotifications {
			value.PendingNotifications[i] = clonePendingNotification(value.PendingNotifications[i])
		}
	}
	value.PendingFailureNotifications = append([]model.ReviewFailureReport(nil), value.PendingFailureNotifications...)
	if value.ReviewFailure != nil {
		failure := *value.ReviewFailure
		value.ReviewFailure = &failure
	}
	return value
}

func clonePendingNotification(value model.PendingNotification) model.PendingNotification {
	value.Report.Findings = append([]model.Finding(nil), value.Report.Findings...)
	return value
}

func saveRepository(path, name string, state RepositoryState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	disk := diskRepositoryState{Version: 1, Repository: name, State: state}
	b, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set state permissions: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	removeTemp = false
	return nil
}
