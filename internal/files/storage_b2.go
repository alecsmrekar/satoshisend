package files

import (
	"context"
	"io"
	"path"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"satoshisend/internal/logging"
)

const b2Endpoint = "s3.us-east-005.backblazeb2.com"

// B2Object wraps the operations needed from an S3 object.
// This abstraction allows for mocking in tests.
type B2Object interface {
	io.ReadCloser
	Stat() (minio.ObjectInfo, error)
}

// B2Client defines the interface for S3-compatible storage operations.
// This abstraction allows for mocking in tests.
type B2Client interface {
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (B2Object, error)
	RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error
	StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
}

// minioClientWrapper wraps *minio.Client to implement B2Client.
type minioClientWrapper struct {
	client *minio.Client
}

func (w *minioClientWrapper) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return w.client.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

func (w *minioClientWrapper) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (B2Object, error) {
	return w.client.GetObject(ctx, bucketName, objectName, opts)
}

func (w *minioClientWrapper) RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error {
	return w.client.RemoveObject(ctx, bucketName, objectName, opts)
}

func (w *minioClientWrapper) StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return w.client.StatObject(ctx, bucketName, objectName, opts)
}

// B2Storage implements Storage using Backblaze B2 via S3-compatible API.
type B2Storage struct {
	client    B2Client
	bucket    string
	prefix    string
	publicURL string // Base URL for public access (e.g., "https://f005.backblazeb2.com/file/mybucket")
}

// B2Config holds configuration for B2 storage.
type B2Config struct {
	KeyID     string // B2_KEY_ID
	AppKey    string // B2_APP_KEY
	Bucket    string // B2_BUCKET
	Prefix    string // B2_PREFIX - optional folder prefix for all objects
	PublicURL string // B2_PUBLIC_URL - base URL for public access (enables direct downloads)
}

// NewB2Storage creates a new B2-backed storage.
func NewB2Storage(cfg B2Config) (*B2Storage, error) {
	logging.B2.Printf("initializing storage (bucket=%s, prefix=%s, endpoint=%s)", cfg.Bucket, cfg.Prefix, b2Endpoint)

	client, err := minio.New(b2Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.KeyID, cfg.AppKey, ""),
		Secure: true,
	})
	if err != nil {
		logging.B2.Printf("failed to create client: %v", err)
		return nil, err
	}

	if cfg.PublicURL != "" {
		logging.B2.Printf("public URL configured: %s", cfg.PublicURL)
	}

	logging.B2.Printf("storage initialized successfully")
	return &B2Storage{
		client:    &minioClientWrapper{client: client},
		bucket:    cfg.Bucket,
		prefix:    cfg.Prefix,
		publicURL: cfg.PublicURL,
	}, nil
}

// NewB2StorageWithClient creates a B2Storage with a custom client.
// This is primarily useful for testing with mock clients.
func NewB2StorageWithClient(client B2Client, bucket, prefix, publicURL string) *B2Storage {
	return &B2Storage{
		client:    client,
		bucket:    bucket,
		prefix:    prefix,
		publicURL: publicURL,
	}
}

func (s *B2Storage) key(id string) string {
	if s.prefix == "" {
		return id
	}
	return path.Join(s.prefix, id)
}

func (s *B2Storage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return s.SaveWithProgress(ctx, id, data, -1, nil)
}

func (s *B2Storage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress ProgressFunc) (int64, error) {
	key := s.key(id)

	// Wrap reader with progress tracking if callback provided
	var reader io.Reader = data
	if onProgress != nil {
		reader = &progressReader{
			reader:     data,
			total:      size,
			onProgress: onProgress,
		}
	}

	info, err := s.client.PutObject(ctx, s.bucket, key, reader, size, minio.PutObjectOptions{})
	if err != nil {
		logging.B2.Printf("upload failed for %s: %v", key, err)
		return 0, err
	}

	return info.Size, nil
}

// progressReader wraps an io.Reader and reports progress as data is read.
type progressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	onProgress ProgressFunc
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.read += int64(n)
		if pr.onProgress != nil {
			pr.onProgress(pr.read, pr.total)
		}
	}
	return n, err
}

func (s *B2Storage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	key := s.key(id)

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		logging.B2.Printf("failed to get object %s: %v", key, err)
		return nil, err
	}

	// Check if object exists by attempting to stat it
	_, err = obj.Stat()
	if err != nil {
		obj.Close()
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		logging.B2.Printf("failed to stat object %s: %v", key, err)
		return nil, err
	}

	return obj, nil
}

func (s *B2Storage) Delete(ctx context.Context, id string) error {
	key := s.key(id)

	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return ErrNotFound
		}
		logging.B2.Printf("failed to delete %s: %v", key, err)
		return err
	}

	return nil
}

// GetPublicURL returns the public URL for a file if public access is configured.
// Returns empty string if public URL is not configured.
func (s *B2Storage) GetPublicURL(id string) string {
	if s.publicURL == "" {
		return ""
	}
	key := s.key(id)
	// Ensure no double slashes
	if s.publicURL[len(s.publicURL)-1] == '/' {
		return s.publicURL + key
	}
	return s.publicURL + "/" + key
}

// Stat returns the size of a file in B2.
func (s *B2Storage) Stat(ctx context.Context, id string) (int64, error) {
	key := s.key(id)

	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			return 0, ErrNotFound
		}
		logging.B2.Printf("failed to stat %s: %v", key, err)
		return 0, err
	}

	return info.Size, nil
}
