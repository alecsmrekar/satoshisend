package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"satoshisend/internal/files"
	"satoshisend/internal/logging"
	"satoshisend/internal/payments"
	"satoshisend/internal/store"
)

var validFileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// backgroundUpload tracks the state of a background B2 upload.
type backgroundUpload struct {
	mu           sync.RWMutex
	status       string          // "uploading", "complete", "error"
	progress     float64         // 0.0 to 1.0
	error        string          // error message if status == "error"
	result       *UploadResponse // final result if status == "complete"
	data         []byte          // file data to upload
	size         int64           // file size
	createdAt    time.Time       // when the upload started
	uploaderIP   string          // IP address of uploader (for rate limiting)
}

// backgroundUploads stores all in-progress background uploads.
var backgroundUploads = sync.Map{}

// WebhookHandler is an interface for handling webhook callbacks.
type WebhookHandler interface {
	HandleWebhook(body []byte, headers http.Header) error
}

// Handler handles HTTP requests.
type Handler struct {
	files          *files.Service
	payments       *payments.Service
	webhookHandler WebhookHandler
	pendingLimiter *PendingFileLimiter
	mux            *http.ServeMux
}

// NewHandler creates a new HTTP handler.
// If pendingLimiter is nil, no pending file limit is enforced.
func NewHandler(files *files.Service, payments *payments.Service, pendingLimiter *PendingFileLimiter) *Handler {
	h := &Handler{
		files:          files,
		payments:       payments,
		pendingLimiter: pendingLimiter,
		mux:            http.NewServeMux(),
	}
	h.registerRoutes()
	return h
}

// SetWebhookHandler sets the webhook handler for payment notifications.
func (h *Handler) SetWebhookHandler(wh WebhookHandler) {
	h.webhookHandler = wh
}

func (h *Handler) registerRoutes() {
	h.mux.HandleFunc("POST /api/upload", h.handleUpload)
	h.mux.HandleFunc("GET /api/upload/{id}/progress", h.handleUploadProgress)
	h.mux.HandleFunc("GET /api/file/{id}", h.handleDownload)
	h.mux.HandleFunc("HEAD /api/file/{id}", h.handleDownload)
	h.mux.HandleFunc("GET /api/file/{id}/status", h.handleStatus)
	h.mux.HandleFunc("GET /api/file/{id}/invoice", h.handleGetInvoice)
	h.mux.HandleFunc("POST /api/webhook/alby", h.handleAlbyWebhook)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func isValidFileID(id string) bool {
	return id != "" && len(id) <= 64 && validFileIDPattern.MatchString(id)
}

// UploadResponse is the final response for completed file upload.
type UploadResponse struct {
	FileID         string `json:"file_id"`
	Size           int64  `json:"size"`
	PaymentRequest string `json:"payment_request"`
	PaymentHash    string `json:"payment_hash"`
	AmountSats     int64  `json:"amount_sats"`
}

// UploadStartResponse is returned immediately when upload to server completes.
type UploadStartResponse struct {
	UploadID string `json:"upload_id"`
	Size     int64  `json:"size"`
}

// UploadProgressResponse is returned when polling for upload progress.
type UploadProgressResponse struct {
	Status   string          `json:"status"`             // "uploading", "complete", "error"
	Progress float64         `json:"progress"`           // 0.0 to 1.0
	Error    string          `json:"error,omitempty"`    // error message if status == "error"
	Result   *UploadResponse `json:"result,omitempty"`   // final result if status == "complete"
}

// MaxUploadSize is the maximum allowed file size (5GB).
const MaxUploadSize = 5 << 30

func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request) {
	// Extract client IP for rate limiting
	ip := extractIP(r)

	// Check pending file limit before accepting upload
	if h.pendingLimiter != nil && !h.pendingLimiter.CanUpload(ip) {
		count := h.pendingLimiter.PendingCount(ip)
		max := h.pendingLimiter.MaxPending()
		msg := fmt.Sprintf("pending file limit reached: you have %d unpaid file(s) (max %d). "+
			"Please pay for or wait for existing files to expire before uploading more.", count, max)
		http.Error(w, msg, http.StatusTooManyRequests)
		return
	}

	// Limit upload size to 5GB
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize)

	// Parse multipart form (use 32MB for memory, rest goes to disk)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "file too large (max 5GB)", http.StatusRequestEntityTooLarge)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read file into memory buffer for background upload
	var buf bytes.Buffer
	fileSize, err := io.Copy(&buf, file)
	if err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	logging.Internal.Printf("upload: received %d bytes", fileSize)

	// Generate upload ID
	uploadIDBytes := make([]byte, 16)
	if _, err := rand.Read(uploadIDBytes); err != nil {
		http.Error(w, "failed to generate upload ID", http.StatusInternalServerError)
		return
	}
	uploadID := hex.EncodeToString(uploadIDBytes)

	// Create background upload tracker
	upload := &backgroundUpload{
		status:     "uploading",
		progress:   0,
		data:       buf.Bytes(),
		size:       fileSize,
		createdAt:  time.Now(),
		uploaderIP: ip,
	}
	backgroundUploads.Store(uploadID, upload)

	// Start background goroutine to upload to B2
	go h.processBackgroundUpload(uploadID, upload)

	// Return immediately with upload ID
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(UploadStartResponse{
		UploadID: uploadID,
		Size:     fileSize,
	}); err != nil {
		logging.Internal.Printf("failed to encode response: %v", err)
	}
}

