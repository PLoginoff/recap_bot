// Max Messenger client implementation
// Documentation: https://dev.max.ru/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const MessengerMax MessengerType = "max"

const maxAPIURL = "https://platform-api2.max.ru"

const maxSkipTLSVerify = true

const maxRetryDelay = 3 * time.Second
const maxDownloadTimeout = 120 * time.Second
const maxPollInterval = 2 * time.Second
const maxHTTPTimeout = 35 * time.Second
const maxUpdatesTimeout = 29

type MaxMessenger struct {
	token          string
	eventHandler   EventHandler
	messages       ConfigMessages
	httpClient     *http.Client
	downloadClient *http.Client
	debug          bool
	ffprobePath    string

	// Webhook mode
	webhookServer *WebhookServer
	webhookPath   string
	webhookURL    string // full https://host:port/path sent to Max API

	// Dedup recently processed message IDs to avoid duplicates from Max API retries
	seenMids map[string]time.Time
	mu       sync.Mutex
}

// NewMaxMessenger creates a Max messenger. If webhookServer and webhookPath are provided,
// it runs in webhook mode; otherwise falls back to long polling.
func NewMaxMessenger(token string, messages ConfigMessages, eventHandler EventHandler, debug bool, ffprobePath string, webhookServer *WebhookServer, webhookPath string) *MaxMessenger {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: maxSkipTLSVerify},
	}
	m := &MaxMessenger{
		token:        token,
		messages:     messages,
		eventHandler: eventHandler,
		debug:        debug,
		httpClient: &http.Client{
			Timeout:   maxHTTPTimeout,
			Transport: transport,
		},
		downloadClient: &http.Client{
			Timeout:   maxDownloadTimeout,
			Transport: transport,
		},
		ffprobePath:   ffprobePath,
		webhookServer: webhookServer,
		webhookPath:   webhookPath,
		seenMids:      make(map[string]time.Time),
	}
	if webhookServer != nil && webhookPath != "" {
		m.webhookURL = buildWebhookURL(webhookServer, webhookPath)
	}
	return m
}

func buildWebhookURL(srv *WebhookServer, path string) string {
	listen := srv.listen
	if listen == "" {
		listen = ":8443"
	}
	scheme := "https"
	if srv.tlsCert == "" || srv.tlsKey == "" {
		scheme = "http"
	}
	if strings.HasPrefix(listen, ":") {
		if srv.publicURL != "" {
			return strings.TrimSuffix(srv.publicURL, "/") + path
		}
		return scheme + "://localhost" + listen + path
	}
	return scheme + "://" + listen + path
}

func (m *MaxMessenger) Start(ctx context.Context) error {
	// cleanup old dedup entries periodically
	go func() {
		ticker := time.NewTicker(maxDedupAge)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.cleanupSeenMids()
			case <-ctx.Done():
				return
			}
		}
	}()

	if m.webhookURL != "" {
		return m.startWebhook(ctx)
	}
	go m.pollUpdates(ctx)
	return nil
}

func (m *MaxMessenger) startWebhook(ctx context.Context) error {
	if m.webhookServer == nil {
		return fmt.Errorf("webhook server not configured")
	}
	if m.webhookURL == "" {
		return fmt.Errorf("webhook URL is empty (check webhooks.public_url in config)")
	}
	if strings.Contains(m.webhookURL, "localhost") || strings.Contains(m.webhookURL, "127.0.0.1") {
		return fmt.Errorf("webhook URL must be a public address, not localhost: %s", m.webhookURL)
	}

	// Register raw MaxUpdate handler on the shared webhook server
	m.webhookServer.RegisterMaxHandler(m.webhookPath, func(ctx context.Context, update *MaxUpdate) {
		m.handleUpdate(ctx, update)
	})

	// Unsubscribe any existing webhook to prevent duplicate deliveries
	if err := m.unsubscribeWebhook(ctx); err != nil {
		slog.Debug("Max: no existing subscription to clean up", "error", err)
	}

	// Log the URL we're about to subscribe with
	slog.Info("Max: subscribing webhook", "url", m.webhookURL)

	// Subscribe with Max API
	if err := m.subscribeWebhook(ctx); err != nil {
		slog.Warn("Max: failed to subscribe webhook, falling back to polling", "error", err, "url", m.webhookURL)
		go m.pollUpdates(ctx)
		return nil
	}

	slog.Info("Max: webhook active", "url", m.webhookURL)
	return nil
}

