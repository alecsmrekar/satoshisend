package api

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPendingFileLimiter_CanUpload(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	// Should allow first 3 uploads
	for i := 0; i < 3; i++ {
		if !limiter.CanUpload(ip) {
			t.Errorf("upload %d should be allowed", i+1)
		}
		limiter.TrackPendingFile(ip, fmt.Sprintf("file%d", i))
	}

	// Should block 4th upload
	if limiter.CanUpload(ip) {
		t.Error("4th upload should be blocked")
	}
}

func TestPendingFileLimiter_DifferentIPs(t *testing.T) {
	limiter := NewPendingFileLimiter(3)

	// Fill up limit for 3 different IPs
	for i := 0; i < 3; i++ {
		ip := fmt.Sprintf("192.168.1.%d", i)
		for j := 0; j < 3; j++ {
			limiter.TrackPendingFile(ip, fmt.Sprintf("file-%d-%d", i, j))
		}
	}

	// Each IP should be at limit
	for i := 0; i < 3; i++ {
		ip := fmt.Sprintf("192.168.1.%d", i)
		if limiter.CanUpload(ip) {
			t.Errorf("IP %s should be at limit", ip)
		}
		if limiter.PendingCount(ip) != 3 {
			t.Errorf("IP %s should have 3 pending, got %d", ip, limiter.PendingCount(ip))
		}
	}

	// A new IP should still be able to upload
	newIP := "10.0.0.1"
	if !limiter.CanUpload(newIP) {
		t.Error("new IP should be able to upload")
	}
}

func TestPendingFileLimiter_OnPaymentReceived(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	// Fill to limit
	for i := 0; i < 3; i++ {
		limiter.TrackPendingFile(ip, fmt.Sprintf("file%d", i))
	}

	if limiter.CanUpload(ip) {
		t.Error("should be at limit")
	}

	// Simulate payment for one file
	limiter.OnPaymentReceived("file1")

	// Should now allow upload
	if !limiter.CanUpload(ip) {
		t.Error("should allow upload after payment")
	}

	if limiter.PendingCount(ip) != 2 {
		t.Errorf("expected 2 pending, got %d", limiter.PendingCount(ip))
	}
}

func TestPendingFileLimiter_OnPaymentReceived_AllFiles(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	// Track 3 files
	limiter.TrackPendingFile(ip, "file1")
	limiter.TrackPendingFile(ip, "file2")
	limiter.TrackPendingFile(ip, "file3")

	// Pay for all of them
	limiter.OnPaymentReceived("file1")
	limiter.OnPaymentReceived("file2")
	limiter.OnPaymentReceived("file3")

	// IP should have 0 pending
	if limiter.PendingCount(ip) != 0 {
		t.Errorf("expected 0 pending, got %d", limiter.PendingCount(ip))
	}

	// Should be able to upload 3 more
	for i := 0; i < 3; i++ {
		if !limiter.CanUpload(ip) {
			t.Errorf("upload %d should be allowed after paying all", i+1)
		}
		limiter.TrackPendingFile(ip, fmt.Sprintf("newfile%d", i))
	}
}

func TestPendingFileLimiter_OnPaymentReceived_UnknownFile(t *testing.T) {
	limiter := NewPendingFileLimiter(3)

	// Should not panic on unknown file
	limiter.OnPaymentReceived("nonexistent")

	// Limiter should still work
	if !limiter.CanUpload("192.168.1.1") {
		t.Error("limiter should work after unknown file payment")
	}
}

func TestPendingFileLimiter_OnPaymentReceived_DuplicatePayment(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	limiter.TrackPendingFile(ip, "file1")
	if limiter.PendingCount(ip) != 1 {
		t.Error("should have 1 pending")
	}

	// Pay twice for the same file
	limiter.OnPaymentReceived("file1")
	limiter.OnPaymentReceived("file1")

	// Should have 0 pending (no negative count)
	if limiter.PendingCount(ip) != 0 {
		t.Errorf("expected 0 pending after duplicate payment, got %d", limiter.PendingCount(ip))
	}
}

