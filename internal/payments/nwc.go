package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"

	"satoshisend/internal/logging"
)

// kindNWCNotification is the NIP-47 notification kind for NIP-44 encrypted content.
const kindNWCNotification = 23197

// NWCClient implements LNDClient over Nostr Wallet Connect (NIP-47).
// It sends requests to the wallet through a Nostr relay and gets payment notifications from the same relay.
type NWCClient struct {
	walletPubKey    string
	clientSecret    string
	clientPubKey    string
	relayURL        string
	conversationKey [32]byte

	mu      sync.Mutex
	relay   *nostr.Relay
	waiting map[string]chan nwcResponse // keyed by request event ID

	updates chan InvoiceUpdate
	cancel  context.CancelFunc
}

// NWCConfig holds configuration for the NWC client.
type NWCConfig struct {
	URI string // nostr+walletconnect://<wallet pubkey>?relay=<url>&secret=<hex>
	// AllowSpendCapable lets the client start with a connection that can send payments.
	AllowSpendCapable bool
}

type nwcResponse struct {
	Error  *nwcError       `json:"error"`
	Result json.RawMessage `json:"result"`
}

type nwcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type nwcInfo struct {
	Methods       []string `json:"methods"`
	Notifications []string `json:"notifications"`
}

type nwcTransaction struct {
	Invoice     string `json:"invoice"`
	PaymentHash string `json:"payment_hash"`
	State       string `json:"state"`
	SettledAt   int64  `json:"settled_at"`
}

// settled is true only for a wallet finality signal. A preimage alone is not proof of payment.
func (tx nwcTransaction) settled() bool {
	return tx.SettledAt > 0 || tx.State == "settled"
}

// NewNWCClient connects to the wallet relay and checks what the connection can do.
// It returns an error if the wallet cannot create and list invoices, or if the
// connection can spend funds and cfg.AllowSpendCapable is false.
func NewNWCClient(cfg NWCConfig) (*NWCClient, error) {
	walletPubKey, relayURL, secret, err := parseNWCURI(cfg.URI)
	if err != nil {
		return nil, err
	}

	c, err := newNWCClient(walletPubKey, relayURL, secret)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel

	sub, err := c.connect(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to relay %s: %w", relayURL, err)
	}
	go c.run(ctx, sub)

	var info nwcInfo
	if err := c.call(ctx, "get_info", struct{}{}, &info); err != nil {
		c.Close()
		return nil, err
	}
	if err := checkCapabilities(info.Methods, cfg.AllowSpendCapable); err != nil {
		c.Close()
		return nil, err
	}
	if !slices.Contains(info.Notifications, "payment_received") {
		logging.NWC.Println("WARNING: wallet does not send payment_received notifications. The wallet scan finds payments, with a delay of up to 30 seconds.")
	}

	logging.NWC.Printf("connected to wallet through %s", relayURL)
	return c, nil
}

func newNWCClient(walletPubKey, relayURL, secret string) (*NWCClient, error) {
	clientPubKey, err := nostr.GetPublicKey(secret)
	if err != nil {
		return nil, errors.New("NWC_URI has an invalid secret")
	}
	conversationKey, err := nip44.GenerateConversationKey(walletPubKey, secret)
	if err != nil {
		return nil, errors.New("NWC_URI has an invalid secret")
	}

	return &NWCClient{
		walletPubKey:    walletPubKey,
		clientSecret:    secret,
		clientPubKey:    clientPubKey,
		relayURL:        relayURL,
		conversationKey: conversationKey,
		waiting:         make(map[string]chan nwcResponse),
		updates:         make(chan InvoiceUpdate, 1000),
		cancel:          func() {},
	}, nil
}

