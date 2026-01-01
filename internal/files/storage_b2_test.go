package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
)

// mockB2Object implements B2Object for testing.
type mockB2Object struct {
	data      []byte
	readIndex int
	statInfo  minio.ObjectInfo
	statErr   error
	closed    bool
}

func (m *mockB2Object) Read(p []byte) (int, error) {
	if m.readIndex >= len(m.data) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.readIndex:])
	m.readIndex += n
	return n, nil
}

func (m *mockB2Object) Close() error {
	m.closed = true
	return nil
}

func (m *mockB2Object) Stat() (minio.ObjectInfo, error) {
	return m.statInfo, m.statErr
}

// mockB2Client implements B2Client for testing.
type mockB2Client struct {
	putFunc    func(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	getFunc    func(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error)
	removeFunc func(ctx context.Context, bucket, key string, opts minio.RemoveObjectOptions) error
	statFunc   func(ctx context.Context, bucket, key string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)

	// Track calls for verification
	putCalls    []putCall
	getCalls    []getCall
	removeCalls []removeCall
}

type putCall struct {
	bucket string
	key    string
	size   int64
}

type getCall struct {
	bucket string
	key    string
}

type removeCall struct {
	bucket string
	key    string
}

func (m *mockB2Client) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	m.putCalls = append(m.putCalls, putCall{bucket: bucket, key: key, size: size})
	if m.putFunc != nil {
		return m.putFunc(ctx, bucket, key, reader, size, opts)
	}
	// Default: read all data and return size
	data, _ := io.ReadAll(reader)
	return minio.UploadInfo{Size: int64(len(data))}, nil
}

func (m *mockB2Client) GetObject(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error) {
	m.getCalls = append(m.getCalls, getCall{bucket: bucket, key: key})
	if m.getFunc != nil {
		return m.getFunc(ctx, bucket, key, opts)
	}
	return nil, errors.New("not implemented")
}

func (m *mockB2Client) RemoveObject(ctx context.Context, bucket, key string, opts minio.RemoveObjectOptions) error {
	m.removeCalls = append(m.removeCalls, removeCall{bucket: bucket, key: key})
	if m.removeFunc != nil {
		return m.removeFunc(ctx, bucket, key, opts)
	}
	return nil
}

func (m *mockB2Client) StatObject(ctx context.Context, bucket, key string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) {
	if m.statFunc != nil {
		return m.statFunc(ctx, bucket, key, opts)
	}
	return minio.ObjectInfo{}, errors.New("not implemented")
}

