package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var ErrNotFound = errors.New("file not found")
var ErrInvalidID = errors.New("invalid file id")

// validIDPattern matches only alphanumeric IDs (no path traversal possible)
var validIDPattern = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// FSStorage implements Storage using the local filesystem.
type FSStorage struct {
	basePath string
}

// NewFSStorage creates a new filesystem-based storage.
func NewFSStorage(basePath string) (*FSStorage, error) {
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, err
	}
	return &FSStorage{basePath: basePath}, nil
}

func (s *FSStorage) validateID(id string) error {
	if id == "" || len(id) > 64 || !validIDPattern.MatchString(id) {
		return ErrInvalidID
	}
	return nil
}

func (s *FSStorage) path(id string) string {
	return filepath.Join(s.basePath, id)
}

func (s *FSStorage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return s.SaveWithProgress(ctx, id, data, -1, nil)
}

func (s *FSStorage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress ProgressFunc) (int64, error) {
	if err := s.validateID(id); err != nil {
		return 0, err
	}
	f, err := os.Create(s.path(id))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Wrap reader with progress tracking if callback provided
	var reader io.Reader = data
	if onProgress != nil {
		reader = &progressReader{
			reader:     data,
			total:      size,
			onProgress: onProgress,
		}
	}

	return io.Copy(f, reader)
}

func (s *FSStorage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	if err := s.validateID(id); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (s *FSStorage) Delete(ctx context.Context, id string) error {
	if err := s.validateID(id); err != nil {
		return err
	}
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}