// parseNWCURI reads the wallet pubkey, the first relay, and the client secret from an NWC connection URI.
// The errors never contain the URI, because the URI holds the secret.
func parseNWCURI(uri string) (walletPubKey, relayURL, secret string, err error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", "", errors.New("NWC_URI is not a valid URI")
	}
	if u.Scheme != "nostr+walletconnect" {
		return "", "", "", errors.New("NWC_URI must start with nostr+walletconnect://")
	}
	if !nostr.IsValidPublicKey(u.Host) {
		return "", "", "", errors.New("NWC_URI has an invalid wallet pubkey")
	}

	query := u.Query()
	relayURL = query.Get("relay")
	if !strings.HasPrefix(relayURL, "wss://") && !strings.HasPrefix(relayURL, "ws://") {
		return "", "", "", errors.New("NWC_URI must have a ws:// or wss:// relay")
	}

	secret = query.Get("secret")
	if !nostr.IsValid32ByteHex(secret) {
		return "", "", "", errors.New("NWC_URI must have a 64-character lowercase hex secret")
	}

	return u.Host, relayURL, secret, nil
}

func checkCapabilities(methods []string, allowSpendCapable bool) error {
	for _, required := range []string{"make_invoice", "list_transactions"} {
		if !slices.Contains(methods, required) {
			return fmt.Errorf("NWC connection does not support %s", required)
		}
	}

	var spendMethods []string
	for _, m := range methods {
		if strings.HasPrefix(m, "pay") || strings.HasPrefix(m, "multi_pay") {
			spendMethods = append(spendMethods, m)
		}
	}
	if len(spendMethods) == 0 {
		return nil
	}

	list := strings.Join(spendMethods, ", ")
	if !allowSpendCapable {
		return fmt.Errorf("NWC connection can spend funds (%s). Use a receive-only connection, or give the connection a small spend budget and set NWC_ALLOW_SPEND_CAPABLE=true", list)
	}
	logging.NWC.Printf("WARNING: NWC connection can spend funds (%s). Keep its spend budget small.", list)
	return nil
}

// connect opens a relay connection and subscribes to wallet responses and notifications.
// The connection stays open until the caller cancels ctx or the relay drops it.
func (c *NWCClient) connect(ctx context.Context) (*nostr.Subscription, error) {
	relay := nostr.NewRelay(ctx, c.relayURL)

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := relay.Connect(connectCtx); err != nil {
		relay.Close()
		return nil, err
	}

	sub, err := relay.Subscribe(ctx, nostr.Filters{{
		Kinds:   []int{nostr.KindNWCWalletResponse, kindNWCNotification},
		Authors: []string{c.walletPubKey},
		Tags:    nostr.TagMap{"p": {c.clientPubKey}},
	}})
	if err != nil {
		relay.Close()
		return nil, err
	}

	c.mu.Lock()
	c.relay = relay
	c.mu.Unlock()

	return sub, nil
}

// run reads events from sub. When the subscription ends, run connects again with backoff.
func (c *NWCClient) run(ctx context.Context, sub *nostr.Subscription) {
	for {
		for evt := range sub.Events {
			c.handleEvent(evt)
		}
		// The relay can end the subscription and keep the connection open. Close that connection before a new one opens.
		if sub.Relay.IsConnected() {
			sub.Relay.Close()
		}
		if ctx.Err() != nil {
			return
		}
		logging.NWC.Println("relay connection lost, connecting again")

		backoff := time.Second
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			var err error
			sub, err = c.connect(ctx)
			if err == nil {
				logging.NWC.Println("relay connection restored")
				break
			}
			logging.NWC.Printf("relay connection failed: %v", err)
			backoff = min(backoff*2, time.Minute)
		}
	}
}

func (c *NWCClient) handleEvent(evt *nostr.Event) {
	if evt.PubKey != c.walletPubKey {
		return
	}

	plaintext, err := nip44.Decrypt(evt.Content, c.conversationKey)
	if err != nil {
		logging.NWC.Printf("failed to decrypt event: %v", err)
		return
	}

	switch evt.Kind {
	case nostr.KindNWCWalletResponse:
		c.handleResponse(evt.Tags, plaintext)
	case kindNWCNotification:
		c.handleNotification(plaintext)
	}
}

