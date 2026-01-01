package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"satoshisend/internal/files"
	"satoshisend/internal/payments"
	"satoshisend/internal/store"
)

// Test mocks

type mockStorage struct {
	files map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{files: make(map[string][]byte)}
}

func (m *mockStorage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return m.SaveWithProgress(ctx, id, data, -1, nil)
}

func (m *mockStorage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress files.ProgressFunc) (int64, error) {
	buf, _ := io.ReadAll(data)
	m.files[id] = buf
	if onProgress != nil {
		onProgress(int64(len(buf)), int64(len(buf)))
	}
	return int64(len(buf)), nil
}

func (m *mockStorage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	data, ok := m.files[id]
	if !ok {
		return nil, files.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *mockStorage) Delete(ctx context.Context, id string) error {
	delete(m.files, id)
	return nil
}

type mockStore struct {
	files    map[string]*store.FileMeta
	invoices map[string]*store.PendingInvoice
}

func newMockStore() *mockStore {
	return &mockStore{
		files:    make(map[string]*store.FileMeta),
		invoices: make(map[string]*store.PendingInvoice),
	}
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
	if meta, ok := m.files[fileID]; ok {
		meta.Paid = paid
	}
	return nil
}

func (m *mockStore) DeleteFileMetadata(ctx context.Context, id string) error {
	delete(m.files, id)
	return nil
}

func (m *mockStore) ListExpiredFiles(ctx context.Context) ([]*store.FileMeta, error) {
	return nil, nil
}

func (m *mockStore) GetStats(ctx context.Context) (*store.Stats, error) {
	return &store.Stats{}, nil
}

func (m *mockStore) Close() error {
	return nil
}

func (m *mockStore) SavePendingInvoice(ctx context.Context, inv *store.PendingInvoice) error {
	m.invoices[inv.PaymentHash] = inv
	return nil
}

func (m *mockStore) DeletePendingInvoice(ctx context.Context, paymentHash string) error {
	delete(m.invoices, paymentHash)
	return nil
}

func (m *mockStore) ListPendingInvoices(ctx context.Context) ([]*store.PendingInvoice, error) {
	var result []*store.PendingInvoice
	for _, inv := range m.invoices {
		result = append(result, inv)
	}
	return result, nil
}

func setupTestHandler() (*Handler, *mockStore) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil)
	return handler, st
}

func TestHandler_Upload(t *testing.T) {
	handler, _ := setupTestHandler()

	// Create multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("encrypted content here"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Parse the upload start response
	var startResp UploadStartResponse
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if startResp.UploadID == "" {
		t.Error("expected upload ID in response")
	}
	if startResp.Size == 0 {
		t.Error("expected size in response")
	}

	// Poll for progress until complete (with timeout)
	var progressResp UploadProgressResponse
	for i := 0; i < 50; i++ { // Max 5 seconds
		req := httptest.NewRequest("GET", "/api/upload/"+startResp.UploadID+"/progress", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
			break
		}

		if err := json.NewDecoder(rec.Body).Decode(&progressResp); err != nil {
			t.Fatalf("failed to decode progress: %v", err)
		}

		if progressResp.Status == "complete" {
			break
		}
		if progressResp.Status == "error" {
			t.Fatalf("upload failed: %s", progressResp.Error)
		}

		time.Sleep(100 * time.Millisecond)
	}

	if progressResp.Status != "complete" {
		t.Fatalf("upload did not complete, status: %s", progressResp.Status)
	}
	if progressResp.Result == nil {
		t.Fatal("expected result in completed response")
	}
	if progressResp.Result.FileID == "" {
		t.Error("expected file ID in result")
	}
	if progressResp.Result.PaymentRequest == "" {
		t.Error("expected payment request in result")
	}
}

func TestHandler_Download_NotPaid(t *testing.T) {
	handler, st := setupTestHandler()

	// Create a file that's not paid
	st.SaveFileMetadata(context.Background(), &store.FileMeta{
		ID:        "abc123def456789",
		Size:      100,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Paid:      false,
		CreatedAt: time.Now(),
	})

	req := httptest.NewRequest("GET", "/api/file/abc123def456789", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("expected 402, got %d", rec.Code)
	}
}

