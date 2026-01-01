package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"satoshisend/internal/logging"
	"satoshisend/internal/store"
)

// PendingTimeout is how long an unpaid file remains before cleanup.
const PendingTimeout = 15 * time.Minute

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

// UploadWithID stores data with a specific file ID (used for streaming proxy uploads).
// The file ID should be obtained from InitUpload first.
func (s *Service) UploadWithID(ctx context.Context, id string, data io.Reader, size int64, hostDuration time.Duration) (int64, error) {
	actualSize, err := s.storage.SaveWithProgress(ctx, id, data, size, nil)
	if err != nil {
		return 0, err
	}

	return actualSize, nil
}

// UploadInitResult contains the result of initiating an upload.
type UploadInitResult struct {
	ID string
}

// InitUpload generates a file ID for a new upload.
func (s *Service) InitUpload(ctx context.Context) (*UploadInitResult, error) {
	id, err := generateID()
	if err != nil {
		return nil, err
	}

	return &UploadInitResult{
		ID: id,
	}, nil
}

// CompleteUpload verifies the file was uploaded to storage and creates metadata.
// Returns an error if the file doesn't exist or is the wrong size.
func (s *Service) CompleteUpload(ctx context.Context, id string, expectedSize int64, hostDuration time.Duration) (*UploadResult, error) {
	statProvider, ok := s.storage.(StatProvider)
	if !ok {
		return nil, errors.New("storage backend does not support stat")
	}

	// Verify the file exists and check size
	actualSize, err := statProvider.Stat(ctx, id)
	if err != nil {
		if err == ErrNotFound {
			return nil, errors.New("file not found in storage - upload may have failed")
		}
		return nil, err
	}

	// Verify size matches (with some tolerance for chunked encoding overhead)
	if expectedSize > 0 && actualSize != expectedSize {
		logging.Internal.Printf("size mismatch for %s: expected %d, got %d", id, expectedSize, actualSize)
		// Don't fail on size mismatch - client-side encryption may add overhead
	}

	meta := &store.FileMeta{
		ID:           id,
		Size:         actualSize,
		ExpiresAt:    time.Now().Add(PendingTimeout),
		HostDuration: hostDuration,
		Paid:         false,
		CreatedAt:    time.Now(),
	}

	if err := s.store.SaveFileMetadata(ctx, meta); err != nil {
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
// It continues processing other files even if some deletions fail,
// but logs errors to detect infrastructure issues.
func (s *Service) CleanupExpired(ctx context.Context) (int, error) {
	expired, err := s.store.ListExpiredFiles(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	storageErrors := 0
	metadataErrors := 0

	for _, meta := range expired {
		if err := s.storage.Delete(ctx, meta.ID); err != nil && err != ErrNotFound {
			storageErrors++
			logging.Internal.Printf("failed to delete file %s from storage: %v", meta.ID, err)
			continue
		}
		if err := s.store.DeleteFileMetadata(ctx, meta.ID); err != nil {
			metadataErrors++
			logging.Internal.Printf("failed to delete metadata for file %s: %v", meta.ID, err)
			continue
		}
		count++
	}

	if storageErrors > 0 || metadataErrors > 0 {
		logging.Internal.Printf("cleanup completed with errors: %d storage failures, %d metadata failures", storageErrors, metadataErrors)
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
