package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"satoshisend/internal/store"
)

// PendingTimeout is how long an unpaid file remains before cleanup.
const PendingTimeout = 1 * time.Hour

// Service handles file operations.
type Service struct {
	storage Storage
	store   store.Store
}

// NewService creates a new file service.
func NewService(storage Storage, st store.Store) *Service {
	return &Service{
		storage: storage,
		store:   st,
	}
}

// UploadResult contains the result of an upload operation.
type UploadResult struct {
	ID   string
	Size int64
}

// Upload stores an encrypted file and creates metadata.
// The file is stored in a pending state until payment is confirmed.
// Unpaid files expire after PendingTimeout; once paid, expiration extends to hostDuration.
func (s *Service) Upload(ctx context.Context, data io.Reader, hostDuration time.Duration) (*UploadResult, error) {
	return s.UploadWithProgress(ctx, data, -1, hostDuration, nil)
}

// UploadWithProgress stores an encrypted file with progress reporting.
// The size parameter should be the known size of the data, or -1 if unknown.
// The onProgress callback is called with (bytesWritten, totalBytes) during upload.
func (s *Service) UploadWithProgress(ctx context.Context, data io.Reader, size int64, hostDuration time.Duration, onProgress ProgressFunc) (*UploadResult, error) {
	id, err := generateID()
	if err != nil {
		return nil, err
	}

	actualSize, err := s.storage.SaveWithProgress(ctx, id, data, size, onProgress)
	if err != nil {
		return nil, err
	}

	meta := &store.FileMeta{
		ID:           id,
		Size:         actualSize,
		ExpiresAt:    time.Now().Add(PendingTimeout), // Short expiration until paid
		HostDuration: hostDuration,                   // Full duration applied after payment
		Paid:         false,
		CreatedAt:    time.Now(),
	}

	if err := s.store.SaveFileMetadata(ctx, meta); err != nil {
		// Clean up the stored file if metadata save fails
		s.storage.Delete(ctx, id)
		return nil, err
	}

	return &UploadResult{ID: id, Size: actualSize}, nil
}

// Download retrieves a file if it exists and is paid for.
func (s *Service) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	meta, err := s.store.GetFileMetadata(ctx, id)
	if err != nil {
		return nil, err
	}

	if !meta.Paid {
		return nil, ErrNotPaid
	}

	if time.Now().After(meta.ExpiresAt) {
		return nil, ErrExpired
	}

	return s.storage.Load(ctx, id)
}

// ReadSeekCloser combines ReadSeeker and Closer interfaces.
type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

// DownloadSeekable retrieves a file for serving with Range request support.
// Returns a ReadSeekCloser if the underlying storage supports it.
func (s *Service) DownloadSeekable(ctx context.Context, id string) (ReadSeekCloser, error) {
	meta, err := s.store.GetFileMetadata(ctx, id)
	if err != nil {
		return nil, err
	}

	if !meta.Paid {
		return nil, ErrNotPaid
	}

	if time.Now().After(meta.ExpiresAt) {
		return nil, ErrExpired
	}

	reader, err := s.storage.Load(ctx, id)
	if err != nil {
		return nil, err
	}

	// Check if the reader supports seeking (e.g., *os.File)
	if rsc, ok := reader.(ReadSeekCloser); ok {
		return rsc, nil
	}

	// Fallback: wrap in a non-seekable wrapper (Range requests won't work)
	return &nonSeekableWrapper{reader}, nil
}

// nonSeekableWrapper wraps a ReadCloser to satisfy ReadSeekCloser interface
// but returns an error on Seek operations.
type nonSeekableWrapper struct {
	io.ReadCloser
}

func (w *nonSeekableWrapper) Seek(offset int64, whence int) (int64, error) {
	return 0, errors.New("seek not supported")
}

// GetMetadata retrieves file metadata.
func (s *Service) GetMetadata(ctx context.Context, id string) (*store.FileMeta, error) {
	return s.store.GetFileMetadata(ctx, id)
}

// GetDirectURL returns the direct download URL for a file if the storage backend
// supports public access. Returns empty string if not available.
func (s *Service) GetDirectURL(id string) string {
	if provider, ok := s.storage.(PublicURLProvider); ok {
		return provider.GetPublicURL(id)
	}
	return ""
}

// MarkPaid marks a file as paid and extends its expiration to the full host duration.
func (s *Service) MarkPaid(ctx context.Context, id string) error {
	return s.store.UpdatePaymentStatus(ctx, id, true)
}

// CleanupExpired removes expired files from storage and database.
func (s *Service) CleanupExpired(ctx context.Context) (int, error) {
	expired, err := s.store.ListExpiredFiles(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, meta := range expired {
		if err := s.storage.Delete(ctx, meta.ID); err != nil && err != ErrNotFound {
			continue
		}
		if err := s.store.DeleteFileMetadata(ctx, meta.ID); err != nil {
			continue
		}
		count++
	}
	return count, nil
}

func generateID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

var (
	ErrNotPaid = errors.New("file not paid for")
	ErrExpired = errors.New("file has expired")
)