func TestHandler_Status(t *testing.T) {
	handler, st := setupTestHandler()

	st.SaveFileMetadata(context.Background(), &store.FileMeta{
		ID:        "statustest123456",
		Size:      2048,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Paid:      true,
		CreatedAt: time.Now(),
	})

	req := httptest.NewRequest("GET", "/api/file/statustest123456/status", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var resp StatusResponse
	json.NewDecoder(rec.Body).Decode(&resp)

	if !resp.Paid {
		t.Error("expected paid to be true")
	}
	if resp.Size != 2048 {
		t.Errorf("expected size 2048, got %d", resp.Size)
	}
}

func TestHandler_InvalidFileID(t *testing.T) {
	handler, _ := setupTestHandler()

	tests := []struct {
		name     string
		fileID   string
		expected int
	}{
		{"special chars", "file<script>", http.StatusBadRequest},
		{"dots in name", "file..test", http.StatusBadRequest},
		{"slashes encoded", "file%2Ftest", http.StatusBadRequest},
		{"too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", http.StatusBadRequest},
		{"dashes", "file-test", http.StatusBadRequest},
		{"underscores", "file_test", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/file/"+tc.fileID+"/status", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.expected {
				t.Errorf("expected %d for %q, got %d", tc.expected, tc.fileID, rec.Code)
			}
		})
	}
}

func TestCORS_AllowAll(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	corsHandler := CORS(CORSConfig{})(handler)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()

	corsHandler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("expected *, got %q", got)
	}
}

func TestCORS_RestrictedOrigins(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	corsHandler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://satoshisend.xyz", "https://localhost:3000"},
	})(handler)

	t.Run("allowed origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.Header.Set("Origin", "https://satoshisend.xyz")
		rec := httptest.NewRecorder()

		corsHandler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://satoshisend.xyz" {
			t.Errorf("expected https://satoshisend.xyz, got %q", got)
		}
	})

	t.Run("disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.Header.Set("Origin", "https://evil.com")
		rec := httptest.NewRecorder()

		corsHandler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("preflight request", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/api/test", nil)
		req.Header.Set("Origin", "https://satoshisend.xyz")
		rec := httptest.NewRecorder()

		corsHandler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for preflight, got %d", rec.Code)
		}
	})
}

func TestRateLimit(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Very restrictive config for testing
	cfg := RateLimitConfig{
		RequestsPerSecond:       1,
		BurstSize:               2,
		UploadRequestsPerMinute: 1,
		UploadBurstSize:         1,
	}

	rateLimiter := NewRateLimiter(cfg)
	defer rateLimiter.Stop() // Clean up goroutines
	rateLimitedHandler := rateLimiter.Middleware(handler)

	t.Run("allows requests within limit", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		rec := httptest.NewRecorder()

		rateLimitedHandler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		// Use a different IP to get a fresh limiter
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest("GET", "/api/test", nil)
			req.RemoteAddr = "10.0.0.1:12345"
			rec := httptest.NewRecorder()

			rateLimitedHandler.ServeHTTP(rec, req)

			// First 2 should pass (burst), rest should be rate limited
			if i < 2 && rec.Code != http.StatusOK {
				t.Errorf("request %d: expected 200, got %d", i, rec.Code)
			}
			if i >= 2 && rec.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: expected 429, got %d", i, rec.Code)
			}
		}
	})

	t.Run("uses X-Forwarded-For header", func(t *testing.T) {
		// This IP gets a fresh limiter
		req := httptest.NewRequest("GET", "/api/test", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.41.3.18")
		rec := httptest.NewRecorder()

		rateLimitedHandler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})
}

func TestRateLimiterCleanup(t *testing.T) {
	// Use a short TTL for testing (must be >= 1 second since lastSeen uses Unix seconds)
	rl := newIPRateLimiterWithTTL(10, 5, 1*time.Second)
	defer rl.Stop()

	// Access some IPs to create limiter entries
	rl.getLimiter("192.168.1.1")
	rl.getLimiter("192.168.1.2")
	rl.getLimiter("192.168.1.3")

	// Verify entries exist
	count := 0
	rl.limiters.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 3 {
		t.Errorf("expected 3 entries, got %d", count)
	}

	// Wait for TTL to expire (need > 1 second since lastSeen uses Unix seconds)
	// Use 2 seconds to account for timing precision on busy systems
	time.Sleep(2 * time.Second)

	// Manually trigger cleanup (faster than waiting for ticker)
	rl.cleanup()

	// Verify entries were cleaned up
	count = 0
	rl.limiters.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Errorf("expected 0 entries after cleanup, got %d", count)
	}
}

