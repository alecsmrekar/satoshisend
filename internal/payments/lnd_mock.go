package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// MockLNDClient implements LNDClient for testing and development.
type MockLNDClient struct {
	mu       sync.Mutex
	invoices map[string]*Invoice
	updates  chan InvoiceUpdate
}

// NewMockLNDClient creates a new mock LND client.
func NewMockLNDClient() *MockLNDClient {
	return &MockLNDClient{
		invoices: make(map[string]*Invoice),
		updates:  make(chan InvoiceUpdate, 100),
	}
}

func (m *MockLNDClient) CreateInvoice(ctx context.Context, amountSats int64, memo string) (*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hash, err := generatePaymentHash()
	if err != nil {
		return nil, err
	}

	inv := &Invoice{
		PaymentHash:    hash,
		PaymentRequest: "lnbc" + hash[:20], // Fake BOLT11
		AmountSats:     amountSats,
	}
	m.invoices[hash] = inv

	// Auto-settle after 20 seconds (for development/testing)
	go func() {
		time.Sleep(20 * time.Second)
		log.Printf("Mock: auto-settling invoice %s", hash[:8])
		m.SimulatePayment(hash)
	}()

	return inv, nil
}

func (m *MockLNDClient) SubscribeInvoices(ctx context.Context) (<-chan InvoiceUpdate, error) {
	return m.updates, nil
}

// SimulatePayment simulates a payment being received (for testing).
func (m *MockLNDClient) SimulatePayment(paymentHash string) {
	m.updates <- InvoiceUpdate{
		PaymentHash: paymentHash,
		Settled:     true,
	}
}

func (m *MockLNDClient) Close() error {
	close(m.updates)
	return nil
}

func generatePaymentHash() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
