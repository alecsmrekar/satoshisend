package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"satoshisend/internal/files"
	"satoshisend/internal/logging"
	"satoshisend/internal/payments"
	"satoshisend/internal/store"
)

var validFileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

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
	h.mux.HandleFunc("POST /api/upload/init", h.handleUploadInit)
	h.mux.HandleFunc("POST /api/upload/complete", h.handleUploadComplete)
	h.mux.HandleFunc("PUT /api/upload/{id}", h.handleUploadStream)
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

// UploadInitRequest is the request body for initiating a direct upload.
type UploadInitRequest struct {
	Size int64 `json:"size"`
}

// UploadInitResponse is returned when initiating an upload.
type UploadInitResponse struct {
	FileID string `json:"file_id"`
}

// UploadCompleteRequest is the request body for completing an upload.
type UploadCompleteRequest struct {
	FileID string `json:"file_id"`
	Size   int64  `json:"size"`
}

// UploadCompleteResponse is the response after completing an upload.
type UploadCompleteResponse struct {
	FileID         string `json:"file_id"`
	Size           int64  `json:"size"`
	PaymentRequest string `json:"payment_request"`
	PaymentHash    string `json:"payment_hash"`
	AmountSats     int64  `json:"amount_sats"`
}

// MaxUploadSize is the maximum allowed file size (5GB).
const MaxUploadSize = 5 << 30

func (h *Handler) handleUploadInit(w http.ResponseWriter, r *http.Request) {
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

	var req UploadInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate size
	if req.Size <= 0 {
		http.Error(w, "size must be positive", http.StatusBadRequest)
		return
	}
	if req.Size > MaxUploadSize {
		http.Error(w, "file too large (max 5GB)", http.StatusRequestEntityTooLarge)
		return
	}

	// Get presigned URL from file service
	result, err := h.files.InitUpload(r.Context())
	if err != nil {
		logging.Internal.Printf("failed to init upload: %v", err)
		http.Error(w, "failed to initialize upload", http.StatusInternalServerError)
		return
	}

	logging.Internal.Printf("upload init: file_id=%s, size=%d", result.ID, req.Size)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(UploadInitResponse{
		FileID: result.ID,
	}); err != nil {
		logging.Internal.Printf("failed to encode response: %v", err)
	}
}

func (h *Handler) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	// Extract client IP for rate limiting
	ip := extractIP(r)

	var req UploadCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate file ID
	if !isValidFileID(req.FileID) {
		http.Error(w, "invalid file ID", http.StatusBadRequest)
		return
	}

	// Validate size
	if req.Size <= 0 {
		http.Error(w, "size must be positive", http.StatusBadRequest)
		return
	}

	// Verify upload and create metadata
	result, err := h.files.CompleteUpload(r.Context(), req.FileID, req.Size, 7*24*time.Hour)
	if err != nil {
		logging.Internal.Printf("failed to complete upload: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Calculate price: 1 sat per MB, minimum 100 sats
	amountSats := result.Size / (1024 * 1024)
	if amountSats < 100 {
		amountSats = 100
	}

	// Create payment invoice
	invoice, err := h.payments.CreateInvoiceForFile(r.Context(), result.ID, amountSats)
	if err != nil {
		logging.Internal.Printf("failed to create invoice: %v", err)
		http.Error(w, "failed to create invoice", http.StatusInternalServerError)
		return
	}

	// Track pending file for rate limiting
	if h.pendingLimiter != nil && ip != "" {
		h.pendingLimiter.TrackPendingFile(ip, result.ID)
	}

	logging.Internal.Printf("upload complete: file_id=%s, size=%d, amount=%d sats", result.ID, result.Size, amountSats)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(UploadCompleteResponse{
		FileID:         result.ID,
		Size:           result.Size,
		PaymentRequest: invoice.PaymentRequest,
		PaymentHash:    invoice.PaymentHash,
		AmountSats:     invoice.AmountSats,
	}); err != nil {
		logging.Internal.Printf("failed to encode response: %v", err)
	}
}

// handleUploadStream handles streaming uploads directly to storage.
// This is a fallback for when direct-to-storage uploads don't work (e.g., B2 CORS issues).
// The client should call /api/upload/init first to get a file ID, then PUT the data here.
func (h *Handler) handleUploadStream(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("id")
	if !isValidFileID(fileID) {
		http.Error(w, "invalid file ID", http.StatusBadRequest)
		return
	}

	// Get content length
	contentLength := r.ContentLength
	if contentLength <= 0 {
		http.Error(w, "Content-Length header required", http.StatusBadRequest)
		return
	}
	if contentLength > MaxUploadSize {
		http.Error(w, "file too large (max 5GB)", http.StatusRequestEntityTooLarge)
		return
	}

	// Limit body size
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize)

	// Stream directly to storage (no temp file)
	size, err := h.files.UploadWithID(r.Context(), fileID, r.Body, contentLength, 7*24*time.Hour)
	if err != nil {
		logging.Internal.Printf("stream upload failed for %s: %v", fileID, err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	logging.Internal.Printf("stream upload complete: file_id=%s, size=%d", fileID, size)
	w.WriteHeader(http.StatusOK)
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
