package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"satoshisend/internal/store"
)

// mockStore implements store.Store for testing.
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
	meta, ok := m.files[fileID]
	if !ok {
		return store.ErrNotFound
	}
	meta.Paid = paid
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

func TestService_CreateInvoice(t *testing.T) {
	lnd := NewMockLNDClient()
	st := newMockStore()
	svc := NewService(lnd, st)

	ctx := context.Background()

	inv, err := svc.CreateInvoiceForFile(ctx, "test-file-id-1234", 1000)
	if err != nil {
		t.Fatalf("create invoice failed: %v", err)
	}

	if inv.PaymentHash == "" {
		t.Error("expected non-empty payment hash")
	}
	if inv.AmountSats != 1000 {
		t.Errorf("expected 1000 sats, got %d", inv.AmountSats)
	}

	// Should be able to retrieve the pending invoice
	pending, err := svc.GetInvoiceForFile("test-file-id-1234")
	if err != nil {
		t.Fatalf("get invoice failed: %v", err)
	}
	if pending.PaymentHash != inv.PaymentHash {
		t.Error("payment hash mismatch")
	}
}

func TestService_InvoicePersistence(t *testing.T) {
	lnd := NewMockLNDClient()
	st := newMockStore()
	svc := NewService(lnd, st)

	ctx := context.Background()

	// Create an invoice - should be persisted
	inv, err := svc.CreateInvoiceForFile(ctx, "test-persist-file", 500)
	if err != nil {
		t.Fatalf("create invoice failed: %v", err)
	}

	// Check it was persisted to the store
	if len(st.invoices) != 1 {
		t.Fatalf("expected 1 invoice in store, got %d", len(st.invoices))
	}

	stored := st.invoices[inv.PaymentHash]
	if stored == nil {
		t.Fatal("invoice not found in store")
	}
	if stored.FileID != "test-persist-file" {
		t.Errorf("stored FileID = %s, want test-persist-file", stored.FileID)
	}
	if stored.AmountSats != 500 {
		t.Errorf("stored AmountSats = %d, want 500", stored.AmountSats)
	}
}

func TestService_LoadPendingInvoices(t *testing.T) {
	lnd := NewMockLNDClient()
	st := newMockStore()

	// Pre-populate the store with a pending invoice (simulating restart)
	st.invoices["preexisting-hash"] = &store.PendingInvoice{
		PaymentHash:    "preexisting-hash",
		FileID:         "preexisting-file",
		PaymentRequest: "lnbc500...",
		AmountSats:     500,
		CreatedAt:      time.Now(),
	}

	svc := NewService(lnd, st)
	ctx := context.Background()

	// Load pending invoices (as would happen on startup)
	if err := svc.LoadPendingInvoices(ctx); err != nil {
		t.Fatalf("LoadPendingInvoices failed: %v", err)
	}

	// Should be able to retrieve the loaded invoice
	pending, err := svc.GetInvoiceForFile("preexisting-file")
	if err != nil {
		t.Fatalf("GetInvoiceForFile failed: %v", err)
	}

	if pending.PaymentHash != "preexisting-hash" {
		t.Errorf("PaymentHash = %s, want preexisting-hash", pending.PaymentHash)
	}
	if pending.Invoice.AmountSats != 500 {
		t.Errorf("AmountSats = %d, want 500", pending.Invoice.AmountSats)
	}
}