func TestB2Storage_Key(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		id     string
		want   string
	}{
		{"no prefix", "", "abc123", "abc123"},
		{"with prefix", "uploads", "abc123", "uploads/abc123"},
		{"prefix with trailing slash normalizes", "uploads/", "abc123", "uploads/abc123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage := NewB2StorageWithClient(nil, "bucket", tc.prefix, "")
			got := storage.key(tc.id)
			if got != tc.want {
				t.Errorf("key(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestB2Storage_GetPublicURL(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		prefix    string
		id        string
		want      string
	}{
		{"no public URL configured", "", "", "abc123", ""},
		{"public URL without trailing slash", "https://cdn.example.com/bucket", "", "abc123", "https://cdn.example.com/bucket/abc123"},
		{"public URL with trailing slash", "https://cdn.example.com/bucket/", "", "abc123", "https://cdn.example.com/bucket/abc123"},
		{"with prefix", "https://cdn.example.com/bucket", "uploads", "abc123", "https://cdn.example.com/bucket/uploads/abc123"},
		{"with prefix and trailing slash", "https://cdn.example.com/bucket/", "uploads", "abc123", "https://cdn.example.com/bucket/uploads/abc123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage := NewB2StorageWithClient(nil, "bucket", tc.prefix, tc.publicURL)
			got := storage.GetPublicURL(tc.id)
			if got != tc.want {
				t.Errorf("GetPublicURL(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestB2Storage_Save(t *testing.T) {
	ctx := context.Background()
	testData := []byte("hello, world!")

	t.Run("successful save", func(t *testing.T) {
		mock := &mockB2Client{}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		n, err := storage.Save(ctx, "testfile", bytes.NewReader(testData))
		if err != nil {
			t.Fatalf("Save failed: %v", err)
		}
		if n != int64(len(testData)) {
			t.Errorf("Save returned %d bytes, want %d", n, len(testData))
		}

		// Verify client was called correctly
		if len(mock.putCalls) != 1 {
			t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
		}
		if mock.putCalls[0].bucket != "test-bucket" {
			t.Errorf("bucket = %q, want %q", mock.putCalls[0].bucket, "test-bucket")
		}
		if mock.putCalls[0].key != "testfile" {
			t.Errorf("key = %q, want %q", mock.putCalls[0].key, "testfile")
		}
	})

	t.Run("save with prefix", func(t *testing.T) {
		mock := &mockB2Client{}
		storage := NewB2StorageWithClient(mock, "test-bucket", "uploads", "")

		_, err := storage.Save(ctx, "testfile", bytes.NewReader(testData))
		if err != nil {
			t.Fatalf("Save failed: %v", err)
		}

		if mock.putCalls[0].key != "uploads/testfile" {
			t.Errorf("key = %q, want %q", mock.putCalls[0].key, "uploads/testfile")
		}
	})

	t.Run("save error", func(t *testing.T) {
		expectedErr := errors.New("upload failed")
		mock := &mockB2Client{
			putFunc: func(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
				return minio.UploadInfo{}, expectedErr
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		_, err := storage.Save(ctx, "testfile", bytes.NewReader(testData))
		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})
}

func TestB2Storage_SaveWithProgress(t *testing.T) {
	ctx := context.Background()
	testData := []byte("hello, world!")

	t.Run("progress callback is invoked", func(t *testing.T) {
		mock := &mockB2Client{}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		var progressCalls int
		var lastWritten, lastTotal int64

		onProgress := func(written, total int64) {
			progressCalls++
			lastWritten = written
			lastTotal = total
		}

		n, err := storage.SaveWithProgress(ctx, "testfile", bytes.NewReader(testData), int64(len(testData)), onProgress)
		if err != nil {
			t.Fatalf("SaveWithProgress failed: %v", err)
		}
		if n != int64(len(testData)) {
			t.Errorf("SaveWithProgress returned %d bytes, want %d", n, len(testData))
		}

		if progressCalls == 0 {
			t.Error("expected progress callback to be called")
		}
		if lastWritten != int64(len(testData)) {
			t.Errorf("last written = %d, want %d", lastWritten, len(testData))
		}
		if lastTotal != int64(len(testData)) {
			t.Errorf("last total = %d, want %d", lastTotal, len(testData))
		}
	})

	t.Run("nil progress callback", func(t *testing.T) {
		mock := &mockB2Client{}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		n, err := storage.SaveWithProgress(ctx, "testfile", bytes.NewReader(testData), int64(len(testData)), nil)
		if err != nil {
			t.Fatalf("SaveWithProgress failed: %v", err)
		}
		if n != int64(len(testData)) {
			t.Errorf("SaveWithProgress returned %d bytes, want %d", n, len(testData))
		}
	})
}

func TestB2Storage_Load(t *testing.T) {
	ctx := context.Background()
	testData := []byte("hello, world!")

	t.Run("successful load", func(t *testing.T) {
		mockObj := &mockB2Object{
			data:     testData,
			statInfo: minio.ObjectInfo{Size: int64(len(testData))},
		}
		mock := &mockB2Client{
			getFunc: func(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error) {
				return mockObj, nil
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		reader, err := storage.Load(ctx, "testfile")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		defer reader.Close()

		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if !bytes.Equal(data, testData) {
			t.Errorf("loaded data = %q, want %q", data, testData)
		}

		// Verify correct bucket and key
		if len(mock.getCalls) != 1 {
			t.Fatalf("expected 1 get call, got %d", len(mock.getCalls))
		}
		if mock.getCalls[0].bucket != "test-bucket" {
			t.Errorf("bucket = %q, want %q", mock.getCalls[0].bucket, "test-bucket")
		}
		if mock.getCalls[0].key != "testfile" {
			t.Errorf("key = %q, want %q", mock.getCalls[0].key, "testfile")
		}
	})

	t.Run("load with prefix", func(t *testing.T) {
		mockObj := &mockB2Object{
			data:     testData,
			statInfo: minio.ObjectInfo{Size: int64(len(testData))},
		}
		mock := &mockB2Client{
			getFunc: func(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error) {
				return mockObj, nil
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "uploads", "")

		_, err := storage.Load(ctx, "testfile")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}

		if mock.getCalls[0].key != "uploads/testfile" {
			t.Errorf("key = %q, want %q", mock.getCalls[0].key, "uploads/testfile")
		}
	})

	t.Run("load not found - NoSuchKey from stat", func(t *testing.T) {
		mockObj := &mockB2Object{
			statErr: minio.ErrorResponse{Code: "NoSuchKey"},
		}
		mock := &mockB2Client{
			getFunc: func(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error) {
				return mockObj, nil
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		_, err := storage.Load(ctx, "nonexistent")
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound, got %v", err)
		}

		// Verify object was closed
		if !mockObj.closed {
			t.Error("expected object to be closed on error")
		}
	})

	t.Run("load GetObject error", func(t *testing.T) {
		expectedErr := errors.New("connection failed")
		mock := &mockB2Client{
			getFunc: func(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error) {
				return nil, expectedErr
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		_, err := storage.Load(ctx, "testfile")
		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})

	t.Run("load stat error (other)", func(t *testing.T) {
		expectedErr := errors.New("stat failed")
		mockObj := &mockB2Object{
			statErr: expectedErr,
		}
		mock := &mockB2Client{
			getFunc: func(ctx context.Context, bucket, key string, opts minio.GetObjectOptions) (B2Object, error) {
				return mockObj, nil
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		_, err := storage.Load(ctx, "testfile")
		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}

		// Verify object was closed
		if !mockObj.closed {
			t.Error("expected object to be closed on error")
		}
	})
}

func TestB2Storage_Delete(t *testing.T) {
	ctx := context.Background()

	t.Run("successful delete", func(t *testing.T) {
		mock := &mockB2Client{}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		err := storage.Delete(ctx, "testfile")
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		// Verify client was called correctly
		if len(mock.removeCalls) != 1 {
			t.Fatalf("expected 1 remove call, got %d", len(mock.removeCalls))
		}
		if mock.removeCalls[0].bucket != "test-bucket" {
			t.Errorf("bucket = %q, want %q", mock.removeCalls[0].bucket, "test-bucket")
		}
		if mock.removeCalls[0].key != "testfile" {
			t.Errorf("key = %q, want %q", mock.removeCalls[0].key, "testfile")
		}
	})

	t.Run("delete with prefix", func(t *testing.T) {
		mock := &mockB2Client{}
		storage := NewB2StorageWithClient(mock, "test-bucket", "uploads", "")

		err := storage.Delete(ctx, "testfile")
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		if mock.removeCalls[0].key != "uploads/testfile" {
			t.Errorf("key = %q, want %q", mock.removeCalls[0].key, "uploads/testfile")
		}
	})

	t.Run("delete not found", func(t *testing.T) {
		mock := &mockB2Client{
			removeFunc: func(ctx context.Context, bucket, key string, opts minio.RemoveObjectOptions) error {
				return minio.ErrorResponse{Code: "NoSuchKey"}
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		err := storage.Delete(ctx, "nonexistent")
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("delete error", func(t *testing.T) {
		expectedErr := errors.New("delete failed")
		mock := &mockB2Client{
			removeFunc: func(ctx context.Context, bucket, key string, opts minio.RemoveObjectOptions) error {
				return expectedErr
			},
		}
		storage := NewB2StorageWithClient(mock, "test-bucket", "", "")

		err := storage.Delete(ctx, "testfile")
		if err != expectedErr {
			t.Errorf("expected error %v, got %v", expectedErr, err)
		}
	})
}

func TestProgressReader(t *testing.T) {
	testData := []byte("hello, world!")

	t.Run("reports progress on read", func(t *testing.T) {
		var progressCalls []struct {
			read  int64
			total int64
		}

		pr := &progressReader{
			reader: bytes.NewReader(testData),
			total:  int64(len(testData)),
			onProgress: func(read, total int64) {
				progressCalls = append(progressCalls, struct {
					read  int64
					total int64
				}{read, total})
			},
		}

		// Read all data
		buf := make([]byte, 32)
		n, err := pr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read failed: %v", err)
		}
		if n != len(testData) {
			t.Errorf("Read returned %d bytes, want %d", n, len(testData))
		}

		// Verify progress was reported
		if len(progressCalls) == 0 {
			t.Error("expected progress to be reported")
		}
		lastCall := progressCalls[len(progressCalls)-1]
		if lastCall.read != int64(len(testData)) {
			t.Errorf("last read = %d, want %d", lastCall.read, len(testData))
		}
		if lastCall.total != int64(len(testData)) {
			t.Errorf("last total = %d, want %d", lastCall.total, len(testData))
		}
	})

	t.Run("accumulates reads", func(t *testing.T) {
		var lastRead int64

		pr := &progressReader{
			reader: bytes.NewReader(testData),
			total:  int64(len(testData)),
			onProgress: func(read, total int64) {
				lastRead = read
			},
		}

		// Read in small chunks
		buf := make([]byte, 5)
		totalRead := 0
		for {
			n, err := pr.Read(buf)
			totalRead += n
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Read failed: %v", err)
			}
		}

		if lastRead != int64(totalRead) {
			t.Errorf("accumulated read = %d, want %d", lastRead, totalRead)
		}
	})

	t.Run("nil progress callback", func(t *testing.T) {
		pr := &progressReader{
			reader:     bytes.NewReader(testData),
			total:      int64(len(testData)),
			onProgress: nil,
		}

		// Should not panic
		buf := make([]byte, 32)
		_, err := pr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read failed: %v", err)
		}
	})
}

func TestNewB2StorageWithClient(t *testing.T) {
	mock := &mockB2Client{}
	storage := NewB2StorageWithClient(mock, "my-bucket", "my-prefix", "https://cdn.example.com")

	if storage.client != mock {
		t.Error("client not set correctly")
	}
	if storage.bucket != "my-bucket" {
		t.Errorf("bucket = %q, want %q", storage.bucket, "my-bucket")
	}
	if storage.prefix != "my-prefix" {
		t.Errorf("prefix = %q, want %q", storage.prefix, "my-prefix")
	}
	if storage.publicURL != "https://cdn.example.com" {
		t.Errorf("publicURL = %q, want %q", storage.publicURL, "https://cdn.example.com")
	}
}
