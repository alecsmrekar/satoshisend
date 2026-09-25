package payments

import (
	"net/url"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"
)

func TestParseNWCURI(t *testing.T) {
	pub := mustPublicKey(t, nostr.GeneratePrivateKey())
	secret := nostr.GeneratePrivateKey()
	relay := url.QueryEscape("wss://relay.example.com")

	t.Run("valid", func(t *testing.T) {
		uri := "nostr+walletconnect://" + pub + "?relay=" + relay + "&secret=" + secret + "&lud16=user@example.com"
		gotPub, gotRelay, gotSecret, err := parseNWCURI(uri)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotPub != pub {
			t.Errorf("pubkey = %s, want %s", gotPub, pub)
		}
		if gotRelay != "wss://relay.example.com" {
			t.Errorf("relay = %s, want wss://relay.example.com", gotRelay)
		}
		if gotSecret != secret {
			t.Error("secret mismatch")
		}
	})

	tests := []struct {
		name   string
		uri    string
		secret string
	}{
		{"wrong scheme", "https://" + pub + "?relay=" + relay + "&secret=" + secret, secret},
		{"invalid pubkey", "nostr+walletconnect://abc?relay=" + relay + "&secret=" + secret, secret},
		{"missing relay", "nostr+walletconnect://" + pub + "?secret=" + secret, secret},
		{"https relay", "nostr+walletconnect://" + pub + "?relay=" + url.QueryEscape("https://relay.example.com") + "&secret=" + secret, secret},
		{"missing secret", "nostr+walletconnect://" + pub + "?relay=" + relay, ""},
		{"short secret", "nostr+walletconnect://" + pub + "?relay=" + relay + "&secret=" + secret[:60], secret[:60]},
		{"invalid secret hex", "nostr+walletconnect://" + pub + "?relay=" + relay + "&secret=" + strings.Repeat("z", 64), strings.Repeat("z", 64)},
		{"unparseable", "nostr+walletconnect://" + pub + "?relay=" + relay + "&secret=" + secret + "\x7f", secret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := parseNWCURI(tt.uri)
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.secret != "" && strings.Contains(err.Error(), tt.secret) {
				t.Errorf("error contains the secret: %v", err)
			}
		})
	}
}

