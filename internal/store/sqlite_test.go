package store

import (
	"context"
	"testing"
	"time"
)

func TestSQLiteStore(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	t.Run("SaveAndGet", func(t *testing.T) {
		meta := &FileMeta{
			ID:           "test-file-1",
			Size:         1024,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
			HostDuration: 24 * time.Hour,
			Paid:         false,
			CreatedAt:    time.Now(),
		}

		if err := store.SaveFileMetadata(ctx, meta); err != nil {
			t.Fatalf("failed to save: %v", err)
		}

		got, err := store.GetFileMetadata(ctx, "test-file-1")
		if err != nil {
			t.Fatalf("failed to get: %v", err)
		}

		if got.ID != meta.ID || got.Size != meta.Size || got.Paid != meta.Paid {
			t.Errorf("got %+v, want %+v", got, meta)
		}
		if got.HostDuration != meta.HostDuration {
			t.Errorf("got HostDuration %v, want %v", got.HostDuration, meta.HostDuration)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := store.GetFileMetadata(ctx, "nonexistent")
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("UpdatePaymentStatus", func(t *testing.T) {
		initialExpiry := time.Now().Add(1 * time.Hour)
		hostDuration := 7 * 24 * time.Hour // 7 days
		meta := &FileMeta{
			ID:           "test-file-2",
			Size:         2048,
			ExpiresAt:    initialExpiry,
			HostDuration: hostDuration,
			Paid:         false,
			CreatedAt:    time.Now(),
		}
		store.SaveFileMetadata(ctx, meta)

		beforeUpdate := time.Now()
		if err := store.UpdatePaymentStatus(ctx, "test-file-2", true); err != nil {
			t.Fatalf("failed to update: %v", err)
		}

		got, _ := store.GetFileMetadata(ctx, "test-file-2")
		if !got.Paid {
			t.Error("expected Paid to be true")
		}

		// Verify expiration was extended to approximately now + hostDuration
		expectedExpiry := beforeUpdate.Add(hostDuration)
		// Allow 1 minute tolerance for test execution time
		if got.ExpiresAt.Before(expectedExpiry.Add(-1*time.Minute)) || got.ExpiresAt.After(expectedExpiry.Add(1*time.Minute)) {
			t.Errorf("expected ExpiresAt around %v, got %v", expectedExpiry, got.ExpiresAt)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		meta := &FileMeta{
			ID:           "test-file-3",
			Size:         512,
			ExpiresAt:    time.Now().Add(24 * time.Hour),
			HostDuration: 24 * time.Hour,
			Paid:         true,
			CreatedAt:    time.Now(),
		}
		store.SaveFileMetadata(ctx, meta)

		if err := store.DeleteFileMetadata(ctx, "test-file-3"); err != nil {
			t.Fatalf("failed to delete: %v", err)
		}

		_, err := store.GetFileMetadata(ctx, "test-file-3")
		if err != ErrNotFound {
			t.Errorf("expected ErrNotFound after delete, got %v", err)
		}
	})

	t.Run("ListExpired", func(t *testing.T) {
		// Add an expired file
		expired := &FileMeta{
			ID:           "expired-file",
			Size:         100,
			ExpiresAt:    time.Now().Add(-1 * time.Hour),
			HostDuration: 24 * time.Hour,
			Paid:         true,
			CreatedAt:    time.Now().Add(-25 * time.Hour),
		}
		store.SaveFileMetadata(ctx, expired)

		// Add a non-expired file
		valid := &FileMeta{
			ID:           "valid-file",
			Size:         100,
			ExpiresAt:    time.Now().Add(24 * time.Hour),
			HostDuration: 24 * time.Hour,
			Paid:         true,
			CreatedAt:    time.Now(),
		}
		store.SaveFileMetadata(ctx, valid)

		files, err := store.ListExpiredFiles(ctx)
		if err != nil {
			t.Fatalf("failed to list expired: %v", err)
		}

		found := false
		for _, f := range files {
			if f.ID == "expired-file" {
				found = true
			}
			if f.ID == "valid-file" {
				t.Error("valid-file should not be in expired list")
			}
		}
		if !found {
			t.Error("expired-file should be in expired list")
		}
	})
}

func TestSQLiteStore_PendingInvoices(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	t.Run("SaveAndList", func(t *testing.T) {
		inv := &PendingInvoice{
			PaymentHash:    "abc123hash",
			FileID:         "file-xyz",
			PaymentRequest: "lnbc1000...",
			AmountSats:     1000,
			CreatedAt:      time.Now(),
		}

		if err := store.SavePendingInvoice(ctx, inv); err != nil {
			t.Fatalf("failed to save invoice: %v", err)
		}

		invoices, err := store.ListPendingInvoices(ctx)
		if err != nil {
			t.Fatalf("failed to list invoices: %v", err)
		}

		if len(invoices) != 1 {
			t.Fatalf("expected 1 invoice, got %d", len(invoices))
		}

		got := invoices[0]
		if got.PaymentHash != inv.PaymentHash {
			t.Errorf("PaymentHash = %s, want %s", got.PaymentHash, inv.PaymentHash)
		}
		if got.FileID != inv.FileID {
			t.Errorf("FileID = %s, want %s", got.FileID, inv.FileID)
		}
		if got.PaymentRequest != inv.PaymentRequest {
			t.Errorf("PaymentRequest = %s, want %s", got.PaymentRequest, inv.PaymentRequest)
		}
		if got.AmountSats != inv.AmountSats {
			t.Errorf("AmountSats = %d, want %d", got.AmountSats, inv.AmountSats)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		// Invoice from previous test should exist
		if err := store.DeletePendingInvoice(ctx, "abc123hash"); err != nil {
			t.Fatalf("failed to delete invoice: %v", err)
		}

		invoices, err := store.ListPendingInvoices(ctx)
		if err != nil {
			t.Fatalf("failed to list invoices: %v", err)
		}

		if len(invoices) != 0 {
			t.Errorf("expected 0 invoices after delete, got %d", len(invoices))
		}
	})

	t.Run("SaveOrReplace", func(t *testing.T) {
		inv1 := &PendingInvoice{
			PaymentHash:    "hash-replace",
			FileID:         "file-1",
			PaymentRequest: "lnbc1...",
			AmountSats:     500,
			CreatedAt:      time.Now(),
		}
		store.SavePendingInvoice(ctx, inv1)

		// Save again with same hash but different data
		inv2 := &PendingInvoice{
			PaymentHash:    "hash-replace",
			FileID:         "file-2",
			PaymentRequest: "lnbc2...",
			AmountSats:     1000,
			CreatedAt:      time.Now(),
		}
		if err := store.SavePendingInvoice(ctx, inv2); err != nil {
			t.Fatalf("failed to replace invoice: %v", err)
		}

		invoices, _ := store.ListPendingInvoices(ctx)
		found := false
		for _, inv := range invoices {
			if inv.PaymentHash == "hash-replace" {
				found = true
				if inv.FileID != "file-2" {
					t.Errorf("expected replaced FileID file-2, got %s", inv.FileID)
				}
			}
		}
		if !found {
			t.Error("replaced invoice not found")
		}
	})
}

func TestSQLiteStore_GetStats(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	t.Run("empty database", func(t *testing.T) {
		stats, err := store.GetStats(ctx)
		if err != nil {
			t.Fatalf("GetStats failed: %v", err)
		}

		if stats.TotalFiles != 0 {
			t.Errorf("expected 0 total files, got %d", stats.TotalFiles)
		}
		if stats.TotalBytes != 0 {
			t.Errorf("expected 0 total bytes, got %d", stats.TotalBytes)
		}
	})

	t.Run("with files", func(t *testing.T) {
		// Add paid file
		paid := &FileMeta{
			ID:           "stats-paid-file",
			Size:         1024,
			ExpiresAt:    time.Now().Add(24 * time.Hour),
			HostDuration: 24 * time.Hour,
			Paid:         true,
			CreatedAt:    time.Now().Add(-1 * time.Hour),
		}
		store.SaveFileMetadata(ctx, paid)

		// Add pending file
		pending := &FileMeta{
			ID:           "stats-pending-file",
			Size:         2048,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
			HostDuration: 24 * time.Hour,
			Paid:         false,
			CreatedAt:    time.Now(),
		}
		store.SaveFileMetadata(ctx, pending)

		stats, err := store.GetStats(ctx)
		if err != nil {
			t.Fatalf("GetStats failed: %v", err)
		}

		if stats.TotalFiles != 2 {
			t.Errorf("expected 2 total files, got %d", stats.TotalFiles)
		}
		if stats.PaidFiles != 1 {
			t.Errorf("expected 1 paid file, got %d", stats.PaidFiles)
		}
		if stats.PendingFiles != 1 {
			t.Errorf("expected 1 pending file, got %d", stats.PendingFiles)
		}
		if stats.TotalBytes != 1024+2048 {
			t.Errorf("expected %d total bytes, got %d", 1024+2048, stats.TotalBytes)
		}
		if stats.PaidBytes != 1024 {
			t.Errorf("expected %d paid bytes, got %d", 1024, stats.PaidBytes)
		}
		if stats.PendingBytes != 2048 {
			t.Errorf("expected %d pending bytes, got %d", 2048, stats.PendingBytes)
		}
		if stats.OldestFile.IsZero() {
			t.Error("expected OldestFile to be set")
		}
		if stats.NewestFile.IsZero() {
			t.Error("expected NewestFile to be set")
		}
	})
}
