package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type diskState struct {
	Version      int                        `json:"version"`
	Repositories map[string]RepositoryState `json:"repositories"`
}

// Store serializes every update so concurrent repository workers cannot lose state.
type Store struct {
	mu       sync.Mutex
	path     string
	data     diskState
	fileLock *fileLock
	closed   bool
}

func Open(path string) (*Store, error) {
	lock, err := acquireFileLock(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock state: %w", err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = lock.close()
		}
	}()

	s := &Store{
		path:     path,
		data:     diskState{Version: 1, Repositories: make(map[string]RepositoryState)},
		fileLock: lock,
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		keepLock = true
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.data.Version != 1 {
		return nil, fmt.Errorf("unsupported state version %d", s.data.Version)
	}
	if s.data.Repositories == nil {
		s.data.Repositories = make(map[string]RepositoryState)
	}
	keepLock = true
	return s, nil
}

// Close releases the process-wide ownership of the state file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.fileLock.close()
}

func (s *Store) Get(name string) (RepositoryState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.data.Repositories[name]
	return cloneRepositoryState(value), ok
}

func (s *Store) Put(name string, value RepositoryState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	previous, existed := s.data.Repositories[name]
	s.data.Repositories[name] = cloneRepositoryState(value)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.data.Repositories[name] = previous
		} else {
			delete(s.data.Repositories, name)
		}
		return err
	}
	return nil
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

func (s *Store) saveLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
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
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	removeTemp = false
	return nil
}