func TestRateLimiterCleanupPreservesActive(t *testing.T) {
	// Use a 2-second TTL for testing (lastSeen uses Unix seconds)
	rl := newIPRateLimiterWithTTL(10, 5, 2*time.Second)
	defer rl.Stop()

	// Add a stale IP first
	rl.getLimiter("192.168.1.2")

	// Wait for the stale IP to age past TTL
	time.Sleep(3 * time.Second)

	// Now add an active IP (will have recent lastSeen)
	rl.getLimiter("192.168.1.1")

	// Trigger cleanup - stale one should be removed, fresh one should remain
	rl.cleanup()

	var remaining []string
	rl.limiters.Range(func(key, _ any) bool {
		remaining = append(remaining, key.(string))
		return true
	})

	if len(remaining) != 1 || remaining[0] != "192.168.1.1" {
		t.Errorf("expected only 192.168.1.1 to remain, got: %v", remaining)
	}
}

func TestRateLimiterStop(t *testing.T) {
	rl := newIPRateLimiterWithTTL(10, 5, 10*time.Millisecond)

	// Calling Stop multiple times should not panic
	rl.Stop()
	rl.Stop()
}

func TestHandler_Download_Expired(t *testing.T) {
	handler, st := setupTestHandler()

	// Create an expired file
	st.SaveFileMetadata(context.Background(), &store.FileMeta{
		ID:        "expiredfile12345",
		Size:      100,
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Expired
		Paid:      true,
		CreatedAt: time.Now().Add(-25 * time.Hour),
	})

	req := httptest.NewRequest("GET", "/api/file/expiredfile12345", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Errorf("expected 410 Gone, got %d", rec.Code)
	}
}

func TestHandler_Download_NotFound(t *testing.T) {
	handler, _ := setupTestHandler()

	req := httptest.NewRequest("GET", "/api/file/nonexistentfile1", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandler_Status_NotFound(t *testing.T) {
	handler, _ := setupTestHandler()

	req := httptest.NewRequest("GET", "/api/file/nonexistent123/status", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandler_GetInvoice(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil)

	// Create an invoice for a file
	ctx := context.Background()
	st.SaveFileMetadata(ctx, &store.FileMeta{
		ID:        "invoicetest12345",
		Size:      1024,
		ExpiresAt: time.Now().Add(1 * time.Hour),
		Paid:      false,
		CreatedAt: time.Now(),
	})

	// Create invoice through payment service
	_, err := paymentsSvc.CreateInvoiceForFile(ctx, "invoicetest12345", 100)
	if err != nil {
		t.Fatalf("failed to create invoice: %v", err)
	}

	t.Run("get existing invoice", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/file/invoicetest12345/invoice", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp InvoiceResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.PaymentRequest == "" {
			t.Error("expected payment request")
		}
		if resp.AmountSats != 100 {
			t.Errorf("expected 100 sats, got %d", resp.AmountSats)
		}
	})

	t.Run("get nonexistent invoice", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/file/noinvoice123456/invoice", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", rec.Code)
		}
	})
}

// mockWebhookHandler implements WebhookHandler for testing
type mockWebhookHandler struct {
	lastBody    []byte
	lastHeaders http.Header
	returnError error
}

func (m *mockWebhookHandler) HandleWebhook(body []byte, headers http.Header) error {
	m.lastBody = body
	m.lastHeaders = headers
	return m.returnError
}

func TestHandler_AlbyWebhook(t *testing.T) {
	handler, _ := setupTestHandler()

	t.Run("no webhook handler configured", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/webhook/alby", bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d", rec.Code)
		}
	})

	t.Run("with webhook handler success", func(t *testing.T) {
		mockWH := &mockWebhookHandler{}
		handler.SetWebhookHandler(mockWH)

		body := `{"payment_hash":"abc123","settled":true}`
		req := httptest.NewRequest("POST", "/api/webhook/alby", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}

		if string(mockWH.lastBody) != body {
			t.Errorf("expected body %q, got %q", body, string(mockWH.lastBody))
		}
	})

	t.Run("with webhook handler error", func(t *testing.T) {
		mockWH := &mockWebhookHandler{
			returnError: io.EOF, // Any error
		}
		handler.SetWebhookHandler(mockWH)

		req := httptest.NewRequest("POST", "/api/webhook/alby", bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rec.Code)
		}
	})
}

