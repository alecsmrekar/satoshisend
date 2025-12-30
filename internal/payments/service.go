package payments

import (
	"context"
	"errors"
	"sync"

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

	return nil
}

func (s *Service) handlePayment(ctx context.Context, paymentHash string) {
	s.mu.Lock()
	pending, ok := s.pending[paymentHash]
	cb := s.onPayment
	if ok {
		delete(s.pending, paymentHash)
		delete(s.byFileID, pending.FileID)
	}
	s.mu.Unlock()

	if ok {
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
}
