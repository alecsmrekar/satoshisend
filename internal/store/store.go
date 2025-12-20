package store

import (
	"context"
	"time"
)

// FileMeta contains metadata about an uploaded file.
type FileMeta struct {
	ID           string
	Size         int64
	ExpiresAt    time.Time
	HostDuration time.Duration // Intended hosting duration after payment
	Paid         bool
	CreatedAt    time.Time
}

// Stats contains aggregate statistics about stored files.
type Stats struct {
	TotalFiles   int
	PaidFiles    int
	PendingFiles int
	ExpiredFiles int
	TotalBytes   int64
	PaidBytes    int64
	PendingBytes int64
	OldestFile   time.Time
	NewestFile   time.Time
}

// Store defines the interface for metadata persistence.
type Store interface {
	SaveFileMetadata(ctx context.Context, meta *FileMeta) error
	GetFileMetadata(ctx context.Context, id string) (*FileMeta, error)
	UpdatePaymentStatus(ctx context.Context, fileID string, paid bool) error
	DeleteFileMetadata(ctx context.Context, id string) error
	ListExpiredFiles(ctx context.Context) ([]*FileMeta, error)
	GetStats(ctx context.Context) (*Stats, error)
	Close() error
}
