// Package web — notification wiring for per-device webhooks and ntfy.
//
// Each device (keyed by alias when set, else MAC) can carry:
//
//   - WebhookURL: a generic HTTP(S) endpoint POSTed with a JSON
//     payload on every ONLINE / OFFLINE event (CHANGE events are
//     intentionally skipped — too noisy).
//   - Ntfy: a server + credentials + two topics (req/res). Request
//     topic receives published event messages (same shape as the
//     webhook); response topic is subscribed to for messages pushed
//     *back* from other clients, surfaced via /api/notifications/messages.
//
// Everything is best-effort. Missing endpoints, network failures,
// invalid responses are all logged and shrugged off — the dispatcher
// never blocks the watcher or the HTTP API.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	argus "github.com/xxl6097/argusd"
)

// NotifyConfig is the per-device notification setup. Empty fields
// mean "feature off" — webhook-only users leave ntfy fields blank,
// and vice versa.
type NotifyConfig struct {
	WebhookURL string `json:"webhook_url,omitempty"`

	NtfyServer   string `json:"ntfy_server,omitempty"`   // e.g. https://ntfy.sh
	NtfyUsername string `json:"ntfy_username,omitempty"` // optional basic-auth
	NtfyPassword string `json:"ntfy_password,omitempty"` // optional basic-auth
	NtfyReqTopic string `json:"ntfy_req_topic,omitempty"`
	NtfyResTopic string `json:"ntfy_res_topic,omitempty"`
}

// HasWebhook reports whether the webhook path is configured.
func (c NotifyConfig) HasWebhook() bool { return strings.TrimSpace(c.WebhookURL) != "" }

// HasNtfyPublish reports whether we can publish ntfy events for this
// device (server + req topic required).
func (c NotifyConfig) HasNtfyPublish() bool {
	return strings.TrimSpace(c.NtfyServer) != "" && strings.TrimSpace(c.NtfyReqTopic) != ""
}

// HasNtfySubscribe reports whether we should subscribe to the
// response topic (server + res topic required).
func (c NotifyConfig) HasNtfySubscribe() bool {
	return strings.TrimSpace(c.NtfyServer) != "" && strings.TrimSpace(c.NtfyResTopic) != ""
}

// NotifyStore persists per-device notification configs, mirroring
// AliasStore / OverrideStore patterns. On-disk keys prefer the current
// alias (when attached), falling back to MAC for unaliased devices.
type NotifyStore struct {
	path    string
	aliases *AliasStore

	mu   sync.RWMutex
	data map[string]NotifyConfig // key = alias or MAC
}

// NewNotifyStore constructs a store backed by path. aliases is optional
// — when attached, on-disk keys use alias (if set) instead of MAC.
func NewNotifyStore(path string, aliases *AliasStore) *NotifyStore {
	s := &NotifyStore{
		path:    path,
		aliases: aliases,
		data:    make(map[string]NotifyConfig),
	}
	s.load()
	return s
}

func (s *NotifyStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	// Tighten file permissions on every load — older versions wrote
	// 0644, and the file holds ntfy credentials. Best-effort: ignore
	// chmod errors (Windows / network FS / etc.) since the data still
	// loads correctly.
	_ = os.Chmod(s.path, 0o600)
	var raw map[string]NotifyConfig
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	out := make(map[string]NotifyConfig, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = v
	}
	s.mu.Lock()
	s.data = out
	s.mu.Unlock()
}

// keyFor resolves a MAC to its storage key: alias when one exists
// and aliases is attached, otherwise normalized MAC.
func (s *NotifyStore) keyFor(mac string) string {
	mac = normalizeMAC(mac)
	if mac == "" {
		return ""
	}
	if s.aliases != nil {
		if name := s.aliases.Lookup(mac); name != "" {
			return name
		}
	}
	return mac
}

// Lookup returns the config for a MAC, falling back to legacy MAC-keyed
// entries when no alias is configured.
func (s *NotifyStore) Lookup(mac string) (NotifyConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if key := s.keyFor(mac); key != "" {
		if c, ok := s.data[key]; ok {
			return c, true
		}
	}
	macKey := normalizeMAC(mac)
	if c, ok := s.data[macKey]; ok {
		return c, true
	}
	if c, ok := s.data[strings.ToUpper(macKey)]; ok {
		return c, true
	}
	return NotifyConfig{}, false
}