func TestPendingFileLimiter_CleanupExpired(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	// Track files
	limiter.TrackPendingFile(ip, "file1")
	limiter.TrackPendingFile(ip, "file2")

	if limiter.PendingCount(ip) != 2 {
		t.Error("should have 2 pending before cleanup")
	}

	// Cleanup with very long duration should remove nothing
	removed := limiter.CleanupExpired(24 * time.Hour)
	if removed != 0 {
		t.Errorf("expected 0 removed with long duration, got %d", removed)
	}

	if limiter.PendingCount(ip) != 2 {
		t.Error("should still have 2 pending")
	}

	// Cleanup with 0 duration should remove all
	removed = limiter.CleanupExpired(0)
	if removed != 2 {
		t.Errorf("expected 2 removed with 0 duration, got %d", removed)
	}

	if limiter.PendingCount(ip) != 0 {
		t.Errorf("expected 0 pending after cleanup, got %d", limiter.PendingCount(ip))
	}
}

func TestPendingFileLimiter_CleanupExpired_OnlyOld(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	// Track a file
	limiter.TrackPendingFile(ip, "file1")

	// Wait a bit
	time.Sleep(50 * time.Millisecond)

	// Track another file
	limiter.TrackPendingFile(ip, "file2")

	// Cleanup files older than 25ms (should only remove file1)
	removed := limiter.CleanupExpired(25 * time.Millisecond)

	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	if limiter.PendingCount(ip) != 1 {
		t.Errorf("expected 1 pending, got %d", limiter.PendingCount(ip))
	}

	// The remaining file should be file2
	// Verify by paying for file2 and checking count goes to 0
	limiter.OnPaymentReceived("file2")
	if limiter.PendingCount(ip) != 0 {
		t.Error("file2 should have been the remaining file")
	}
}

func TestPendingFileLimiter_CleanupExpired_MultipleIPs(t *testing.T) {
	limiter := NewPendingFileLimiter(3)

	// Track files for multiple IPs
	limiter.TrackPendingFile("192.168.1.1", "file1")
	limiter.TrackPendingFile("192.168.1.2", "file2")
	limiter.TrackPendingFile("192.168.1.3", "file3")

	// Cleanup all
	removed := limiter.CleanupExpired(0)

	if removed != 3 {
		t.Errorf("expected 3 removed, got %d", removed)
	}

	// All IPs should have 0 pending
	for i := 1; i <= 3; i++ {
		ip := fmt.Sprintf("192.168.1.%d", i)
		if limiter.PendingCount(ip) != 0 {
			t.Errorf("IP %s should have 0 pending", ip)
		}
	}
}

func TestPendingFileLimiter_Concurrency(t *testing.T) {
	limiter := NewPendingFileLimiter(100)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ip := fmt.Sprintf("192.168.1.%d", n%10)
			fileID := fmt.Sprintf("file%d", n)

			limiter.CanUpload(ip)
			limiter.TrackPendingFile(ip, fileID)
			limiter.PendingCount(ip)
			limiter.OnPaymentReceived(fileID)
		}(i)
	}
	wg.Wait()

	// All files were paid, all IPs should have 0 pending
	for i := 0; i < 10; i++ {
		ip := fmt.Sprintf("192.168.1.%d", i)
		if limiter.PendingCount(ip) != 0 {
			t.Errorf("IP %s should have 0 pending after concurrent operations", ip)
		}
	}
}

func TestPendingFileLimiter_ConcurrencyWithCleanup(t *testing.T) {
	limiter := NewPendingFileLimiter(100)

	var wg sync.WaitGroup

	// Spawn writers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ip := fmt.Sprintf("192.168.1.%d", n%10)
			fileID := fmt.Sprintf("file%d", n)
			limiter.TrackPendingFile(ip, fileID)
		}(i)
	}

	// Spawn cleanup goroutines
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limiter.CleanupExpired(time.Hour)
		}()
	}

	// Spawn payment handlers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			fileID := fmt.Sprintf("file%d", n)
			limiter.OnPaymentReceived(fileID)
		}(i)
	}

	wg.Wait()
	// Should not deadlock or panic
}

