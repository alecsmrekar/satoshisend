package files

import (
	"context"
	"io"
)

// ProgressFunc is called during upload with bytes written and total size.
// If total is -1, the total size is unknown.
type ProgressFunc func(written, total int64)

// Storage defines the interface for blob storage.
type Storage interface {
	Save(ctx context.Context, id string, data io.Reader) (int64, error)
	SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress ProgressFunc) (int64, error)
	Load(ctx context.Context, id string) (io.ReadCloser, error)
	Delete(ctx context.Context, id string) error
}

// PublicURLProvider is an optional interface for storage backends that support
// direct public access to files (e.g., public B2 buckets).
type PublicURLProvider interface {
	// GetPublicURL returns the public URL for a file, or empty string if not available.
	GetPublicURL(id string) string
}
