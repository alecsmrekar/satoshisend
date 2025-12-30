package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"satoshisend/internal/logging"
)

const albyAPIBase = "https://api.getalby.com"

// AlbyHTTPClient implements LNDClient using the Alby Wallet HTTP API.
// This connects to Alby's custodial wallet service and uses webhooks for payment notifications.
type AlbyHTTPClient struct {
	accessToken   string
	httpClient    *http.Client
	webhookSecret string

	updates chan InvoiceUpdate
	done    chan struct{}
}

// Alby API request/response structures
type albyCreateInvoiceRequest struct {
	Amount      int64  `json:"amount"`
	Description string `json:"description,omitempty"`
	Memo        string `json:"memo,omitempty"`
}

type albyInvoiceResponse struct {
	PaymentHash    string `json:"payment_hash"`
	PaymentRequest string `json:"payment_request"`
	Amount         int64  `json:"amount"`
	Settled        bool   `json:"settled"`
	SettledAt      string `json:"settled_at,omitempty"`
}

// AlbyConfig holds configuration for the Alby HTTP client.
type AlbyConfig struct {
	AccessToken   string
	WebhookSecret string // The SVIX webhook secret from your Alby webhook endpoint
}

// NewAlbyHTTPClient creates a new Alby HTTP API client with webhook support.
// The webhook must be pre-registered with Alby and the secret provided in config.
func NewAlbyHTTPClient(cfg AlbyConfig) (*AlbyHTTPClient, error) {
	if cfg.AccessToken == "" {
		return nil, fmt.Errorf("access token is required")
	}
	if cfg.WebhookSecret == "" {
		return nil, fmt.Errorf("webhook secret is required")
	}

	c := &AlbyHTTPClient{
		accessToken:   cfg.AccessToken,
		webhookSecret: cfg.WebhookSecret,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		updates: make(chan InvoiceUpdate, 1000),
		done:    make(chan struct{}),
	}

	// Test the connection
	logging.Alby.Println("testing connection...")
	if err := c.testConnection(); err != nil {
		return nil, fmt.Errorf("failed to connect to Alby: %w", err)
	}
	logging.Alby.Println("connected successfully!")

	return c, nil
}

func (c *AlbyHTTPClient) testConnection() error {
	req, err := http.NewRequest("GET", albyAPIBase+"/balance", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *AlbyHTTPClient) CreateInvoice(ctx context.Context, amountSats int64, memo string) (*Invoice, error) {
	logging.Alby.Printf("creating invoice for %d sats...", amountSats)

	reqBody := albyCreateInvoiceRequest{
		Amount:      amountSats,
		Description: memo,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", albyAPIBase+"/invoices", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var albyResp albyInvoiceResponse
	if err := json.NewDecoder(resp.Body).Decode(&albyResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	inv := &Invoice{
		PaymentHash:    albyResp.PaymentHash,
		PaymentRequest: albyResp.PaymentRequest,
		AmountSats:     amountSats,
	}

	logging.Alby.Printf("created invoice %s for %d sats", albyResp.PaymentHash[:16], amountSats)

	return inv, nil
}

func (c *AlbyHTTPClient) SubscribeInvoices(ctx context.Context) (<-chan InvoiceUpdate, error) {
	return c.updates, nil
}

func (c *AlbyHTTPClient) Close() error {
	close(c.done)
	return nil
}

// AlbyWebhookPayload is the payload sent by Alby when an invoice is settled.
type AlbyWebhookPayload struct {
	Amount      int64  `json:"amount"`
	Settled     bool   `json:"settled"`
	SettledAt   string `json:"settled_at,omitempty"`
	Type        string `json:"type"`
	PaymentHash string `json:"payment_hash"`
}

// HandleWebhook processes an incoming webhook request from Alby.
// It verifies the SVIX signature and sends the update to the channel.
func (c *AlbyHTTPClient) HandleWebhook(body []byte, headers http.Header) error {
	// Verify SVIX signature
	if err := c.verifyWebhookSignature(body, headers); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}

	var payload AlbyWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("failed to parse payload: %w", err)
	}

	if payload.Settled && payload.PaymentHash != "" {
		logging.Alby.Printf("webhook: invoice %s settled!", payload.PaymentHash[:16])

		select {
		case c.updates <- InvoiceUpdate{
			PaymentHash: payload.PaymentHash,
			Settled:     true,
		}:
			logging.Alby.Printf("webhook: queued payment %s (buffer: %d/%d)", payload.PaymentHash[:16], len(c.updates), cap(c.updates))
		default:
			// Payment dropped from channel - will be recovered from DB on next webhook or restart
			logging.Alby.Printf("webhook: WARNING - update channel full (%d/%d), payment %s not queued (persisted to DB). Payment hash: %s",
				len(c.updates), cap(c.updates), payload.PaymentHash[:16], payload.PaymentHash)
		}
	}

	return nil
}

// verifyWebhookSignature verifies the SVIX signature on a webhook request.
// SVIX signs webhooks using HMAC-SHA256.
func (c *AlbyHTTPClient) verifyWebhookSignature(body []byte, headers http.Header) error {
	svixID := headers.Get("svix-id")
	svixTimestamp := headers.Get("svix-timestamp")
	svixSignature := headers.Get("svix-signature")

	if svixID == "" || svixTimestamp == "" || svixSignature == "" {
		return fmt.Errorf("missing SVIX headers")
	}

	// Check timestamp to prevent replay attacks (5 minute tolerance)
	ts, err := parseTimestamp(svixTimestamp)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	now := time.Now()
	if now.Sub(ts) > 5*time.Minute || ts.Sub(now) > 5*time.Minute {
		return fmt.Errorf("timestamp too old or in future")
	}

	// Extract the base64 secret (remove "whsec_" prefix)
	secret := c.webhookSecret
	if strings.HasPrefix(secret, "whsec_") {
		secret = secret[6:]
	}

	secretBytes, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return fmt.Errorf("failed to decode secret: %w", err)
	}

	// Create signed content: id.timestamp.body
	signedContent := fmt.Sprintf("%s.%s.%s", svixID, svixTimestamp, string(body))

	// Calculate expected signature
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(signedContent))
	expectedSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// SVIX signature format: "v1,<base64sig>" (may have multiple signatures)
	signatures := strings.Split(svixSignature, " ")
	for _, sig := range signatures {
		parts := strings.SplitN(sig, ",", 2)
		if len(parts) == 2 && parts[0] == "v1" {
			if hmac.Equal([]byte(parts[1]), []byte(expectedSig)) {
				return nil
			}
		}
	}

	return fmt.Errorf("signature mismatch")
}

func parseTimestamp(ts string) (time.Time, error) {
	var unix int64
	if _, err := fmt.Sscanf(ts, "%d", &unix); err != nil {
		return time.Time{}, err
	}
	return time.Unix(unix, 0), nil
}