func TestPendingFileLimiter_PendingCount(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	if limiter.PendingCount(ip) != 0 {
		t.Error("new IP should have 0 pending")
	}

	limiter.TrackPendingFile(ip, "file1")
	if limiter.PendingCount(ip) != 1 {
		t.Error("should have 1 pending")
	}

	limiter.TrackPendingFile(ip, "file2")
	if limiter.PendingCount(ip) != 2 {
		t.Error("should have 2 pending")
	}

	limiter.TrackPendingFile(ip, "file3")
	if limiter.PendingCount(ip) != 3 {
		t.Error("should have 3 pending")
	}
}

func TestPendingFileLimiter_MaxPending(t *testing.T) {
	limiter := NewPendingFileLimiter(5)
	if limiter.MaxPending() != 5 {
		t.Errorf("expected max pending 5, got %d", limiter.MaxPending())
	}

	limiter2 := NewPendingFileLimiter(10)
	if limiter2.MaxPending() != 10 {
		t.Errorf("expected max pending 10, got %d", limiter2.MaxPending())
	}
}

func TestPendingFileLimiter_IPv6(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ipv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"

	// Should work with IPv6 addresses
	for i := 0; i < 3; i++ {
		if !limiter.CanUpload(ipv6) {
			t.Errorf("upload %d should be allowed for IPv6", i+1)
		}
		limiter.TrackPendingFile(ipv6, fmt.Sprintf("file%d", i))
	}

	if limiter.CanUpload(ipv6) {
		t.Error("4th upload should be blocked for IPv6")
	}

	if limiter.PendingCount(ipv6) != 3 {
		t.Errorf("expected 3 pending for IPv6, got %d", limiter.PendingCount(ipv6))
	}
}

func TestPendingFileLimiter_EmptyIP(t *testing.T) {
	limiter := NewPendingFileLimiter(3)

	// Empty IP should still work (treated as a unique "IP")
	limiter.TrackPendingFile("", "file1")
	if limiter.PendingCount("") != 1 {
		t.Error("empty IP should track files")
	}

	limiter.OnPaymentReceived("file1")
	if limiter.PendingCount("") != 0 {
		t.Error("empty IP should have 0 after payment")
	}
}

func TestPendingFileLimiter_DuplicateTrack(t *testing.T) {
	limiter := NewPendingFileLimiter(3)
	ip := "192.168.1.1"

	// Track the same file twice
	limiter.TrackPendingFile(ip, "file1")
	limiter.TrackPendingFile(ip, "file1")

	// Should only count as 1 (map overwrites)
	if limiter.PendingCount(ip) != 1 {
		t.Errorf("duplicate track should count as 1, got %d", limiter.PendingCount(ip))
	}
}

func TestPendingFileLimiter_SameFileIDDifferentIPs(t *testing.T) {
	limiter := NewPendingFileLimiter(3)

	// Track same file ID from different IPs (edge case - shouldn't happen in practice)
	limiter.TrackPendingFile("192.168.1.1", "file1")
	limiter.TrackPendingFile("192.168.1.2", "file1")

	// Second track overwrites the IP mapping
	if limiter.PendingCount("192.168.1.1") != 1 {
		t.Error("first IP should still have the file in its pending set")
	}

	// Pay for the file - should clear from second IP's tracking
	limiter.OnPaymentReceived("file1")

	// First IP still has a stale entry (the file was re-associated to second IP)
	// This is acceptable edge case behavior - in practice file IDs are unique
	if limiter.PendingCount("192.168.1.2") != 0 {
		t.Error("second IP should have 0 pending after payment")
	}
}
