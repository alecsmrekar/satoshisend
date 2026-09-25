package payments

import (
	"context"
	"errors"
	"sync"
	"time"

	"satoshisend/internal/logging"
	"satoshisend/internal/store"
)

var (
	ErrInvoiceNotFound = errors.New("invoice not found")
)

// PaymentCallback is called when a payment is received for a file.
type PaymentCallback func(fileID string)

// PendingInvoice tracks an invoice waiting for payment.
type PendingInvoice struct {
	FileID      string
	PaymentHash string
	Invoice     *Invoice
	CreatedAt   time.Time
}

// Service handles payment operations.
type Service struct {
	lnd   LNDClient
	store store.Store

	mu        sync.RWMutex
	pending   map[string]*PendingInvoice // keyed by payment hash
	byFileID  map[string]*PendingInvoice // keyed by file ID
	onPayment PaymentCallback            // optional callback when payment received
}

// NewService creates a new payment service.
func NewService(lnd LNDClient, st store.Store) *Service {
	return &Service{
		lnd:      lnd,
		store:    st,
		pending:  make(map[string]*PendingInvoice),
		byFileID: make(map[string]*PendingInvoice),
	}
}

// CreateInvoiceForFile creates a Lightning invoice for hosting a file.
func (s *Service) CreateInvoiceForFile(ctx context.Context, fileID string, amountSats int64) (*Invoice, error) {
	memo := "SatoshiSend file hosting: " + fileID[:8]

	inv, err := s.lnd.CreateInvoice(ctx, amountSats, memo)
	if err != nil {
		return nil, err
	}

	pending := &PendingInvoice{
		FileID:      fileID,
		PaymentHash: inv.PaymentHash,
		Invoice:     inv,
		CreatedAt:   time.Now(),
	}

	// Persist to database for restart recovery
	storeInv := &store.PendingInvoice{
		PaymentHash:    inv.PaymentHash,
		FileID:         fileID,
		PaymentRequest: inv.PaymentRequest,
		AmountSats:     amountSats,
		CreatedAt:      pending.CreatedAt,
	}
	if err := s.store.SavePendingInvoice(ctx, storeInv); err != nil {
		logging.Internal.Printf("failed to persist invoice %s: %v", inv.PaymentHash[:16], err)
		// Continue anyway - in-memory tracking still works
	}

	s.mu.Lock()
	s.pending[inv.PaymentHash] = pending
	s.byFileID[fileID] = pending
	s.mu.Unlock()

	return inv, nil
}

// GetInvoiceForFile returns the pending invoice for a file.
func (s *Service) GetInvoiceForFile(fileID string) (*PendingInvoice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pending, ok := s.byFileID[fileID]
	if !ok {
		return nil, ErrInvoiceNotFound
	}
	return pending, nil
}

// SetPaymentCallback sets a callback function that will be called when a
// payment is received. This allows external components (like rate limiters)
// to be notified of payments.
func (s *Service) SetPaymentCallback(cb PaymentCallback) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onPayment = cb
}

// StartPaymentWatcher starts watching for invoice payments.
// It marks files as paid when their invoices are settled.
// It also scans the wallet at start and every 30 seconds, to find payments whose notification was lost.
func (s *Service) StartPaymentWatcher(ctx context.Context) error {
	updates, err := s.lnd.SubscribeInvoices(ctx)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				if update.Settled {
					s.handlePayment(ctx, update.PaymentHash)
				}
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			s.scan(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return nil
}

// scan asks the wallet for settled payments since the oldest pending invoice.
// After a successful scan, it removes each unpaid invoice that expired more than 15 minutes before the scan started.
// The 15 minutes cover a payment that was in flight when its invoice expired.
func (s *Service) scan(ctx context.Context) {
	s.mu.RLock()
	var oldest time.Time
	for _, p := range s.pending {
		if oldest.IsZero() || p.CreatedAt.Before(oldest) {
			oldest = p.CreatedAt
		}
	}
	s.mu.RUnlock()
	if oldest.IsZero() {
		return
	}

	scanStart := time.Now()
	// The 5 minutes allow for clock skew between this server and the wallet.
	hashes, err := s.lnd.ListSettled(ctx, oldest.Add(-5*time.Minute))
	if err != nil {
		logging.Internal.Printf("wallet scan failed: %v", err)
		return
	}
	for _, hash := range hashes {
		if s.handlePayment(ctx, hash) {
			logging.Internal.Printf("wallet scan found payment %s", hash[:16])
		}
	}

	cutoff := scanStart.Add(-(InvoiceExpiry + 15*time.Minute))
	var expired []*PendingInvoice
	s.mu.Lock()
	for hash, p := range s.pending {
		if p.CreatedAt.Before(cutoff) {
			expired = append(expired, p)
			delete(s.pending, hash)
			delete(s.byFileID, p.FileID)
		}
	}
	s.mu.Unlock()

	for _, p := range expired {
		if err := s.store.DeletePendingInvoice(ctx, p.PaymentHash); err != nil {
			logging.Internal.Printf("failed to delete expired invoice %s: %v", p.PaymentHash[:16], err)
		}
	}
}

// handlePayment marks the file of a pending invoice as paid.
// It returns false if no pending invoice has this payment hash.
func (s *Service) handlePayment(ctx context.Context, paymentHash string) bool {
	s.mu.Lock()
	pending, ok := s.pending[paymentHash]
	cb := s.onPayment
	if ok {
		delete(s.pending, paymentHash)
		delete(s.byFileID, pending.FileID)
	}
	s.mu.Unlock()

	if ok {
		// Remove from persistent storage
		if err := s.store.DeletePendingInvoice(ctx, paymentHash); err != nil {
			logging.Internal.Printf("failed to delete pending invoice %s: %v", paymentHash[:16], err)
		}

		if err := s.store.UpdatePaymentStatus(ctx, pending.FileID, true); err != nil {
			logging.Internal.Printf("CRITICAL: failed to mark file %s as paid after receiving payment: %v", pending.FileID, err)
		}

		// Notify callback (e.g., pending file limiter)
		if cb != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logging.Internal.Printf("payment callback panic for file %s: %v", pending.FileID, r)
					}
				}()
				cb(pending.FileID)
			}()
		}
	}
	return ok
}

// LoadPendingInvoices loads pending invoices from the database into memory.
// This should be called on startup to recover state after a restart.
func (s *Service) LoadPendingInvoices(ctx context.Context) error {
	invoices, err := s.store.ListPendingInvoices(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, inv := range invoices {
		pending := &PendingInvoice{
			FileID:      inv.FileID,
			PaymentHash: inv.PaymentHash,
			Invoice: &Invoice{
				PaymentHash:    inv.PaymentHash,
				PaymentRequest: inv.PaymentRequest,
				AmountSats:     inv.AmountSats,
			},
			CreatedAt: inv.CreatedAt,
		}
		s.pending[inv.PaymentHash] = pending
		s.byFileID[inv.FileID] = pending
	}

	if len(invoices) > 0 {
		logging.Internal.Printf("loaded %d pending invoices from database", len(invoices))
	}

	return nil
}
