package files

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"satoshisend/internal/store"
)

// mockStorage implements Storage for testing.
type mockStorage struct {
	files map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{files: make(map[string][]byte)}
}

func (m *mockStorage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return m.SaveWithProgress(ctx, id, data, -1, nil)
}

func (m *mockStorage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress ProgressFunc) (int64, error) {
	buf, err := io.ReadAll(data)
	if err != nil {
		return 0, err
	}
	m.files[id] = buf
	if onProgress != nil {
		onProgress(int64(len(buf)), int64(len(buf)))
	}
	return int64(len(buf)), nil
}

func (m *mockStorage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	data, ok := m.files[id]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *mockStorage) Delete(ctx context.Context, id string) error {
	if _, ok := m.files[id]; !ok {
		return ErrNotFound
	}
	delete(m.files, id)
	return nil
}

// mockStore implements store.Store for testing.
type mockStore struct {
	files map[string]*store.FileMeta
}

func newMockStore() *mockStore {
	return &mockStore{files: make(map[string]*store.FileMeta)}
}

func (m *mockStore) SaveFileMetadata(ctx context.Context, meta *store.FileMeta) error {
	m.files[meta.ID] = meta
	return nil
}

func (m *mockStore) GetFileMetadata(ctx context.Context, id string) (*store.FileMeta, error) {
	meta, ok := m.files[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return meta, nil
}

func (m *mockStore) UpdatePaymentStatus(ctx context.Context, fileID string, paid bool) error {
	meta, ok := m.files[fileID]
	if !ok {
		return store.ErrNotFound
	}
	meta.Paid = paid
	if paid {
		// Extend expiration to now + HostDuration (mimics SQLite behavior)
		meta.ExpiresAt = time.Now().Add(meta.HostDuration)
	}
	return nil
}

func (m *mockStore) DeleteFileMetadata(ctx context.Context, id string) error {
	if _, ok := m.files[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.files, id)
	return nil
}

func (m *mockStore) ListExpiredFiles(ctx context.Context) ([]*store.FileMeta, error) {
	var expired []*store.FileMeta
	now := time.Now()
	for _, meta := range m.files {
		if meta.ExpiresAt.Before(now) {
			expired = append(expired, meta)
		}
	}
	return expired, nil
}

func (m *mockStore) GetStats(ctx context.Context) (*store.Stats, error) {
	return &store.Stats{}, nil
}

func (m *mockStore) Close() error {
	return nil
}

func TestService_Upload(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	svc := NewService(storage, st)

	ctx := context.Background()
	data := bytes.NewReader([]byte("encrypted file content"))
	hostDuration := 7 * 24 * time.Hour

	beforeUpload := time.Now()
	result, err := svc.Upload(ctx, data, hostDuration)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
	if result.Size != 22 {
		t.Errorf("expected size 22, got %d", result.Size)
	}

	// Verify file is stored but not paid
	meta, err := svc.GetMetadata(ctx, result.ID)
	if err != nil {
		t.Fatalf("get metadata failed: %v", err)
	}
	if meta.Paid {
		t.Error("expected file to be unpaid initially")
	}

	// Verify initial expiration is set to PendingTimeout (1 hour), not hostDuration
	expectedPendingExpiry := beforeUpload.Add(PendingTimeout)
	if meta.ExpiresAt.Before(expectedPendingExpiry.Add(-1*time.Minute)) || meta.ExpiresAt.After(expectedPendingExpiry.Add(1*time.Minute)) {
		t.Errorf("expected ExpiresAt around %v (pending timeout), got %v", expectedPendingExpiry, meta.ExpiresAt)
	}

	// Verify HostDuration is stored for later use
	if meta.HostDuration != hostDuration {
		t.Errorf("expected HostDuration %v, got %v", hostDuration, meta.HostDuration)
	}
}

func TestService_MarkPaidExtendsExpiration(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	svc := NewService(storage, st)

	ctx := context.Background()
	data := bytes.NewReader([]byte("encrypted file content"))
	hostDuration := 7 * 24 * time.Hour

	result, err := svc.Upload(ctx, data, hostDuration)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	// Verify initial expiration is short (PendingTimeout)
	metaBefore, _ := svc.GetMetadata(ctx, result.ID)
	if metaBefore.ExpiresAt.After(time.Now().Add(PendingTimeout + time.Minute)) {
		t.Error("initial expiration should be ~1 hour, not full host duration")
	}

	// Mark as paid
	beforePaid := time.Now()
	if err := svc.MarkPaid(ctx, result.ID); err != nil {
		t.Fatalf("mark paid failed: %v", err)
	}

	// Verify expiration is now extended to full hostDuration
	metaAfter, _ := svc.GetMetadata(ctx, result.ID)
	expectedExpiry := beforePaid.Add(hostDuration)
	if metaAfter.ExpiresAt.Before(expectedExpiry.Add(-1*time.Minute)) || metaAfter.ExpiresAt.After(expectedExpiry.Add(1*time.Minute)) {
		t.Errorf("expected ExpiresAt around %v after payment, got %v", expectedExpiry, metaAfter.ExpiresAt)
	}
}

func TestService_Download(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	svc := NewService(storage, st)

	ctx := context.Background()
	content := []byte("encrypted file content")
	data := bytes.NewReader(content)

	result, _ := svc.Upload(ctx, data, 24*time.Hour)

	t.Run("unpaid file", func(t *testing.T) {
		_, err := svc.Download(ctx, result.ID)
		if err != ErrNotPaid {
			t.Errorf("expected ErrNotPaid, got %v", err)
		}
	})

	t.Run("paid file", func(t *testing.T) {
		svc.MarkPaid(ctx, result.ID)

		reader, err := svc.Download(ctx, result.ID)
		if err != nil {
			t.Fatalf("download failed: %v", err)
		}
		defer reader.Close()

		downloaded, _ := io.ReadAll(reader)
		if !bytes.Equal(downloaded, content) {
			t.Errorf("content mismatch")
		}
	})
}

func TestService_CleanupExpired(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	svc := NewService(storage, st)

	ctx := context.Background()

	// Upload an expired file (by manipulating the store directly)
	data := bytes.NewReader([]byte("old content"))
	result, _ := svc.Upload(ctx, data, -1*time.Hour) // Already expired
	svc.MarkPaid(ctx, result.ID)

	// Upload a valid file
	data2 := bytes.NewReader([]byte("new content"))
	result2, _ := svc.Upload(ctx, data2, 24*time.Hour)
	svc.MarkPaid(ctx, result2.ID)

	count, err := svc.CleanupExpired(ctx)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 cleaned up, got %d", count)
	}

	// Expired file should be gone from metadata
	_, err = svc.GetMetadata(ctx, result.ID)
	if err != store.ErrNotFound {
		t.Error("expired file metadata should be deleted")
	}

	// Expired file should be gone from storage
	if _, exists := storage.files[result.ID]; exists {
		t.Error("expired file blob should be deleted from storage")
	}

	// Valid file should still exist
	_, err = svc.GetMetadata(ctx, result2.ID)
	if err != nil {
		t.Error("valid file should still exist")
	}
}