// processBackgroundUpload handles the B2 upload in the background.
func (h *Handler) processBackgroundUpload(uploadID string, upload *backgroundUpload) {
	ctx := context.Background()

	// Track progress
	onProgress := func(written, total int64) {
		if total > 0 {
			upload.mu.Lock()
			upload.progress = float64(written) / float64(total)
			upload.mu.Unlock()
		}
	}

	// Upload to B2
	reader := bytes.NewReader(upload.data)
	result, err := h.files.UploadWithProgress(ctx, reader, upload.size, 7*24*time.Hour, onProgress)
	if err != nil {
		logging.Internal.Printf("background upload failed: %v", err)
		upload.mu.Lock()
		upload.status = "error"
		upload.error = "upload failed"
		upload.data = nil // Free memory
		upload.mu.Unlock()
		return
	}

	// Calculate price: 1 sat per MB, minimum 100 sats
	amountSats := result.Size / (1024 * 1024)
	if amountSats < 100 {
		amountSats = 100
	}

	// Create payment invoice
	invoice, err := h.payments.CreateInvoiceForFile(ctx, result.ID, amountSats)
	if err != nil {
		logging.Internal.Printf("failed to create invoice: %v", err)
		upload.mu.Lock()
		upload.status = "error"
		upload.error = "failed to create invoice"
		upload.data = nil
		upload.mu.Unlock()
		return
	}

	// Track pending file for rate limiting (using IP stored in upload)
	if h.pendingLimiter != nil && upload.uploaderIP != "" {
		h.pendingLimiter.TrackPendingFile(upload.uploaderIP, result.ID)
	}

	// Mark as complete
	upload.mu.Lock()
	upload.status = "complete"
	upload.progress = 1.0
	upload.result = &UploadResponse{
		FileID:         result.ID,
		Size:           result.Size,
		PaymentRequest: invoice.PaymentRequest,
		PaymentHash:    invoice.PaymentHash,
		AmountSats:     invoice.AmountSats,
	}
	upload.data = nil // Free memory
	upload.mu.Unlock()

	logging.Internal.Printf("background upload complete: %s -> %s", uploadID, result.ID)

	// Schedule cleanup after 5 minutes
	go func() {
		time.Sleep(5 * time.Minute)
		backgroundUploads.Delete(uploadID)
	}()
}

// handleUploadProgress returns the progress of a background upload.
func (h *Handler) handleUploadProgress(w http.ResponseWriter, r *http.Request) {
	uploadID := r.PathValue("id")
	if uploadID == "" {
		http.Error(w, "missing upload ID", http.StatusBadRequest)
		return
	}

	val, ok := backgroundUploads.Load(uploadID)
	if !ok {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}

	upload := val.(*backgroundUpload)
	upload.mu.RLock()
	resp := UploadProgressResponse{
		Status:   upload.status,
		Progress: upload.progress,
		Error:    upload.error,
		Result:   upload.result,
	}
	upload.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logging.Internal.Printf("failed to encode response: %v", err)
	}
}

func (h *Handler) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidFileID(id) {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}

	// Get seekable reader (supports Range requests)
	reader, err := h.files.DownloadSeekable(r.Context(), id)
	if err == files.ErrNotPaid {
		http.Error(w, "payment required", http.StatusPaymentRequired)
		return
	}
	if err == files.ErrExpired {
		http.Error(w, "file expired", http.StatusGone)
		return
	}
	if err == store.ErrNotFound {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "download failed", http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	// Get metadata for modification time (used by ServeContent for caching)
	meta, _ := h.files.GetMetadata(r.Context(), id)
	modTime := time.Time{}
	if meta != nil {
		modTime = meta.CreatedAt
	}

	// ServeContent handles Range requests, Content-Length, and HEAD automatically
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", modTime, reader)
}

// StatusResponse is the response for file status check.
type StatusResponse struct {
	Paid      bool      `json:"paid"`
	ExpiresAt time.Time `json:"expires_at"`
	Size      int64     `json:"size"`
	DirectURL string    `json:"direct_url,omitempty"` // Direct download URL (if available and paid)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidFileID(id) {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}

	meta, err := h.files.GetMetadata(r.Context(), id)
	if err == store.ErrNotFound {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to get status", http.StatusInternalServerError)
		return
	}

	resp := StatusResponse{
		Paid:      meta.Paid,
		ExpiresAt: meta.ExpiresAt,
		Size:      meta.Size,
	}

	// Include direct download URL if file is paid and direct access is available
	if meta.Paid {
		if directURL := h.files.GetDirectURL(id); directURL != "" {
			resp.DirectURL = directURL
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logging.Internal.Printf("failed to encode response: %v", err)
	}
}

// InvoiceResponse is the response for invoice retrieval.
type InvoiceResponse struct {
	PaymentRequest string `json:"payment_request"`
	PaymentHash    string `json:"payment_hash"`
	AmountSats     int64  `json:"amount_sats"`
}

func (h *Handler) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidFileID(id) {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}

	pending, err := h.payments.GetInvoiceForFile(id)
	if err == payments.ErrInvoiceNotFound {
		http.Error(w, "invoice not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to get invoice", http.StatusInternalServerError)
		return
	}

	resp := InvoiceResponse{
		PaymentRequest: pending.Invoice.PaymentRequest,
		PaymentHash:    pending.Invoice.PaymentHash,
		AmountSats:     pending.Invoice.AmountSats,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logging.Internal.Printf("failed to encode response: %v", err)
	}
}

func (h *Handler) handleAlbyWebhook(w http.ResponseWriter, r *http.Request) {
	if h.webhookHandler == nil {
		http.Error(w, "webhook handler not configured", http.StatusServiceUnavailable)
		return
	}

	// Read raw body for signature verification
	body, err := io.ReadAll(r.Body)
	if err != nil {
		logging.Internal.Printf("webhook: failed to read body: %v", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	if err := h.webhookHandler.HandleWebhook(body, r.Header); err != nil {
		logging.Internal.Printf("webhook: failed to process: %v", err)
		http.Error(w, "webhook processing failed", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}
