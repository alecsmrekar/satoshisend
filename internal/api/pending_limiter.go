package api

import (
	"sync"
	"time"
)

// PendingFileLimiter tracks pending (unpaid) files per IP address and enforces
// a maximum number of concurrent pending files per IP. This prevents abuse where
// users upload many files without paying.
type PendingFileLimiter struct {
	mu          sync.RWMutex
	maxPending  int
	pendingByIP map[string]map[string]time.Time // IP -> fileID -> tracked time
	fileToIP    map[string]string               // fileID -> IP (reverse lookup)
}

// NewPendingFileLimiter creates a new limiter with the specified maximum
// pending files per IP.
func NewPendingFileLimiter(maxPending int) *PendingFileLimiter {
	return &PendingFileLimiter{
		maxPending:  maxPending,
		pendingByIP: make(map[string]map[string]time.Time),
		fileToIP:    make(map[string]string),
	}
}

// CanUpload checks if the IP can upload another file.
// Returns true if under the limit, false otherwise.
func (l *PendingFileLimiter) CanUpload(ip string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	files := l.pendingByIP[ip]
	return len(files) < l.maxPending
}

// PendingCount returns the number of pending files for an IP.
func (l *PendingFileLimiter) PendingCount(ip string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return len(l.pendingByIP[ip])
}

// MaxPending returns the configured maximum pending files per IP.
func (l *PendingFileLimiter) MaxPending() int {
	return l.maxPending
}

// TrackPendingFile records a new pending file for an IP.
// Should be called after successful upload before payment.
func (l *PendingFileLimiter) TrackPendingFile(ip, fileID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.pendingByIP[ip] == nil {
		l.pendingByIP[ip] = make(map[string]time.Time)
	}
	l.pendingByIP[ip][fileID] = time.Now()
	l.fileToIP[fileID] = ip
}

// OnPaymentReceived removes a file from pending tracking.
// This is the callback to be invoked when payment settles.
func (l *PendingFileLimiter) OnPaymentReceived(fileID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	ip, ok := l.fileToIP[fileID]
	if !ok {
		return // File not tracked (maybe already expired/cleaned)
	}

	delete(l.fileToIP, fileID)
	if files := l.pendingByIP[ip]; files != nil {
		delete(files, fileID)
		if len(files) == 0 {
			delete(l.pendingByIP, ip)
		}
	}
}

// CleanupExpired removes files older than the given duration.
// Should be called periodically (e.g., hourly along with file cleanup).
// Returns the number of entries removed.
func (l *PendingFileLimiter) CleanupExpired(maxAge time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	removed := 0

	for ip, files := range l.pendingByIP {
		for fileID, trackedAt := range files {
			if trackedAt.Before(cutoff) {
				delete(files, fileID)
				delete(l.fileToIP, fileID)
				removed++
			}
		}
		if len(files) == 0 {
			delete(l.pendingByIP, ip)
		}
	}

	return removed
}