func TestService_CleanupExpired_UnpaidPendingFiles(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	svc := NewService(storage, st)

	ctx := context.Background()

	// Upload files - they start unpaid with PendingTimeout (1 hour) expiry
	unpaidOld := bytes.NewReader([]byte("unpaid old content"))
	resultOld, err := svc.Upload(ctx, unpaidOld, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	unpaidNew := bytes.NewReader([]byte("unpaid new content"))
	resultNew, err := svc.Upload(ctx, unpaidNew, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	// Simulate time passing - set the old file's expiry to the past
	// This simulates an unpaid file that has exceeded PendingTimeout
	st.files[resultOld.ID].ExpiresAt = time.Now().Add(-1 * time.Minute)

	// Verify both files exist before cleanup
	if _, exists := storage.files[resultOld.ID]; !exists {
		t.Fatal("old file should exist in storage before cleanup")
	}
	if _, exists := storage.files[resultNew.ID]; !exists {
		t.Fatal("new file should exist in storage before cleanup")
	}

	// Run cleanup
	count, err := svc.CleanupExpired(ctx)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 file cleaned up, got %d", count)
	}

	// Old unpaid file should be deleted (metadata)
	_, err = svc.GetMetadata(ctx, resultOld.ID)
	if err != store.ErrNotFound {
		t.Error("expired unpaid file metadata should be deleted")
	}

	// Old unpaid file should be deleted (storage blob)
	if _, exists := storage.files[resultOld.ID]; exists {
		t.Error("expired unpaid file blob should be deleted from storage")
	}

	// New unpaid file should still exist (not yet expired)
	meta, err := svc.GetMetadata(ctx, resultNew.ID)
	if err != nil {
		t.Error("non-expired unpaid file should still exist")
	}
	if meta.Paid {
		t.Error("file should still be unpaid")
	}

	// New file's storage blob should still exist
	if _, exists := storage.files[resultNew.ID]; !exists {
		t.Error("non-expired file blob should still exist in storage")
	}
}
