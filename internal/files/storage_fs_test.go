package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSStorage_ValidateID(t *testing.T) {
	storage := &FSStorage{basePath: "/tmp"}

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid alphanumeric", "abc123XYZ", false},
		{"valid hex", "a1b2c3d4e5f6", false},
		{"empty", "", true},
		{"path traversal dots", "../etc/passwd", true},
		{"path traversal encoded", "..%2F..%2Fetc", true},
		{"contains slash", "path/to/file", true},
		{"contains backslash", "path\\to\\file", true},
		{"contains dot", "file.txt", true},
		{"contains space", "file name", true},
		{"contains dash", "file-name", true},
		{"contains underscore", "file_name", true},
		{"too long", strings.Repeat("a", 65), true},
		{"max length valid", strings.Repeat("a", 64), false},
		{"special chars", "file<script>", true},
		{"null byte", "file\x00name", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := storage.validateID(tc.id)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateID(%q) error = %v, wantErr %v", tc.id, err, tc.wantErr)
			}
		})
	}
}

func TestFSStorage_SaveLoadDelete(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "fsstorage_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storage, err := NewFSStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	ctx := context.Background()
	testID := "testfile123"
	testData := []byte("hello, world!")

	t.Run("save file", func(t *testing.T) {
		n, err := storage.Save(ctx, testID, bytes.NewReader(testData))
		if err != nil {
			t.Fatalf("Save failed: %v", err)
		}
		if n != int64(len(testData)) {
			t.Errorf("Save returned %d bytes, want %d", n, len(testData))
		}

		// Verify file exists on disk
		path := filepath.Join(tmpDir, testID)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Error("file should exist on disk")
		}
	})

	t.Run("load file", func(t *testing.T) {
		reader, err := storage.Load(ctx, testID)
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
	})

	t.Run("load nonexistent file", func(t *testing.T) {
		_, err := storage.Load(ctx, "nonexistent")
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("delete file", func(t *testing.T) {
		err := storage.Delete(ctx, testID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}

		// Verify file is gone
		_, err = storage.Load(ctx, testID)
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound after delete, got %v", err)
		}
	})

	t.Run("delete nonexistent file", func(t *testing.T) {
		err := storage.Delete(ctx, "nonexistent")
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("save with invalid ID", func(t *testing.T) {
		_, err := storage.Save(ctx, "../invalid", bytes.NewReader(testData))
		if err != ErrInvalidID {
			t.Errorf("expected ErrInvalidID, got %v", err)
		}
	})

	t.Run("load with invalid ID", func(t *testing.T) {
		_, err := storage.Load(ctx, "../invalid")
		if err != ErrInvalidID {
			t.Errorf("expected ErrInvalidID, got %v", err)
		}
	})

	t.Run("delete with invalid ID", func(t *testing.T) {
		err := storage.Delete(ctx, "../invalid")
		if err != ErrInvalidID {
			t.Errorf("expected ErrInvalidID, got %v", err)
		}
	})
}

func TestFSStorage_SaveWithProgress(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fsstorage_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storage, err := NewFSStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	ctx := context.Background()
	testID := "progresstest"
	testData := []byte("test data for progress tracking")

	var progressCalls int
	var lastWritten, lastTotal int64

	onProgress := func(written, total int64) {
		progressCalls++
		lastWritten = written
		lastTotal = total
	}

	n, err := storage.SaveWithProgress(ctx, testID, bytes.NewReader(testData), int64(len(testData)), onProgress)
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
}

func TestFSStorage_NewFSStorage(t *testing.T) {
	t.Run("creates directory if not exists", func(t *testing.T) {
		tmpDir := filepath.Join(os.TempDir(), "fsstorage_new_test")
		defer os.RemoveAll(tmpDir)

		storage, err := NewFSStorage(tmpDir)
		if err != nil {
			t.Fatalf("NewFSStorage failed: %v", err)
		}

		if storage.basePath != tmpDir {
			t.Errorf("basePath = %q, want %q", storage.basePath, tmpDir)
		}

		// Verify directory was created
		info, err := os.Stat(tmpDir)
		if err != nil {
			t.Fatalf("directory should exist: %v", err)
		}
		if !info.IsDir() {
			t.Error("should be a directory")
		}
	})
}

func TestFSStorage_Path(t *testing.T) {
	storage := &FSStorage{basePath: "/var/uploads"}

	path := storage.path("testfile123")
	expected := "/var/uploads/testfile123"

	if path != expected {
		t.Errorf("path() = %q, want %q", path, expected)
	}
}