func (m *MaxMessenger) subscribeWebhook(ctx context.Context) error {
	payload := map[string]interface{}{
		"url":          m.webhookURL,
		"update_types": []string{"message_created", "message_callback", "bot_started"},
	}
	if m.webhookServer != nil && m.webhookServer.secret != "" {
		payload["secret"] = m.webhookServer.secret
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal subscription payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, maxAPIURL+"/subscriptions", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read subscription response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe failed (%s): %s", resp.Status, string(respBody))
	}

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("subscribe failed: %s", result.Message)
	}

	slog.Info("Max: webhook subscribed", "url", m.webhookURL)
	return nil
}

func (m *MaxMessenger) unsubscribeWebhook(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, maxAPIURL+"/subscriptions", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", m.token)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unsubscribe failed (%s): %s", resp.Status, string(body))
	}
	return nil
}

func (m *MaxMessenger) pollUpdates(ctx context.Context) {
	var marker int64

	for {
		select {
		case <-ctx.Done():
			return
		default:
			updates, newMarker, err := m.getUpdates(ctx, marker)
			if err != nil {
				slog.Warn("Max: error getting updates", "error", err)
				select {
				case <-time.After(maxRetryDelay):
				case <-ctx.Done():
					return
				}
				continue
			}

			if newMarker > 0 {
				marker = newMarker
			}

			for _, update := range updates {
				m.handleUpdate(ctx, &update)
			}

			select {
			case <-time.After(maxPollInterval):
			case <-ctx.Done():
				return
			}
		}
	}
}

type MaxUpdate struct {
	UpdateType string          `json:"update_type"`
	Timestamp  int64           `json:"timestamp"`
	Message    *MaxMessage     `json:"message"`
	Callback   *MaxCallback    `json:"callback"`
	UserLocale string          `json:"user_locale"`
	Payload    json.RawMessage `json:"payload"`
}

type MaxMessage struct {
	Mid         string          `json:"mid"`
	Recipient   MaxRecipient    `json:"recipient"`
	Sender      MaxSender       `json:"sender"`
	Timestamp   int64           `json:"timestamp"`
	Body        MaxMessageBody  `json:"body"`
	Attachments []MaxAttachment `json:"attachments,omitempty"`
	Link        *MaxLink        `json:"link,omitempty"`
}

type MaxRecipient struct {
	ChatID   int64  `json:"chat_id"`
	ChatType string `json:"chat_type"`
	UserID   int64  `json:"user_id"`
}

