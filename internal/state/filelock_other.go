//go:build !darwin && !linux

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type fileLock struct {
	file *os.File
	path string
}

func acquireFileLock(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, errors.New("state is already in use by another cancanneed process")
	}
	if err != nil {
		return nil, err
	}
	return &fileLock{file: file, path: path}, nil
}

func (l *fileLock) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	closeErr := l.file.Close()
	removeErr := os.Remove(l.path)
	l.file = nil
	return errors.Join(closeErr, removeErr)
}
