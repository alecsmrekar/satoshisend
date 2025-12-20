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

	mu       sync.RWMutex
	pending  map[string]*PendingInvoice // keyed by payment hash
	byFileID map[string]*PendingInvoice // keyed by file ID
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
	if ok {
		delete(s.pending, paymentHash)
		delete(s.byFileID, pending.FileID)
	}
	s.mu.Unlock()

	if ok {
		if err := s.store.UpdatePaymentStatus(ctx, pending.FileID, true); err != nil {
			logging.Internal.Printf("CRITICAL: failed to mark file %s as paid after receiving payment: %v", pending.FileID, err)
		}
	}
}
