package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"valid timestamp", "1703980800", 1703980800, false},
		{"zero", "0", 0, false},
		{"invalid", "not-a-number", 0, true},
		{"empty", "", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTimestamp(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("parseTimestamp(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
				return
			}
			if !tc.wantErr && got.Unix() != tc.want {
				t.Errorf("parseTimestamp(%q) = %v, want %v", tc.input, got.Unix(), tc.want)
			}
		})
	}
}

func TestVerifyWebhookSignature(t *testing.T) {
	// Create a client with a known secret
	secret := base64.StdEncoding.EncodeToString([]byte("test-secret-key-1234"))
	client := &AlbyHTTPClient{
		webhookSecret: "whsec_" + secret,
	}

	// Helper to create valid signature
	createSignature := func(body, svixID, timestamp string) string {
		secretBytes, _ := base64.StdEncoding.DecodeString(secret)
		signedContent := fmt.Sprintf("%s.%s.%s", svixID, timestamp, body)
		mac := hmac.New(sha256.New, secretBytes)
		mac.Write([]byte(signedContent))
		return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}

	now := time.Now()
	validTimestamp := fmt.Sprintf("%d", now.Unix())
	testBody := `{"payment_hash":"abc123","settled":true}`
	svixID := "msg_test123"

	t.Run("valid signature", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", createSignature(testBody, svixID, validTimestamp))

		err := client.verifyWebhookSignature([]byte(testBody), headers)
		if err != nil {
			t.Errorf("expected valid signature, got error: %v", err)
		}
	})

	t.Run("missing headers", func(t *testing.T) {
		headers := http.Header{}
		err := client.verifyWebhookSignature([]byte(testBody), headers)
		if err == nil {
			t.Error("expected error for missing headers")
		}
	})

	t.Run("invalid signature", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", "v1,invalidsignature")

		err := client.verifyWebhookSignature([]byte(testBody), headers)
		if err == nil {
			t.Error("expected error for invalid signature")
		}
	})

	t.Run("expired timestamp", func(t *testing.T) {
		oldTimestamp := fmt.Sprintf("%d", now.Add(-10*time.Minute).Unix())
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", oldTimestamp)
		headers.Set("svix-signature", createSignature(testBody, svixID, oldTimestamp))

		err := client.verifyWebhookSignature([]byte(testBody), headers)
		if err == nil {
			t.Error("expected error for expired timestamp")
		}
	})

	t.Run("future timestamp", func(t *testing.T) {
		futureTimestamp := fmt.Sprintf("%d", now.Add(10*time.Minute).Unix())
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", futureTimestamp)
		headers.Set("svix-signature", createSignature(testBody, svixID, futureTimestamp))

		err := client.verifyWebhookSignature([]byte(testBody), headers)
		if err == nil {
			t.Error("expected error for future timestamp")
		}
	})

	t.Run("tampered body", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", createSignature(testBody, svixID, validTimestamp))

		// Try with different body
		err := client.verifyWebhookSignature([]byte(`{"tampered":true}`), headers)
		if err == nil {
			t.Error("expected error for tampered body")
		}
	})

	t.Run("multiple signatures with one valid", func(t *testing.T) {
		validSig := createSignature(testBody, svixID, validTimestamp)
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", "v1,invalid1 "+validSig+" v1,invalid2")

		err := client.verifyWebhookSignature([]byte(testBody), headers)
		if err != nil {
			t.Errorf("expected valid with multiple signatures, got error: %v", err)
		}
	})
}

func TestHandleWebhook(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("test-secret-key-1234"))
	client := &AlbyHTTPClient{
		webhookSecret: "whsec_" + secret,
		updates:       make(chan InvoiceUpdate, 10),
	}

	createSignature := func(body, svixID, timestamp string) string {
		secretBytes, _ := base64.StdEncoding.DecodeString(secret)
		signedContent := fmt.Sprintf("%s.%s.%s", svixID, timestamp, body)
		mac := hmac.New(sha256.New, secretBytes)
		mac.Write([]byte(signedContent))
		return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}

	now := time.Now()
	validTimestamp := fmt.Sprintf("%d", now.Unix())
	svixID := "msg_test123"

	t.Run("valid settled invoice", func(t *testing.T) {
		// Payment hash must be at least 16 chars for logging
		body := `{"payment_hash":"abc123def456789012345678","settled":true,"amount":1000}`
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", createSignature(body, svixID, validTimestamp))

		err := client.HandleWebhook([]byte(body), headers)
		if err != nil {
			t.Errorf("expected no error, got: %v", err)
		}

		// Check that update was sent to channel
		select {
		case update := <-client.updates:
			if update.PaymentHash != "abc123def456789012345678" {
				t.Errorf("expected payment hash abc123def456789012345678, got %s", update.PaymentHash)
			}
			if !update.Settled {
				t.Error("expected settled to be true")
			}
		default:
			t.Error("expected update in channel")
		}
	})

	t.Run("unsettled invoice ignored", func(t *testing.T) {
		body := `{"payment_hash":"xyz789","settled":false,"amount":500}`
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", createSignature(body, svixID, validTimestamp))

		err := client.HandleWebhook([]byte(body), headers)
		if err != nil {
			t.Errorf("expected no error, got: %v", err)
		}

		// Channel should be empty
		select {
		case <-client.updates:
			t.Error("expected no update for unsettled invoice")
		default:
			// Good, no update
		}
	})

	t.Run("invalid signature rejected", func(t *testing.T) {
		body := `{"payment_hash":"test123","settled":true}`
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", "v1,invalidsig")

		err := client.HandleWebhook([]byte(body), headers)
		if err == nil {
			t.Error("expected error for invalid signature")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		body := `{invalid json}`
		headers := http.Header{}
		headers.Set("svix-id", svixID)
		headers.Set("svix-timestamp", validTimestamp)
		headers.Set("svix-signature", createSignature(body, svixID, validTimestamp))

		err := client.HandleWebhook([]byte(body), headers)
		if err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
}

func TestSubscribeInvoices(t *testing.T) {
	client := &AlbyHTTPClient{
		updates: make(chan InvoiceUpdate, 10),
	}

	ch, err := client.SubscribeInvoices(nil)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	if ch != client.updates {
		t.Error("expected returned channel to be the updates channel")
	}
}
