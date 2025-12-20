package payments

import (
	"context"
)

// Invoice represents a Lightning Network invoice.
type Invoice struct {
	PaymentHash    string
	PaymentRequest string // BOLT11 encoded invoice
	AmountSats     int64
}

// InvoiceUpdate represents a payment status update.
type InvoiceUpdate struct {
	PaymentHash string
	Settled     bool
}

// LNDClient defines the interface for Lightning Network operations.
type LNDClient interface {
	CreateInvoice(ctx context.Context, amountSats int64, memo string) (*Invoice, error)
	SubscribeInvoices(ctx context.Context) (<-chan InvoiceUpdate, error)
	Close() error
}