func (c *NWCClient) handleResponse(tags nostr.Tags, plaintext string) {
	tag := tags.Find("e")
	if tag == nil {
		return
	}

	var resp nwcResponse
	if err := json.Unmarshal([]byte(plaintext), &resp); err != nil {
		logging.NWC.Printf("failed to parse response: %v", err)
		return
	}

	c.mu.Lock()
	ch, ok := c.waiting[tag[1]]
	c.mu.Unlock()
	if ok {
		select {
		case ch <- resp:
		default:
		}
	}
}

func (c *NWCClient) handleNotification(plaintext string) {
	var n struct {
		NotificationType string         `json:"notification_type"`
		Notification     nwcTransaction `json:"notification"`
	}
	if err := json.Unmarshal([]byte(plaintext), &n); err != nil {
		logging.NWC.Printf("failed to parse notification: %v", err)
		return
	}

	hash := n.Notification.PaymentHash
	if n.NotificationType != "payment_received" || !n.Notification.settled() || len(hash) != 64 {
		return
	}

	logging.NWC.Printf("payment %s received", hash[:16])

	select {
	case c.updates <- InvoiceUpdate{PaymentHash: hash, Settled: true}:
	default:
		logging.NWC.Printf("WARNING: update channel full, the wallet scan will find payment %s", hash[:16])
	}
}

// call sends one NIP-47 request and decodes the result into result.
func (c *NWCClient) call(ctx context.Context, method string, params any, result any) error {
	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	content, err := nip44.Encrypt(string(payload), c.conversationKey)
	if err != nil {
		return fmt.Errorf("%s: encrypt: %w", method, err)
	}

	evt := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindNWCWalletRequest,
		Tags:      nostr.Tags{{"p", c.walletPubKey}, {"encryption", "nip44_v2"}},
		Content:   content,
	}
	if err := evt.Sign(c.clientSecret); err != nil {
		return fmt.Errorf("%s: sign: %w", method, err)
	}

	ch := make(chan nwcResponse, 1)
	c.mu.Lock()
	relay := c.relay
	c.waiting[evt.ID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiting, evt.ID)
		c.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := relay.Publish(ctx, evt); err != nil {
		return fmt.Errorf("%s: publish: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("%s: wallet error %s: %s", method, resp.Error.Code, resp.Error.Message)
		}
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("%s: parse result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: no response from wallet: %w", method, ctx.Err())
	}
}

func (c *NWCClient) CreateInvoice(ctx context.Context, amountSats int64, memo string) (*Invoice, error) {
	var tx nwcTransaction
	err := c.call(ctx, "make_invoice", map[string]any{
		"amount":      amountSats * 1000,
		"description": memo,
		"expiry":      int64(InvoiceExpiry.Seconds()),
	}, &tx)
	if err != nil {
		return nil, err
	}
	if len(tx.PaymentHash) != 64 || tx.Invoice == "" {
		return nil, errors.New("make_invoice: wallet returned an incomplete invoice")
	}

	logging.NWC.Printf("created invoice %s for %d sats", tx.PaymentHash[:16], amountSats)

	return &Invoice{
		PaymentHash:    tx.PaymentHash,
		PaymentRequest: tx.Invoice,
		AmountSats:     amountSats,
	}, nil
}

func (c *NWCClient) SubscribeInvoices(ctx context.Context) (<-chan InvoiceUpdate, error) {
	return c.updates, nil
}

func (c *NWCClient) ListSettled(ctx context.Context, since time.Time) ([]string, error) {
	limit := 50
	var hashes []string
	for offset := 0; ; offset += limit {
		var page struct {
			Transactions []nwcTransaction `json:"transactions"`
		}
		err := c.call(ctx, "list_transactions", map[string]any{
			"from":   since.Unix(),
			"limit":  limit,
			"offset": offset,
			"type":   "incoming",
		}, &page)
		if err != nil {
			return nil, err
		}

		for _, tx := range page.Transactions {
			if tx.settled() {
				hashes = append(hashes, tx.PaymentHash)
			}
		}
		if len(page.Transactions) < limit {
			return hashes, nil
		}
	}
}

// Close stops the reconnect loop and closes the relay connection.
func (c *NWCClient) Close() error {
	c.cancel()
	return nil
}