type MaxSender struct {
	UserID    int64  `json:"user_id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Name      string `json:"name"`
}

type MaxLink struct {
	Type    string     `json:"type"`
	Message MaxMessage `json:"message"`
}

type MaxMessageBody struct {
	Mid         string          `json:"mid"`
	Seq         int64           `json:"seq"`
	Text        string          `json:"text"`
	Attachments []MaxAttachment `json:"attachments"`
}

type MaxAttachment struct {
	Type    string               `json:"type"`
	Payload MaxAttachmentPayload `json:"payload"`
}

type MaxAttachmentPayload struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	ID    int64  `json:"id"`
}

type MaxCallback struct {
	CallbackID string     `json:"callback_id"`
	Message    MaxMessage `json:"message"`
}

func (m *MaxMessenger) getUpdates(ctx context.Context, marker int64) ([]MaxUpdate, int64, error) {
	url := fmt.Sprintf("%s/updates?timeout=%d&limit=100", maxAPIURL, maxUpdatesTimeout)
	if marker > 0 {
		url += fmt.Sprintf("&marker=%d", marker)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", m.token)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("max API error (%s): %s", resp.Status, string(body))
	}

	var rawResponse struct {
		Updates []json.RawMessage `json:"updates"`
		Marker  int64             `json:"marker"`
	}
	if err := json.Unmarshal(body, &rawResponse); err != nil {
		return nil, 0, err
	}

	updates := make([]MaxUpdate, 0, len(rawResponse.Updates))
	for _, raw := range rawResponse.Updates {
		if m.debug {
			slog.Debug("Raw update JSON", "json", string(raw))
		}
		var update MaxUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			slog.Warn("Failed to unmarshal update", "error", err, "raw", string(raw))
			continue
		}
		updates = append(updates, update)
	}

	return updates, rawResponse.Marker, nil
}

const maxDedupAge = 5 * time.Minute

func (m *MaxMessenger) dedupMid(mid string) bool {
	if mid == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts, ok := m.seenMids[mid]; ok && time.Since(ts) < maxDedupAge {
		return true // duplicate
	}
	m.seenMids[mid] = time.Now()
	return false
}

func (m *MaxMessenger) cleanupSeenMids() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-maxDedupAge)
	for mid, ts := range m.seenMids {
		if ts.Before(cutoff) {
			delete(m.seenMids, mid)
		}
	}
}

func (m *MaxMessenger) handleUpdate(ctx context.Context, update *MaxUpdate) {
	msg := update.Message
	if msg == nil && len(update.Payload) > 0 {
		var wrapper struct {
			Message *MaxMessage `json:"message"`
		}
		if err := json.Unmarshal(update.Payload, &wrapper); err == nil && wrapper.Message != nil {
			msg = wrapper.Message
			if m.debug {
				slog.Debug("Found message inside payload wrapper")
			}
		} else {
			if err := json.Unmarshal(update.Payload, &msg); err != nil {
				if m.debug {
					slog.Debug("Failed to unmarshal payload", "error", err)
				}
			}
		}
	}
	if msg == nil {
		if update.UpdateType == "message_created" || update.UpdateType == "message_callback" {
			slog.Warn("Max: update has nil message and no payload", "type", update.UpdateType)
		}
		return
	}

	// dedup: Max API retries can deliver the same update multiple times
	if m.dedupMid(msg.Body.Mid) {
		slog.Debug("Max: duplicate mid, skipping", "mid", msg.Body.Mid)
		return
	}

	if m.debug {
		slog.Debug("Processing update", "type", update.UpdateType, "timestamp", update.Timestamp, "mid", msg.Body.Mid, "sender_id", msg.Sender.UserID)
	}

	if msg.Link != nil {
		if m.debug {
			slog.Debug("Link found in message", "type", msg.Link.Type)
		}
		if msg.Link.Type == "forward" {
			if m.tryAttachments(ctx, msg, msg.Link.Message.Attachments) {
				return
			}
			if m.tryAttachments(ctx, msg, msg.Link.Message.Body.Attachments) {
				return
			}
		}
	}

	if msg.Body.Text == "/start" {
		m.handleStart(ctx, msg)
		return
	}

	if m.tryAttachments(ctx, msg, msg.Body.Attachments) {
		return
	}

	if m.tryAttachments(ctx, msg, msg.Attachments) {
		return
	}

	if m.debug {
		slog.Debug("No audio/voice/video attachments found")
	}
}

func (m *MaxMessenger) tryAttachments(ctx context.Context, msg *MaxMessage, attachments []MaxAttachment) bool {
	for _, attachment := range attachments {
		if m.debug {
			slog.Debug("Attachment", "type", attachment.Type)
		}
		if attachment.Type == "audio" || attachment.Type == "voice" {
			m.handleAudioAttachment(ctx, msg, attachment)
			return true
		}
		if attachment.Type == "video" {
			m.handleVideoAttachment(ctx, msg, attachment)
			return true
		}
	}
	return false
}

func (m *MaxMessenger) handleStart(ctx context.Context, msg *MaxMessage) {
	if _, err := m.SendMessage(ctx, strconv.FormatInt(msg.Recipient.ChatID, 10), "", m.messages.StartMessage); err != nil {
		slog.Warn("Max: failed to send start message", "error", err)
	}
}

func (m *MaxMessenger) handleAudioAttachment(ctx context.Context, msg *MaxMessage, attachment MaxAttachment) {
	duration := 0
	if m.ffprobePath != "" {
		var err error
		duration, err = ffprobeDuration(ctx, m.ffprobePath, attachment.Payload.URL)
		if err != nil && m.debug {
			slog.Debug("ffprobe failed for audio", "error", err)
		}
	}
	event := &IncomingEvent{
		Type:      EventIncomingVoice,
		ChatID:    strconv.FormatInt(msg.Recipient.ChatID, 10),
		MessageID: msg.Body.Mid,
		FileID:    attachment.Payload.URL,
		UserID:    strconv.FormatInt(msg.Sender.UserID, 10),
		Timestamp: time.Now(),
		Messenger: MessengerMax,
		IsMP3:     true,
		Duration:  duration,
	}
	m.eventHandler(ctx, event)
}

func (m *MaxMessenger) handleVideoAttachment(ctx context.Context, msg *MaxMessage, attachment MaxAttachment) {
	duration := 0
	if m.ffprobePath != "" {
		var err error
		duration, err = ffprobeDuration(ctx, m.ffprobePath, attachment.Payload.URL)
		if err != nil && m.debug {
			slog.Debug("ffprobe failed for video", "error", err)
		}
	}
	event := &IncomingEvent{
		Type:      EventIncomingVideo,
		ChatID:    strconv.FormatInt(msg.Recipient.ChatID, 10),
		MessageID: msg.Body.Mid,
		FileID:    attachment.Payload.URL,
		UserID:    strconv.FormatInt(msg.Sender.UserID, 10),
		Timestamp: time.Now(),
		Messenger: MessengerMax,
		IsMP3:     false,
		Duration:  duration,
	}
	m.eventHandler(ctx, event)
}

func (m *MaxMessenger) SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error) {
	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid chat ID: %v", err)
	}

	apiURL := fmt.Sprintf("%s/messages?chat_id=%d", maxAPIURL, chatIDInt)

	requestBody := map[string]interface{}{
		"text": text,
	}

	if replyTo != "" {
		requestBody["link"] = map[string]interface{}{
			"type": "reply",
			"mid":  replyTo,
		}
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode == 400 && strings.Contains(string(body), "Unknown recipient") {
		apiURL = fmt.Sprintf("%s/messages?user_id=%d", maxAPIURL, chatIDInt)
		req, err = http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonBody))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", m.token)
		req.Header.Set("Content-Type", "application/json")

		resp, err = m.httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("max API error (%s): %s", resp.Status, string(body))
	}

	var response struct {
		Message struct {
			Body struct {
				Mid string `json:"mid"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}

	if response.Message.Body.Mid == "" {
		return "", fmt.Errorf("max API error: %s", string(body))
	}

	return response.Message.Body.Mid, nil
}