func TestCheckCapabilities(t *testing.T) {
	coinosMethods := []string{"pay_keysend", "pay_invoice", "pay", "receive", "get_balance", "get_info", "make_invoice", "lookup_invoice", "list_transactions"}

	tests := []struct {
		name              string
		methods           []string
		allowSpendCapable bool
		wantErr           bool
	}{
		{"receive-only", []string{"get_info", "make_invoice", "lookup_invoice", "list_transactions"}, false, false},
		{"missing make_invoice", []string{"get_info", "list_transactions"}, false, true},
		{"missing list_transactions", []string{"get_info", "make_invoice"}, false, true},
		{"spend-capable without override", coinosMethods, false, true},
		{"spend-capable with override", coinosMethods, true, false},
		{"multi_pay without override", []string{"make_invoice", "list_transactions", "multi_pay_invoice"}, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCapabilities(tt.methods, tt.allowSpendCapable)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func mustPublicKey(t *testing.T, secret string) string {
	t.Helper()
	pub, err := nostr.GetPublicKey(secret)
	if err != nil {
		t.Fatalf("GetPublicKey failed: %v", err)
	}
	return pub
}

func newTestNWCClient(t *testing.T) (*NWCClient, string) {
	t.Helper()
	walletSecret := nostr.GeneratePrivateKey()
	c, err := newNWCClient(mustPublicKey(t, walletSecret), "wss://relay.example.com", nostr.GeneratePrivateKey())
	if err != nil {
		t.Fatalf("newNWCClient failed: %v", err)
	}
	return c, walletSecret
}

// walletEvent builds an event the way the wallet sends it: encrypted to the client and signed by signer.
func walletEvent(t *testing.T, c *NWCClient, signer string, kind int, tags nostr.Tags, payload string) *nostr.Event {
	t.Helper()
	key, err := nip44.GenerateConversationKey(c.clientPubKey, signer)
	if err != nil {
		t.Fatalf("conversation key failed: %v", err)
	}
	content, err := nip44.Encrypt(payload, key)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	evt := &nostr.Event{CreatedAt: nostr.Now(), Kind: kind, Tags: tags, Content: content}
	if err := evt.Sign(signer); err != nil {
		t.Fatalf("sign failed: %v", err)
	}
	return evt
}

func TestNWCClient_HandleNotification(t *testing.T) {
	hash := strings.Repeat("ab", 32)

	tests := []struct {
		name        string
		wrongAuthor bool
		payload     string
		wantUpdate  bool
	}{
		{"settled_at", false, `{"notification_type":"payment_received","notification":{"payment_hash":"` + hash + `","settled_at":1700000000}}`, true},
		{"state settled", false, `{"notification_type":"payment_received","notification":{"payment_hash":"` + hash + `","state":"settled"}}`, true},
		{"no finality signal", false, `{"notification_type":"payment_received","notification":{"payment_hash":"` + hash + `","preimage":"00"}}`, false},
		{"payment_sent", false, `{"notification_type":"payment_sent","notification":{"payment_hash":"` + hash + `","settled_at":1700000000}}`, false},
		{"invalid payment hash", false, `{"notification_type":"payment_received","notification":{"payment_hash":"abc","settled_at":1700000000}}`, false},
		{"wrong author", true, `{"notification_type":"payment_received","notification":{"payment_hash":"` + hash + `","settled_at":1700000000}}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, walletSecret := newTestNWCClient(t)
			signer := walletSecret
			if tt.wrongAuthor {
				signer = nostr.GeneratePrivateKey()
			}

			c.handleEvent(walletEvent(t, c, signer, kindNWCNotification, nil, tt.payload))

			select {
			case update := <-c.updates:
				if !tt.wantUpdate {
					t.Fatalf("unexpected update: %+v", update)
				}
				if update.PaymentHash != hash || !update.Settled {
					t.Errorf("update = %+v, want settled %s", update, hash)
				}
			default:
				if tt.wantUpdate {
					t.Fatal("expected an update")
				}
			}
		})
	}
}

func TestNWCClient_HandleResponse(t *testing.T) {
	c, walletSecret := newTestNWCClient(t)
	requestID := strings.Repeat("1", 64)
	ch := make(chan nwcResponse, 1)
	c.waiting[requestID] = ch
	tags := nostr.Tags{{"e", requestID}, {"p", c.clientPubKey}}

	t.Run("result", func(t *testing.T) {
		c.handleEvent(walletEvent(t, c, walletSecret, nostr.KindNWCWalletResponse, tags,
			`{"result_type":"make_invoice","result":{"invoice":"lnbc1","payment_hash":"`+strings.Repeat("cd", 32)+`"}}`))

		select {
		case resp := <-ch:
			if resp.Error != nil {
				t.Fatalf("unexpected error: %+v", resp.Error)
			}
			if !strings.Contains(string(resp.Result), "lnbc1") {
				t.Errorf("result = %s, want the invoice", resp.Result)
			}
		default:
			t.Fatal("expected a response")
		}
	})

	t.Run("wallet error", func(t *testing.T) {
		c.handleEvent(walletEvent(t, c, walletSecret, nostr.KindNWCWalletResponse, tags,
			`{"result_type":"make_invoice","error":{"code":"RATE_LIMITED","message":"slow down"}}`))

		select {
		case resp := <-ch:
			if resp.Error == nil || resp.Error.Code != "RATE_LIMITED" {
				t.Errorf("error = %+v, want RATE_LIMITED", resp.Error)
			}
		default:
			t.Fatal("expected a response")
		}
	})

	t.Run("unknown request", func(t *testing.T) {
		otherTags := nostr.Tags{{"e", strings.Repeat("2", 64)}}
		c.handleEvent(walletEvent(t, c, walletSecret, nostr.KindNWCWalletResponse, otherTags, `{"result":{}}`))

		select {
		case resp := <-ch:
			t.Fatalf("unexpected response: %+v", resp)
		default:
		}
	})
}
