package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func (m *mockStorage) Stat(ctx context.Context, id string) (int64, error) {
	data, ok := m.files[id]
	if !ok {
		return 0, files.ErrNotFound
	}
	return int64(len(data)), nil
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

func setupTestHandler() (*Handler, *mockStorage, *mockStore) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)

	handler := NewHandler(filesSvc, paymentsSvc, nil)
	return handler, storage, st
}

func TestHandler_UploadInit(t *testing.T) {
	handler, _, _ := setupTestHandler()

	body := `{"size": 1024}`
	req := httptest.NewRequest("POST", "/api/upload/init", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UploadInitResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.FileID == "" {
		t.Error("expected file ID in response")
	}
}

func TestHandler_UploadInit_InvalidSize(t *testing.T) {
	handler, _, _ := setupTestHandler()

	tests := []struct {
		name     string
		body     string
		expected int
	}{
		{"zero size", `{"size": 0}`, http.StatusBadRequest},
		{"negative size", `{"size": -1}`, http.StatusBadRequest},
		{"too large", `{"size": 6000000000}`, http.StatusRequestEntityTooLarge},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/upload/init", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.expected {
				t.Errorf("expected %d, got %d: %s", tc.expected, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandler_UploadComplete(t *testing.T) {
	handler, storage, _ := setupTestHandler()

	// First, init an upload
	initBody := `{"size": 1024}`
	initReq := httptest.NewRequest("POST", "/api/upload/init", bytes.NewReader([]byte(initBody)))
	initReq.Header.Set("Content-Type", "application/json")
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	var initResp UploadInitResponse
	json.NewDecoder(initRec.Body).Decode(&initResp)

	// Simulate the file being uploaded to storage
	storage.files[initResp.FileID] = make([]byte, 1024)

	// Complete the upload
	completeBody := `{"file_id": "` + initResp.FileID + `", "size": 1024}`
	req := httptest.NewRequest("POST", "/api/upload/complete", bytes.NewReader([]byte(completeBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UploadCompleteResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.FileID != initResp.FileID {
		t.Errorf("expected file ID %s, got %s", initResp.FileID, resp.FileID)
	}
	if resp.PaymentRequest == "" {
		t.Error("expected payment request in response")
	}
	if resp.AmountSats < 100 {
		t.Errorf("expected at least 100 sats, got %d", resp.AmountSats)
	}
}

func TestHandler_UploadComplete_FileNotFound(t *testing.T) {
	handler, _, _ := setupTestHandler()

	body := `{"file_id": "nonexistent12345678901234567890ab", "size": 1024}`
	req := httptest.NewRequest("POST", "/api/upload/complete", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Download_NotPaid(t *testing.T) {
	handler, _, st := setupTestHandler()

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
	handler, _, st := setupTestHandler()

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
	handler, _, _ := setupTestHandler()

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
	defer rateLimiter.Stop()
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
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest("GET", "/api/test", nil)
			req.RemoteAddr = "10.0.0.1:12345"
			rec := httptest.NewRecorder()

			rateLimitedHandler.ServeHTTP(rec, req)

			if i < 2 && rec.Code != http.StatusOK {
				t.Errorf("request %d: expected 200, got %d", i, rec.Code)
			}
			if i >= 2 && rec.Code != http.StatusTooManyRequests {
				t.Errorf("request %d: expected 429, got %d", i, rec.Code)
			}
		}
	})
}

func TestHandler_Download_Expired(t *testing.T) {
	handler, _, st := setupTestHandler()

	st.SaveFileMetadata(context.Background(), &store.FileMeta{
		ID:        "expiredfile12345",
		Size:      100,
		ExpiresAt: time.Now().Add(-1 * time.Hour),
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
	handler, _, _ := setupTestHandler()

	req := httptest.NewRequest("GET", "/api/file/nonexistentfile1", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandler_Status_NotFound(t *testing.T) {
	handler, _, _ := setupTestHandler()

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

	ctx := context.Background()
	st.SaveFileMetadata(ctx, &store.FileMeta{
		ID:        "invoicetest12345",
		Size:      1024,
		ExpiresAt: time.Now().Add(1 * time.Hour),
		Paid:      false,
		CreatedAt: time.Now(),
	})

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
	handler, _, _ := setupTestHandler()

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
			returnError: io.EOF,
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

func TestHandler_UploadInit_PendingLimit(t *testing.T) {
	storage := newMockStorage()
	st := newMockStore()
	lnd := payments.NewMockLNDClient()

	filesSvc := files.NewService(storage, st)
	paymentsSvc := payments.NewService(lnd, st)
	pendingLimiter := NewPendingFileLimiter(2)

	handler := NewHandler(filesSvc, paymentsSvc, pendingLimiter)

	ip := "10.0.0.99"

	// First two uploads should succeed
	for i := 0; i < 2; i++ {
		// Init
		initBody := `{"size": 1024}`
		initReq := httptest.NewRequest("POST", "/api/upload/init", bytes.NewReader([]byte(initBody)))
		initReq.Header.Set("Content-Type", "application/json")
		initReq.Header.Set("X-Forwarded-For", ip)
		initRec := httptest.NewRecorder()
		handler.ServeHTTP(initRec, initReq)

		if initRec.Code != http.StatusOK {
			t.Fatalf("init %d failed: %d", i+1, initRec.Code)
		}

		var initResp UploadInitResponse
		json.NewDecoder(initRec.Body).Decode(&initResp)

		// Simulate upload to storage
		storage.files[initResp.FileID] = make([]byte, 1024)

		// Complete
		completeBody := `{"file_id": "` + initResp.FileID + `", "size": 1024}`
		completeReq := httptest.NewRequest("POST", "/api/upload/complete", bytes.NewReader([]byte(completeBody)))
		completeReq.Header.Set("Content-Type", "application/json")
		completeReq.Header.Set("X-Forwarded-For", ip)
		completeRec := httptest.NewRecorder()
		handler.ServeHTTP(completeRec, completeReq)

		if completeRec.Code != http.StatusOK {
			t.Fatalf("complete %d failed: %d", i+1, completeRec.Code)
		}
	}

	// Third upload should be blocked at init
	initBody := `{"size": 1024}`
	initReq := httptest.NewRequest("POST", "/api/upload/init", bytes.NewReader([]byte(initBody)))
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("X-Forwarded-For", ip)
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	if initRec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for 3rd upload, got %d: %s", initRec.Code, initRec.Body.String())
	}
}

func TestHandler_UploadStream(t *testing.T) {
	handler, _, _ := setupTestHandler()

	// First, init an upload to get a file ID
	initBody := `{"size": 1024}`
	initReq := httptest.NewRequest("POST", "/api/upload/init", bytes.NewReader([]byte(initBody)))
	initReq.Header.Set("Content-Type", "application/json")
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	if initRec.Code != http.StatusOK {
		t.Fatalf("init failed: %d", initRec.Code)
	}

	var initResp UploadInitResponse
	json.NewDecoder(initRec.Body).Decode(&initResp)

	// Stream upload the data
	data := make([]byte, 1024)
	req := httptest.NewRequest("PUT", "/api/upload/"+initResp.FileID, bytes.NewReader(data))
	req.ContentLength = 1024
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_UploadStream_InvalidID(t *testing.T) {
	handler, _, _ := setupTestHandler()

	data := make([]byte, 100)
	req := httptest.NewRequest("PUT", "/api/upload/invalid<>id", bytes.NewReader(data))
	req.ContentLength = 100
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_UploadStream_NoContentLength(t *testing.T) {
	handler, _, _ := setupTestHandler()

	req := httptest.NewRequest("PUT", "/api/upload/abc123def456", bytes.NewReader([]byte("data")))
	// Don't set ContentLength
	req.ContentLength = -1
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_UploadStream_TooLarge(t *testing.T) {
	handler, _, _ := setupTestHandler()

	req := httptest.NewRequest("PUT", "/api/upload/abc123def456", bytes.NewReader([]byte{}))
	req.ContentLength = MaxUploadSize + 1
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
}