func (m *MaxMessenger) UpdateMessage(ctx context.Context, chatID, messageID, text string, formatted bool) error {
	if formatted {
		text = m.formatText(text)
	}

	apiURL := fmt.Sprintf("%s/messages?message_id=%s", maxAPIURL, messageID)

	requestBody := map[string]interface{}{
		"text": text,
	}
	if formatted {
		requestBody["format"] = "html"
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", apiURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("max API error (%s): %s", resp.Status, string(body))
	}

	return nil
}

func (m *MaxMessenger) formatText(text string) string {
	paragraphs := strings.Split(text, "\n\n")
	if len(paragraphs) <= 1 {
		return fmt.Sprintf("<i>%s</i>", text)
	}

	var builder strings.Builder
	for i, para := range paragraphs {
		if i == 0 {
			builder.WriteString(strings.TrimSpace(para))
		} else {
			lines := strings.Split(strings.TrimSpace(para), "\n")
			for _, line := range lines {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					builder.WriteString("\n• ")
					builder.WriteString(trimmed)
				}
			}
		}
	}

	return fmt.Sprintf("<i>%s</i>", builder.String())
}

func (m *MaxMessenger) GetFile(ctx context.Context, fileID string) (*FileInfo, error) {
	return &FileInfo{
		FilePath: fileID,
		FileSize: 0,
	}, nil
}

func (m *MaxMessenger) DownloadFile(ctx context.Context, filePath string) (string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", filePath, nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := m.downloadClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("max download error: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	return filePath, data, nil
}
