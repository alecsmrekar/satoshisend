package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"satoshisend/internal/api"
	"satoshisend/internal/files"
	"satoshisend/internal/logging"
	"satoshisend/internal/payments"
	"satoshisend/internal/store"
)

func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func printStats(st *store.SQLiteStore) {
	ctx := context.Background()
	stats, err := st.GetStats(ctx)
	if err != nil {
		logging.Internal.Fatalf("failed to get stats: %v", err)
	}

	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║          SatoshiSend Statistics          ║")
	fmt.Println("╠══════════════════════════════════════════╣")
	fmt.Printf("║  Total Files:     %-22d║\n", stats.TotalFiles)
	fmt.Printf("║  ├─ Paid:         %-22d║\n", stats.PaidFiles)
	fmt.Printf("║  ├─ Pending:      %-22d║\n", stats.PendingFiles)
	fmt.Printf("║  └─ Expired:      %-22d║\n", stats.ExpiredFiles)
	fmt.Println("╠══════════════════════════════════════════╣")
	fmt.Printf("║  Total Storage:   %-22s║\n", formatBytes(stats.TotalBytes))
	fmt.Printf("║  ├─ Paid:         %-22s║\n", formatBytes(stats.PaidBytes))
	fmt.Printf("║  └─ Pending:      %-22s║\n", formatBytes(stats.PendingBytes))
	fmt.Println("╠══════════════════════════════════════════╣")
	if !stats.OldestFile.IsZero() {
		fmt.Printf("║  Oldest File:     %-22s║\n", stats.OldestFile.Format("2006-01-02 15:04"))
		fmt.Printf("║  Newest File:     %-22s║\n", stats.NewestFile.Format("2006-01-02 15:04"))
	} else {
		fmt.Println("║  No files in database                    ║")
	}
	if len(stats.DailyStats) > 0 {
		fmt.Println("╠══════════════════════════════════════════╣")
		fmt.Println("║  Paid Files (last 14 days)               ║")
		fmt.Println("║  ──────────────────────────────────────  ║")
		for _, ds := range stats.DailyStats {
			fmt.Printf("║  %s:    %3d files  %12s  ║\n", ds.Date, ds.PaidFiles, formatBytes(ds.PaidBytes))
		}
	}
	fmt.Println("╚══════════════════════════════════════════╝")
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "satoshisend.db", "SQLite database path")
	storagePath := flag.String("storage", "./uploads", "File storage directory")
	showStats := flag.Bool("stats", false, "Show database statistics and exit")
	devMode := flag.Bool("dev", false, "Development mode: disables CORS restrictions and rate limiting")
	corsOrigins := flag.String("cors-origins", "https://satoshisend.xyz", "Comma-separated list of allowed CORS origins")
	flag.Parse()

	// Initialize store
	st, err := store.NewSQLiteStore(*dbPath)
	if err != nil {
		logging.Internal.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	// Show stats and exit if requested
	if *showStats {
		printStats(st)
		return
	}

	// Clean up any orphaned temp files from previous runs
	if cleanedUp := api.CleanupOrphanedTempFiles(); cleanedUp > 0 {
		logging.Internal.Printf("cleaned up %d orphaned temp files from previous run", cleanedUp)
	}

	// Initialize file storage - use B2 if configured, otherwise local filesystem
	var storage files.Storage
	b2Bucket := os.Getenv("B2_BUCKET")
	if b2Bucket != "" {
		b2PublicURL := os.Getenv("B2_PUBLIC_URL")
		b2Storage, err := files.NewB2Storage(files.B2Config{
			KeyID:     os.Getenv("B2_KEY_ID"),
			AppKey:    os.Getenv("B2_APP_KEY"),
			Bucket:    b2Bucket,
			Prefix:    os.Getenv("B2_PREFIX"),
			PublicURL: b2PublicURL,
		})
		if err != nil {
			logging.Internal.Fatalf("failed to initialize B2 storage: %v", err)
		}
		storage = b2Storage
		if b2PublicURL != "" {
			logging.Internal.Printf("using Backblaze B2 storage (bucket: %s, direct downloads enabled)", b2Bucket)
		} else {
			logging.Internal.Printf("using Backblaze B2 storage (bucket: %s)", b2Bucket)
		}
	} else {
		fsStorage, err := files.NewFSStorage(*storagePath)
		if err != nil {
			logging.Internal.Fatalf("failed to initialize storage: %v", err)
		}
		storage = fsStorage
		logging.Internal.Printf("using local filesystem storage (%s)", *storagePath)
	}

	// Initialize services
	filesSvc := files.NewService(storage, st)

	// Initialize LND client - use Alby HTTP API if configured, otherwise mock
	var lndClient payments.LNDClient
	var albyClient *payments.AlbyHTTPClient
	albyToken := os.Getenv("ALBY_TOKEN")
	albyWebhookSecret := os.Getenv("ALBY_WEBHOOK_SECRET")
	if albyToken != "" && albyWebhookSecret != "" {
		var err error
		albyClient, err = payments.NewAlbyHTTPClient(payments.AlbyConfig{
			AccessToken:   albyToken,
			WebhookSecret: albyWebhookSecret,
		})
		if err != nil {
			logging.Internal.Fatalf("failed to connect to Alby wallet: %v", err)
		}
		lndClient = albyClient
		logging.Internal.Println("connected to Lightning wallet via Alby HTTP API")
	} else if albyToken != "" {
		logging.Internal.Fatalf("ALBY_TOKEN is set but ALBY_WEBHOOK_SECRET is missing (see README for webhook setup)")
	} else {
		lndClient = payments.NewMockLNDClient()
		logging.Internal.Println("using mock LND client (set ALBY_TOKEN and ALBY_WEBHOOK_SECRET for real payments)")
	}
	paymentsSvc := payments.NewService(lndClient, st)

	// Load pending invoices from database (restart recovery)
	if err := paymentsSvc.LoadPendingInvoices(context.Background()); err != nil {
		logging.Internal.Printf("warning: failed to load pending invoices: %v", err)
	}

	// Create pending file limiter (max 3 pending files per IP)
	pendingLimiter := api.NewPendingFileLimiter(3)

	// Set payment callback to clear pending file tracking when payment is received
	paymentsSvc.SetPaymentCallback(pendingLimiter.OnPaymentReceived)

	// Start payment watcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := paymentsSvc.StartPaymentWatcher(ctx); err != nil {
		logging.Internal.Fatalf("failed to start payment watcher: %v", err)
	}

	// Start cleanup goroutine for expired files
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				count, err := filesSvc.CleanupExpired(ctx)
				if err != nil {
					logging.Internal.Printf("cleanup error: %v", err)
				} else if count > 0 {
					logging.Internal.Printf("cleaned up %d expired files", count)
				}

				// Cleanup expired entries from pending file limiter
				limiterCount := pendingLimiter.CleanupExpired(files.PendingTimeout)
				if limiterCount > 0 {
					logging.Internal.Printf("cleaned up %d expired pending file entries", limiterCount)
				}
			}
		}
	}()

	// Setup HTTP handler
	handler := api.NewHandler(filesSvc, paymentsSvc, pendingLimiter)

	// Wire up Alby webhook handler if configured
	if albyClient != nil {
		handler.SetWebhookHandler(albyClient)
	}

	// Serve static files for the frontend
	fs := http.FileServer(http.Dir("web"))

	mux := http.NewServeMux()
	mux.Handle("/api/", handler)

	// SPA routes - serve index.html for client-side routing
	mux.HandleFunc("/file/", serveIndex)
	mux.HandleFunc("/pending/", serveIndex)

	mux.Handle("/", fs)

	// Configure CORS
	var corsConfig api.CORSConfig
	if *devMode {
		logging.Internal.Println("development mode: CORS allowing all origins")
	} else {
		origins := strings.Split(*corsOrigins, ",")
		for i, o := range origins {
			origins[i] = strings.TrimSpace(o)
		}
		corsConfig.AllowedOrigins = origins
		logging.Internal.Printf("CORS restricted to origins: %v", origins)
	}

	// Apply middleware (order: Logger -> RateLimit -> CORS -> handler)
	var finalHandler http.Handler = mux
	finalHandler = api.CORS(corsConfig)(finalHandler)
	var rateLimiter *api.RateLimiterMiddleware
	if !*devMode {
		rateLimiter = api.NewRateLimiter(api.DefaultRateLimitConfig())
		finalHandler = rateLimiter.Middleware(finalHandler)
		logging.Internal.Println("rate limiting enabled")
	}
	finalHandler = api.Logger(finalHandler)

	server := &http.Server{
		Addr:    *addr,
		Handler: finalHandler,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		logging.Internal.Println("shutting down...")
		cancel()

		// Stop rate limiter cleanup goroutines
		if rateLimiter != nil {
			rateLimiter.Stop()
		}

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logging.Internal.Printf("shutdown error: %v", err)
		}
	}()

	logging.Internal.Printf("starting server on %s", *addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		logging.Internal.Fatalf("server error: %v", err)
	}
}