func TestHandler_UploadProgress_NotFound(t *testing.T) {
	handler, _ := setupTestHandler()

	req := httptest.NewRequest("GET", "/api/upload/nonexistent123/progress", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestDefaultRateLimitConfig(t *testing.T) {
	cfg := DefaultRateLimitConfig()

	if cfg.RequestsPerSecond <= 0 {
		t.Error("RequestsPerSecond should be positive")
	}
	if cfg.BurstSize <= 0 {
		t.Error("BurstSize should be positive")
	}
	if cfg.UploadRequestsPerMinute <= 0 {
		t.Error("UploadRequestsPerMinute should be positive")
	}
	if cfg.UploadBurstSize <= 0 {
		t.Error("UploadBurstSize should be positive")
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		want       string
	}{
		{"remote addr only", "192.168.1.1:12345", "", "", "192.168.1.1"},
		{"X-Forwarded-For single", "127.0.0.1:80", "203.0.113.50", "", "203.0.113.50"},
		{"X-Forwarded-For chain", "127.0.0.1:80", "203.0.113.50, 70.41.3.18", "", "203.0.113.50"},
		{"X-Real-IP", "127.0.0.1:80", "", "203.0.113.100", "203.0.113.100"},
		{"X-Forwarded-For takes precedence", "127.0.0.1:80", "1.2.3.4", "5.6.7.8", "1.2.3.4"},
		{"IPv6", "[::1]:8080", "", "", "[::1]"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xri != "" {
				req.Header.Set("X-Real-IP", tc.xri)
			}

			got := extractIP(req)
			if got != tc.want {
				t.Errorf("extractIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Helper to upload a file and wait for completion
func uploadFileWithIP(t *testing.T, handler *Handler, ip string) (*UploadProgressResponse, int) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("test content for pending limit"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}

	var startResp UploadStartResponse
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("failed to decode start response: %v", err)
	}

	// Poll for completion
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("GET", "/api/upload/"+startResp.UploadID+"/progress", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var progress UploadProgressResponse
		if err := json.NewDecoder(rec.Body).Decode(&progress); err != nil {
			t.Fatalf("failed to decode progress: %v", err)
		}

		if progress.Status == "complete" {
			return &progress, http.StatusOK
		}
		if progress.Status == "error" {
			t.Fatalf("upload failed: %s", progress.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("upload did not complete in time")
	return nil, 0
}

func TestHandler_Upload_PendingLimit(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)
	pendingLimiter := NewPendingFileLimiter(2) // Low limit for testing

	handler := NewHandler(filesSvc, paymentsSvc, pendingLimiter)

	ip := "10.0.0.99"

	// First two uploads should succeed
	for i := 0; i < 2; i++ {
		resp, code := uploadFileWithIP(t, handler, ip)
		if code != http.StatusOK {
			t.Fatalf("upload %d failed with code %d", i+1, code)
		}
		if resp == nil || resp.Result == nil {
			t.Fatalf("upload %d did not complete", i+1)
		}
	}

	// Third upload should be blocked
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("blocked content"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for 3rd upload, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify error message contains helpful info
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte("pending file limit")) {
		t.Error("error message should mention pending file limit")
	}
	if !bytes.Contains([]byte(body), []byte("2")) {
		t.Error("error message should mention count")
	}
}

func TestHandler_Upload_PendingLimit_DifferentIPs(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)
	pendingLimiter := NewPendingFileLimiter(2)

	handler := NewHandler(filesSvc, paymentsSvc, pendingLimiter)

	// Fill up limit for IP1
	for i := 0; i < 2; i++ {
		_, code := uploadFileWithIP(t, handler, "10.0.0.1")
		if code != http.StatusOK {
			t.Fatalf("upload for IP1 failed")
		}
	}

	// IP2 should still be able to upload
	_, code := uploadFileWithIP(t, handler, "10.0.0.2")
	if code != http.StatusOK {
		t.Errorf("IP2 should be able to upload, got %d", code)
	}
}

func TestHandler_Upload_PendingLimit_ClearsOnPayment(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)
	pendingLimiter := NewPendingFileLimiter(1) // Very strict limit

	// Wire up the callback
	paymentsSvc.SetPaymentCallback(pendingLimiter.OnPaymentReceived)

	// Start payment watcher so it can receive simulated payments
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := paymentsSvc.StartPaymentWatcher(ctx); err != nil {
		t.Fatalf("failed to start payment watcher: %v", err)
	}

	handler := NewHandler(filesSvc, paymentsSvc, pendingLimiter)

	ip := "10.0.0.50"

	// Upload first file
	resp, code := uploadFileWithIP(t, handler, ip)
	if code != http.StatusOK {
		t.Fatalf("first upload failed with code %d", code)
	}
	if resp == nil || resp.Result == nil {
		t.Fatal("first upload did not complete")
	}

	fileID := resp.Result.FileID
	paymentHash := resp.Result.PaymentHash

	// Second upload should be blocked
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "blocked.txt")
	part.Write([]byte("should be blocked"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 before payment, got %d", rec.Code)
	}

	// Simulate payment
	lnd.SimulatePayment(paymentHash)

	// Give the payment handler time to process
	time.Sleep(100 * time.Millisecond)

	// Verify file is marked as paid
	meta, err := st.GetFileMetadata(context.Background(), fileID)
	if err != nil {
		t.Fatalf("failed to get metadata: %v", err)
	}
	if !meta.Paid {
		t.Error("file should be marked as paid")
	}

	// Now upload should succeed
	resp2, code2 := uploadFileWithIP(t, handler, ip)
	if code2 != http.StatusOK {
		t.Errorf("upload after payment should succeed, got %d", code2)
	}
	if resp2 == nil || resp2.Result == nil {
		t.Error("upload after payment did not complete")
	}
}

func TestHandler_Upload_NoPendingLimiter(t *testing.T) {
	// Test that handler works when no limiter is configured
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil) // No limiter

	// Should be able to upload many files
	for i := 0; i < 5; i++ {
		_, code := uploadFileWithIP(t, handler, "10.0.0.1")
		if code != http.StatusOK {
			t.Errorf("upload %d should succeed without limiter, got %d", i+1, code)
		}
	}
}

// --- Temp file handling tests ---

func TestHandler_Upload_TempFileCleanup(t *testing.T) {
	// Create a dedicated temp directory for this test
	testTempDir, err := os.MkdirTemp("", "satoshisend-test-*")
	if err != nil {
		t.Fatalf("failed to create test temp dir: %v", err)
	}
	defer os.RemoveAll(testTempDir)

	// Override the global TempDir for this test
	originalTempDir := TempDir
	TempDir = testTempDir
	defer func() { TempDir = originalTempDir }()

	handler, _ := setupTestHandler()

	// Upload a file
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("test content for temp file cleanup"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload failed: %d - %s", rec.Code, rec.Body.String())
	}

	var startResp UploadStartResponse
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Poll until complete
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("GET", "/api/upload/"+startResp.UploadID+"/progress", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var progress UploadProgressResponse
		json.NewDecoder(rec.Body).Decode(&progress)

		if progress.Status == "complete" {
			break
		}
		if progress.Status == "error" {
			t.Fatalf("upload failed: %s", progress.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Give a moment for cleanup
	time.Sleep(100 * time.Millisecond)

	// Verify temp file was cleaned up
	files, _ := filepath.Glob(filepath.Join(testTempDir, TempFilePrefix+"*"))
	if len(files) > 0 {
		t.Errorf("temp files should be cleaned up after successful upload, found: %v", files)
	}
}

func TestHandler_Upload_TempFileCleanupOnError(t *testing.T) {
	// Create a dedicated temp directory for this test
	testTempDir, err := os.MkdirTemp("", "satoshisend-test-*")
	if err != nil {
		t.Fatalf("failed to create test temp dir: %v", err)
	}
	defer os.RemoveAll(testTempDir)

	// Override the global TempDir for this test
	originalTempDir := TempDir
	TempDir = testTempDir
	defer func() { TempDir = originalTempDir }()

	// Use a failing storage
	storage := &failingMockStorage{failAfter: 0}
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil)

	// Upload a file
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("test content for error cleanup"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload start failed: %d", rec.Code)
	}

	var startResp UploadStartResponse
	json.NewDecoder(rec.Body).Decode(&startResp)

	// Poll until error (with all retries exhausted)
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/api/upload/"+startResp.UploadID+"/progress", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var progress UploadProgressResponse
		json.NewDecoder(rec.Body).Decode(&progress)

		if progress.Status == "error" {
			break
		}
		if progress.Status == "complete" {
			t.Fatal("upload should have failed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Give a moment for cleanup
	time.Sleep(100 * time.Millisecond)

	// Verify temp file was cleaned up
	files, _ := filepath.Glob(filepath.Join(testTempDir, TempFilePrefix+"*"))
	if len(files) > 0 {
		t.Errorf("temp files should be cleaned up after failed upload, found: %v", files)
	}
}

// failingMockStorage fails after a certain number of calls
type failingMockStorage struct {
	callCount int
	failAfter int // fail on all calls if 0
}

func (m *failingMockStorage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return m.SaveWithProgress(ctx, id, data, -1, nil)
}

func (m *failingMockStorage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress files.ProgressFunc) (int64, error) {
	m.callCount++
	if m.failAfter == 0 || m.callCount <= m.failAfter {
		return 0, errors.New("simulated storage failure")
	}
	// Drain the reader
	n, _ := io.Copy(io.Discard, data)
	return n, nil
}

func (m *failingMockStorage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	return nil, files.ErrNotFound
}

func (m *failingMockStorage) Delete(ctx context.Context, id string) error {
	return nil
}

func TestHandler_Upload_RetryLogic(t *testing.T) {
	// Create a dedicated temp directory for this test
	testTempDir, err := os.MkdirTemp("", "satoshisend-test-*")
	if err != nil {
		t.Fatalf("failed to create test temp dir: %v", err)
	}
	defer os.RemoveAll(testTempDir)

	// Override the global TempDir for this test
	originalTempDir := TempDir
	TempDir = testTempDir
	defer func() { TempDir = originalTempDir }()

	// Storage that fails first 2 attempts, succeeds on 3rd
	storage := &retryMockStorage{
		failCount:    2,
		successFiles: make(map[string][]byte),
	}
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil)

	// Upload a file
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("test content for retry"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload start failed: %d", rec.Code)
	}

	var startResp UploadStartResponse
	json.NewDecoder(rec.Body).Decode(&startResp)

	// Poll until complete
	var finalStatus string
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/api/upload/"+startResp.UploadID+"/progress", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var progress UploadProgressResponse
		json.NewDecoder(rec.Body).Decode(&progress)

		finalStatus = progress.Status
		if progress.Status == "complete" || progress.Status == "error" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalStatus != "complete" {
		t.Errorf("upload should have succeeded after retries, got status: %s", finalStatus)
	}

	// Verify storage was called 3 times (2 failures + 1 success)
	if storage.callCount != 3 {
		t.Errorf("expected 3 storage calls (2 failures + 1 success), got %d", storage.callCount)
	}
}

// retryMockStorage fails first N attempts, then succeeds
type retryMockStorage struct {
	callCount    int
	failCount    int
	successFiles map[string][]byte
}

func (m *retryMockStorage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return m.SaveWithProgress(ctx, id, data, -1, nil)
}

func (m *retryMockStorage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress files.ProgressFunc) (int64, error) {
	m.callCount++
	if m.callCount <= m.failCount {
		return 0, errors.New("simulated temporary failure")
	}
	buf, _ := io.ReadAll(data)
	m.successFiles[id] = buf
	if onProgress != nil {
		onProgress(int64(len(buf)), int64(len(buf)))
	}
	return int64(len(buf)), nil
}

func (m *retryMockStorage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	data, ok := m.successFiles[id]
	if !ok {
		return nil, files.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *retryMockStorage) Delete(ctx context.Context, id string) error {
	delete(m.successFiles, id)
	return nil
}

func TestCleanupOrphanedTempFiles(t *testing.T) {
	// Create a dedicated temp directory for this test
	testTempDir, err := os.MkdirTemp("", "satoshisend-test-*")
	if err != nil {
		t.Fatalf("failed to create test temp dir: %v", err)
	}
	defer os.RemoveAll(testTempDir)

	// Override the global TempDir for this test
	originalTempDir := TempDir
	TempDir = testTempDir
	defer func() { TempDir = originalTempDir }()

	// Create some orphaned temp files
	orphanedFiles := []string{
		filepath.Join(testTempDir, TempFilePrefix+"abc123-456"),
		filepath.Join(testTempDir, TempFilePrefix+"def456-789"),
		filepath.Join(testTempDir, TempFilePrefix+"ghi789-012"),
	}
	for _, f := range orphanedFiles {
		if err := os.WriteFile(f, []byte("orphaned content"), 0644); err != nil {
			t.Fatalf("failed to create orphaned file: %v", err)
		}
	}

	// Create a non-matching file (should NOT be deleted)
	nonMatchingFile := filepath.Join(testTempDir, "other-file.txt")
	if err := os.WriteFile(nonMatchingFile, []byte("other content"), 0644); err != nil {
		t.Fatalf("failed to create non-matching file: %v", err)
	}

	// Run cleanup
	count := CleanupOrphanedTempFiles()

	if count != 3 {
		t.Errorf("expected 3 files cleaned up, got %d", count)
	}

	// Verify orphaned files are gone
	for _, f := range orphanedFiles {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("orphaned file should be deleted: %s", f)
		}
	}

	// Verify non-matching file is still there
	if _, err := os.Stat(nonMatchingFile); os.IsNotExist(err) {
		t.Error("non-matching file should NOT be deleted")
	}
}

func TestCleanupOrphanedTempFiles_EmptyDir(t *testing.T) {
	// Create a dedicated temp directory for this test
	testTempDir, err := os.MkdirTemp("", "satoshisend-test-*")
	if err != nil {
		t.Fatalf("failed to create test temp dir: %v", err)
	}
	defer os.RemoveAll(testTempDir)

	// Override the global TempDir for this test
	originalTempDir := TempDir
	TempDir = testTempDir
	defer func() { TempDir = originalTempDir }()

	// Run cleanup on empty directory
	count := CleanupOrphanedTempFiles()

	if count != 0 {
		t.Errorf("expected 0 files cleaned up, got %d", count)
	}
}

func TestHandler_Upload_UsesTempDir(t *testing.T) {
	// Create a dedicated temp directory for this test
	testTempDir, err := os.MkdirTemp("", "satoshisend-test-*")
	if err != nil {
		t.Fatalf("failed to create test temp dir: %v", err)
	}
	defer os.RemoveAll(testTempDir)

	// Override the global TempDir for this test
	originalTempDir := TempDir
	TempDir = testTempDir
	defer func() { TempDir = originalTempDir }()

	// Use a slow storage to give us time to check the temp file
	storage := &slowMockStorage{delay: 200 * time.Millisecond}
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil)

	// Upload a file
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "test.txt")
	part.Write([]byte("test content"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/upload", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload failed: %d", rec.Code)
	}

	// Check that a temp file exists while upload is in progress
	time.Sleep(50 * time.Millisecond)
	files, _ := filepath.Glob(filepath.Join(testTempDir, TempFilePrefix+"*"))
	if len(files) != 1 {
		t.Errorf("expected 1 temp file during upload, found %d", len(files))
	}

	// Verify it has our prefix
	if len(files) > 0 && !strings.Contains(files[0], TempFilePrefix) {
		t.Errorf("temp file should have prefix %s, got %s", TempFilePrefix, files[0])
	}
}

// slowMockStorage adds a delay to simulate slow uploads
type slowMockStorage struct {
	delay time.Duration
	files map[string][]byte
}

func (m *slowMockStorage) Save(ctx context.Context, id string, data io.Reader) (int64, error) {
	return m.SaveWithProgress(ctx, id, data, -1, nil)
}

func (m *slowMockStorage) SaveWithProgress(ctx context.Context, id string, data io.Reader, size int64, onProgress files.ProgressFunc) (int64, error) {
	time.Sleep(m.delay)
	if m.files == nil {
		m.files = make(map[string][]byte)
	}
	buf, _ := io.ReadAll(data)
	m.files[id] = buf
	if onProgress != nil {
		onProgress(int64(len(buf)), int64(len(buf)))
	}
	return int64(len(buf)), nil
}

func (m *slowMockStorage) Load(ctx context.Context, id string) (io.ReadCloser, error) {
	data, ok := m.files[id]
	if !ok {
		return nil, files.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *slowMockStorage) Delete(ctx context.Context, id string) error {
	delete(m.files, id)
	return nil
}