func TestService_PaymentDeletesInvoice(t *testing.T) {
	lnd := NewMockLNDClient()
	st := newMockStore()
	svc := NewService(lnd, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a file in the store
	fileID := "test-delete-invoice"
	st.SaveFileMetadata(ctx, &store.FileMeta{
		ID:        fileID,
		Size:      1024,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Paid:      false,
		CreatedAt: time.Now(),
	})

	// Start payment watcher
	if err := svc.StartPaymentWatcher(ctx); err != nil {
		t.Fatalf("start watcher failed: %v", err)
	}

	// Create invoice
	inv, _ := svc.CreateInvoiceForFile(ctx, fileID, 500)

	// Verify invoice is in store
	if len(st.invoices) != 1 {
		t.Fatalf("expected 1 invoice in store before payment, got %d", len(st.invoices))
	}

	// Simulate payment
	lnd.SimulatePayment(inv.PaymentHash)

	// Give the goroutine time to process
	time.Sleep(50 * time.Millisecond)

	// Invoice should be deleted from store
	if len(st.invoices) != 0 {
		t.Errorf("expected 0 invoices in store after payment, got %d", len(st.invoices))
	}
}

func TestService_PaymentWatcher(t *testing.T) {
	lnd := NewMockLNDClient()
	st := newMockStore()
	svc := NewService(lnd, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a file in the store
	fileID := "test-file-id-5678"
	st.SaveFileMetadata(ctx, &store.FileMeta{
		ID:        fileID,
		Size:      1024,
		ExpiresAt: time.Now().Add(24 * time.Hour),
		Paid:      false,
		CreatedAt: time.Now(),
	})

	// Start payment watcher
	if err := svc.StartPaymentWatcher(ctx); err != nil {
		t.Fatalf("start watcher failed: %v", err)
	}

	// Create invoice
	inv, _ := svc.CreateInvoiceForFile(ctx, fileID, 500)

	// Simulate payment
	lnd.SimulatePayment(inv.PaymentHash)

	// Give the goroutine time to process
	time.Sleep(50 * time.Millisecond)

	// File should now be marked as paid
	meta, _ := st.GetFileMetadata(ctx, fileID)
	if !meta.Paid {
		t.Error("expected file to be marked as paid")
	}

	// Pending invoice should be cleared
	_, err := svc.GetInvoiceForFile(fileID)
	if err != ErrInvoiceNotFound {
		t.Error("expected pending invoice to be cleared")
	}
}

// scanLND returns fixed scan results, to simulate a lost notification or a failed scan.
type scanLND struct {
	*MockLNDClient
	settled []string
	err     error
}

func (f *scanLND) ListSettled(ctx context.Context, since time.Time) ([]string, error) {
	return f.settled, f.err
}

func TestService_ScanFindsMissedPayment(t *testing.T) {
	lnd := &scanLND{MockLNDClient: NewMockLNDClient()}
	st := newMockStore()
	svc := NewService(lnd, st)
	ctx := context.Background()

	fileID := "test-scan-file"
	st.SaveFileMetadata(ctx, &store.FileMeta{
		ID:        fileID,
		Size:      1024,
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	})

	inv, err := svc.CreateInvoiceForFile(ctx, fileID, 500)
	if err != nil {
		t.Fatalf("create invoice failed: %v", err)
	}

	lnd.settled = []string{strings.Repeat("0", 64), inv.PaymentHash}
	svc.scan(ctx)

	meta, _ := st.GetFileMetadata(ctx, fileID)
	if !meta.Paid {
		t.Error("expected file to be marked as paid")
	}
	if len(st.invoices) != 0 {
		t.Errorf("expected 0 invoices in store after scan, got %d", len(st.invoices))
	}
}

func TestService_ScanRemovesExpiredInvoices(t *testing.T) {
	lnd := &scanLND{MockLNDClient: NewMockLNDClient()}
	st := newMockStore()
	expiredHash := strings.Repeat("e", 64)
	recentHash := strings.Repeat("f", 64)

	// Invoices expire after 10 minutes. The scan waits 15 more minutes before it removes one.
	st.invoices[expiredHash] = &store.PendingInvoice{
		PaymentHash: expiredHash,
		FileID:      "expired-file",
		CreatedAt:   time.Now().Add(-26 * time.Minute),
	}
	st.invoices[recentHash] = &store.PendingInvoice{
		PaymentHash: recentHash,
		FileID:      "recent-file",
		CreatedAt:   time.Now().Add(-24 * time.Minute),
	}

	svc := NewService(lnd, st)
	ctx := context.Background()
	if err := svc.LoadPendingInvoices(ctx); err != nil {
		t.Fatalf("LoadPendingInvoices failed: %v", err)
	}

	lnd.err = errors.New("relay unavailable")
	svc.scan(ctx)
	if len(st.invoices) != 2 {
		t.Fatalf("a failed scan removed invoices: %d left, want 2", len(st.invoices))
	}

	lnd.err = nil
	svc.scan(ctx)
	if _, ok := st.invoices[expiredHash]; ok {
		t.Error("expected expired invoice to be removed from store")
	}
	if _, err := svc.GetInvoiceForFile("expired-file"); err != ErrInvoiceNotFound {
		t.Error("expected expired invoice to be removed from memory")
	}
	if _, ok := st.invoices[recentHash]; !ok {
		t.Error("expected recent invoice to stay in store")
	}
	if _, err := svc.GetInvoiceForFile("recent-file"); err != nil {
		t.Error("expected recent invoice to stay in memory")
	}
}