// Set stores or replaces the config for a MAC. Whitespace-only fields
// are normalized to empty before persistence.
//
// Set NEVER deletes an existing row — even when every field comes in
// empty. The on-disk shape is the user's responsibility (managed
// exclusively through the dashboard's save/delete buttons), so we make
// it impossible for code paths like "alias rename" or "subscription
// reconcile" to silently nuke a row by passing an empty cfg. To remove
// a row, callers must invoke Delete(mac) explicitly.
func (s *NotifyStore) Set(mac string, cfg NotifyConfig) error {
	macKey := normalizeMAC(mac)
	if macKey == "" {
		return errors.New("web: notify mac required")
	}
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	cfg.NtfyServer = strings.TrimSpace(cfg.NtfyServer)
	cfg.NtfyUsername = strings.TrimSpace(cfg.NtfyUsername)
	cfg.NtfyPassword = strings.TrimSpace(cfg.NtfyPassword)
	cfg.NtfyReqTopic = strings.TrimSpace(cfg.NtfyReqTopic)
	cfg.NtfyResTopic = strings.TrimSpace(cfg.NtfyResTopic)
	if cfg.WebhookURL != "" {
		if _, err := url.ParseRequestURI(cfg.WebhookURL); err != nil {
			return fmt.Errorf("web: webhook_url invalid: %w", err)
		}
	}
	if cfg.NtfyServer != "" {
		if _, err := url.ParseRequestURI(cfg.NtfyServer); err != nil {
			return fmt.Errorf("web: ntfy_server invalid: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.keyFor(mac)
	if key == "" {
		key = macKey
	}
	s.data[key] = cfg
	// If we now have an alias-keyed row, drop any stale legacy MAC-keyed
	// duplicate so future Lookups don't return the wrong copy. This is
	// an in-place migration, not a deletion of user content.
	if key != macKey {
		delete(s.data, macKey)
		delete(s.data, strings.ToUpper(macKey))
	}
	return s.persistLocked()
}

// Delete removes the row for a MAC entirely. This is the ONLY API
// that drops an entry from notifications.json — every other write
// path (Set, subscription reconcile, alias rename) is required to
// preserve existing rows.
//
// Returns nil even when no row existed for the MAC, so the dashboard
// "remove" button is idempotent from the caller's perspective.
func (s *NotifyStore) Delete(mac string) error {
	macKey := normalizeMAC(mac)
	if macKey == "" {
		return errors.New("web: notify mac required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.keyFor(mac)
	if key != "" {
		delete(s.data, key)
	}
	delete(s.data, macKey)
	delete(s.data, strings.ToUpper(macKey))
	return s.persistLocked()
}

// AllByMAC returns a snapshot keyed by the uppercase MAC of every
// configured device. Requires the alias store to resolve alias keys
// back to MACs — entries whose alias no longer matches any known MAC
// are returned keyed by the alias itself.
//
// Used by the dispatcher on startup to walk subscriptions.
func (s *NotifyStore) AllByMAC(resolver func(alias string) (mac string, ok bool)) map[string]NotifyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]NotifyConfig, len(s.data))
	for key, cfg := range s.data {
		if strings.Contains(key, ":") {
			out[strings.ToUpper(key)] = cfg
			continue
		}
		if resolver != nil {
			if mac, ok := resolver(key); ok {
				out[strings.ToUpper(mac)] = cfg
				continue
			}
		}
		out[key] = cfg // keep alias-keyed fallback
	}
	return out
}

// Raw returns the exact on-disk map (alias or MAC keys) for GET handlers.
func (s *NotifyStore) Raw() map[string]NotifyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]NotifyConfig, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

func (s *NotifyStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	// 0600: notifications.json holds ntfy basic-auth credentials in
	// cleartext; we don't want non-root readers on the router. Atomic
	// write via tmp + rename so a crash mid-write can't truncate the
	// existing file.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// -------- Dispatcher --------

// InboxMessage is one entry in the per-device inbox fed by ntfy
// subscriptions (the "res_topic" side).
type InboxMessage struct {
	MAC        string    `json:"mac"`
	ReceivedAt time.Time `json:"received_at"`
	Topic      string    `json:"topic"`
	Title      string    `json:"title,omitempty"`
	Message    string    `json:"message"`
}

// Notifier dispatches device events to webhooks/ntfy and keeps a
// small per-MAC inbox of messages received on ntfy response topics.
type Notifier struct {
	store *NotifyStore
	log   *log.Logger
	http  *http.Client

	// Inbox: capped ring-buffer per MAC (uppercase MAC key).
	// We stash the last 100 messages per device; older ones are dropped.
	inboxMu sync.Mutex
	inbox   map[string][]InboxMessage

	// Subscription state: one goroutine per active (mac → res_topic)
	// pair. Map value is the cancel func so we can tear down stale
	// subs when the user edits a config.
	subsMu sync.Mutex
	subs   map[string]func() // key = uppercase MAC
}

const inboxMaxPerMAC = 100

// NewNotifier wires a notifier over the given store. Pass nil logger
// to use the default.
func NewNotifier(store *NotifyStore, logger *log.Logger) *Notifier {
	if logger == nil {
		logger = log.New(os.Stderr, "notifier: ", log.LstdFlags)
	}
	return &Notifier{
		store: store,
		log:   logger,
		http:  &http.Client{Timeout: 10 * time.Second},
		inbox: make(map[string][]InboxMessage),
		subs:  make(map[string]func()),
	}
}

// OnEvent is the legacy entry-point: notifies with a structured-only
// payload (no markdown). Server.OnEvent now calls Dispatch directly
// after building markdown context, so this is kept only for embedders
// that don't have worktime context to inject.
func (n *Notifier) OnEvent(e argus.Event) {
	if n == nil || n.store == nil {
		return
	}
	if e.Kind != argus.EventOnline && e.Kind != argus.EventOffline {
		return
	}
	cfg, ok := n.store.Lookup(e.Device.MAC)
	if !ok {
		return
	}
	n.Dispatch(e.Device.MAC, cfg, eventPayload(e), "")
}

// Dispatch sends a prebuilt payload + optional markdown body to
// whatever destinations the given config has. payload is the
// structured JSON body (always sent via webhook); markdown is the
// human-readable rendering used as the ntfy body and embedded in
// the webhook payload as `markdown`. Either may be empty.
func (n *Notifier) Dispatch(mac string, cfg NotifyConfig, payload map[string]any, kind string) {
	if n == nil {
		return
	}
	macU := strings.ToUpper(normalizeMAC(mac))
	sent := []string{}
	if cfg.HasWebhook() {
		sent = append(sent, "webhook")
		go n.postWebhook(cfg.WebhookURL, payload, macU, kind)
	}
	if cfg.HasNtfyPublish() {
		sent = append(sent, "ntfy")
		go n.publishNtfy(cfg, payload, macU, kind)
	}
	if len(sent) == 0 {
		n.log.Printf("event [%s %s]: config exists but no destinations configured", kind, macU)
	} else {
		n.log.Printf("event [%s %s]: dispatching to %s", kind, macU, strings.Join(sent, "+"))
	}
}

func eventPayload(e argus.Event) map[string]any {
	//return map[string]any{
	//	"time":     e.Time.Format(time.RFC3339),
	//	"kind":     e.Kind.String(),
	//	"mac":      strings.ToUpper(e.Device.MAC),
	//	"ip":       e.Device.IP,
	//	"hostname": e.Device.Hostname,
	//}
	return map[string]any{}
}

func (n *Notifier) postWebhook(rawURL string, payload map[string]any, macU, kind string) {
	body, _ := json.Marshal(payload)
	//fmt.Printf("body: %s\n", string(body))
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		n.log.Printf("webhook [%s %s] %s: build request: %v", kind, macU, rawURL, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "argus-app/1.0")
	resp, err := n.http.Do(req)
	if err != nil {
		n.log.Printf("webhook [%s %s] %s: %v (took %s)", kind, macU, rawURL, err, time.Since(start))
		return
	}
	defer resp.Body.Close()
	//_, _ = io.Copy(io.Discard, resp.Body)

	// 读取响应内容
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("无法读取响应内容:", err)
	}
	n.log.Printf("webhook [%s %s] %s: status=%d took=%s res=%s", kind, macU, rawURL, resp.StatusCode, time.Since(start), string(respBody))
}

// publishNtfy POSTs to <server>/<topic>. Body is the markdown when
// available (ntfy displays it directly), else falls back to the JSON
// payload. Markdown rendering on the recipient side requires the
// X-Markdown header.
func (n *Notifier) publishNtfy(cfg NotifyConfig, payload map[string]any, macU, kind string) {
	server := strings.TrimRight(cfg.NtfyServer, "/")
	target := server + "/" + cfg.NtfyReqTopic
	body, _ := json.Marshal(payload)
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		n.log.Printf("ntfy [%s %s] %s: build request: %v", kind, macU, target, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "argus-app/1.0")
	req.Header.Set("X-Title", fmt.Sprintf("%s %s", kind, macU))
	req.Header.Set("X-Tags", strings.ToLower(kind))
	if cfg.NtfyUsername != "" || cfg.NtfyPassword != "" {
		req.SetBasicAuth(cfg.NtfyUsername, cfg.NtfyPassword)
	}
	resp, err := n.http.Do(req)
	if err != nil {
		n.log.Printf("ntfy [%s %s] %s: %v (took %s)", kind, macU, target, err, time.Since(start))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	n.log.Printf("ntfy [%s %s] %s: status=%d took=%s", kind, macU, target, resp.StatusCode, time.Since(start))
}

// EnsureSubscriptions walks every known device in the store and starts
// (or restarts) a background ntfy subscriber for any device whose
// config has a res_topic. Called on startup and whenever a config changes.
//
// macsForKey resolves on-disk keys (alias or MAC) back to an uppercase
// MAC — necessary because res-topic messages need to land in the
// right MAC's inbox regardless of whether the key was stored by alias.
func (n *Notifier) EnsureSubscriptions(macsForKey func(key string) string) {
	if n == nil || n.store == nil {
		return
	}
	// Snapshot the desired state first so we can diff against the
	// current sub set atomically.
	raw := n.store.Raw()
	want := make(map[string]NotifyConfig) // uppercase MAC -> cfg
	for key, cfg := range raw {
		if !cfg.HasNtfySubscribe() {
			continue
		}
		mac := macsForKey(key)
		if mac == "" {
			// Key is alias with no current match — skip. Will retry on
			// next EnsureSubscriptions call after the device shows up.
			continue
		}
		want[strings.ToUpper(mac)] = cfg
	}
	n.subsMu.Lock()
	defer n.subsMu.Unlock()
	// Tear down subs for devices that no longer want one or have a
	// changed config (simplest: always recreate when present).
	for mac, cancel := range n.subs {
		cancel()
		delete(n.subs, mac)
	}
	for mac, cfg := range want {
		ctx, cancel := context.WithCancel(context.Background())
		n.subs[mac] = cancel
		go n.runNtfySubscriber(ctx, mac, cfg)
	}
}

// StopSubscriptions cancels every active ntfy subscriber. Called on
// server shutdown.
func (n *Notifier) StopSubscriptions() {
	if n == nil {
		return
	}
	n.subsMu.Lock()
	defer n.subsMu.Unlock()
	for mac, cancel := range n.subs {
		cancel()
		delete(n.subs, mac)
	}
}

// runNtfySubscriber streams /json from the ntfy server for the given
// res_topic, appending messages to the MAC's inbox. Auto-reconnects
// on any error with a short backoff; stops when ctx is cancelled.
func (n *Notifier) runNtfySubscriber(ctx context.Context, mac string, cfg NotifyConfig) {
	server := strings.TrimRight(cfg.NtfyServer, "/")
	// ntfy's streaming JSON endpoint.
	streamURL := server + "/" + cfg.NtfyResTopic + "/json"
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := n.streamNtfyOnce(ctx, mac, cfg, streamURL); err != nil && ctx.Err() == nil {
			n.log.Printf("ntfy subscribe %s: %v (retry in %s)", streamURL, err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// Exponential-ish backoff, capped — but stay short enough that
		// a restarted ntfy server is picked up within a minute.
		if backoff < 60*time.Second {
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
	}
}

func (n *Notifier) streamNtfyOnce(ctx context.Context, mac string, cfg NotifyConfig, streamURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "argus-app/1.0")
	if cfg.NtfyUsername != "" || cfg.NtfyPassword != "" {
		req.SetBasicAuth(cfg.NtfyUsername, cfg.NtfyPassword)
	}
	// Long-lived stream — no client timeout, rely on ctx cancel.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ntfy subscribe: status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			ID      string `json:"id"`
			Time    int64  `json:"time"`
			Event   string `json:"event"`
			Topic   string `json:"topic"`
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		if err := dec.Decode(&msg); err != nil {
			return err
		}
		// ntfy sends "keepalive" / "open" events too; only store real
		// messages (event == "message" or empty for historical servers).
		if msg.Event != "" && msg.Event != "message" {
			continue
		}
		n.appendInbox(mac, InboxMessage{
			MAC:        mac,
			ReceivedAt: time.Now(),
			Topic:      msg.Topic,
			Title:      msg.Title,
			Message:    msg.Message,
		})
	}
}

func (n *Notifier) appendInbox(mac string, m InboxMessage) {
	n.inboxMu.Lock()
	defer n.inboxMu.Unlock()
	buf := n.inbox[mac]
	buf = append(buf, m)
	if len(buf) > inboxMaxPerMAC {
		buf = buf[len(buf)-inboxMaxPerMAC:]
	}
	n.inbox[mac] = buf
}

// Inbox returns a snapshot of the most-recent messages for a MAC,
// newest first. Empty when the MAC has no subscription or no messages.
func (n *Notifier) Inbox(mac string) []InboxMessage {
	if n == nil {
		return nil
	}
	mac = strings.ToUpper(normalizeMAC(mac))
	n.inboxMu.Lock()
	defer n.inboxMu.Unlock()
	buf := n.inbox[mac]
	// Return newest-first reversed copy.
	out := make([]InboxMessage, len(buf))
	for i, m := range buf {
		out[len(buf)-1-i] = m
	}
	return out
}
