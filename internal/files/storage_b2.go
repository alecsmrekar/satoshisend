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

// B2Storage implements Storage using Backblaze B2 via S3-compatible API.
type B2Storage struct {
	client    *minio.Client
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
		client:    client,
		bucket:    cfg.Bucket,
		prefix:    cfg.Prefix,
		publicURL: cfg.PublicURL,
	}, nil
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
	logging.B2.Printf("uploading file %s to bucket %s", key, s.bucket)

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

	logging.B2.Printf("uploaded %s successfully (%d bytes)", key, info.Size)
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
	logging.B2.Printf("loading file %s from bucket %s", key, s.bucket)

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		logging.B2.Printf("failed to get object %s: %v", key, err)
		return nil, err
	}

	// Check if object exists by attempting to stat it
	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			logging.B2.Printf("file %s not found", key)
			return nil, ErrNotFound
		}
		logging.B2.Printf("failed to stat object %s: %v", key, err)
		return nil, err
	}

	logging.B2.Printf("loaded %s successfully (%d bytes)", key, stat.Size)
	return obj, nil
}

func (s *B2Storage) Delete(ctx context.Context, id string) error {
	key := s.key(id)
	logging.B2.Printf("deleting file %s from bucket %s", key, s.bucket)

	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			logging.B2.Printf("file %s not found for deletion", key)
			return ErrNotFound
		}
		logging.B2.Printf("failed to delete %s: %v", key, err)
		return err
	}

	logging.B2.Printf("deleted %s successfully", key)
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
