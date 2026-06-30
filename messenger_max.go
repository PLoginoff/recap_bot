// Max Messenger client implementation
// State-mandated messenger in Russia for government oversight
// Documentation: https://dev.max.ru/
// Go SDK: https://github.com/max-messenger/max-bot-api-client-go
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
	"time"
)

const MessengerMax MessengerType = "max"

const maxAPIURL = "https://platform-api2.max.ru"

// Skip TLS verification for Max API (required for Russian Trusted CA not in default pool)
const maxSkipTLSVerify = true

const maxRetryDelay = 3 * time.Second
const maxDownloadTimeout = 120 * time.Second
const maxPollInterval = 2 * time.Second
const maxHTTPTimeout = 35 * time.Second
const maxUpdatesTimeout = 29 // seconds for long polling, less than prev

type MaxMessenger struct {
	token          string
	eventHandler   EventHandler
	messages       ConfigMessages
	httpClient     *http.Client
	downloadClient *http.Client
	debug          bool
}

func NewMaxMessenger(token string, messages ConfigMessages, eventHandler EventHandler, debug bool) *MaxMessenger {
	return &MaxMessenger{
		token:        token,
		messages:     messages,
		eventHandler: eventHandler,
		debug:        debug,
		httpClient: &http.Client{
			Timeout: maxHTTPTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: maxSkipTLSVerify},
			},
		},
		downloadClient: &http.Client{
			Timeout: maxDownloadTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: maxSkipTLSVerify},
			},
		},
	}
}

func (m *MaxMessenger) Start(ctx context.Context) error {
	// Start long polling for updates
	go m.pollUpdates(ctx)
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
				m.handleUpdate(ctx, update)
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
	Payload    json.RawMessage `json:"payload"` // fallback for unknown structures
}

type MaxMessage struct {
	Mid       string         `json:"mid"`
	Recipient MaxRecipient   `json:"recipient"`
	Sender    MaxSender      `json:"sender"`
	Timestamp int64          `json:"timestamp"`
	Body      MaxMessageBody `json:"body"`
	// Attachments at message level (for forwarded messages)
	Attachments []MaxAttachment `json:"attachments,omitempty"`
	// Link for forwarded/replied messages
	Link *MaxLink `json:"link,omitempty"`
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

type MaxAudio struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Duration int    `json:"duration"`
}

type MaxVoice struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Duration int    `json:"duration"`
}

type MaxVideoNote struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Duration int    `json:"duration"`
}

type MaxCallback struct {
	CallbackID string     `json:"callback_id"`
	Message    MaxMessage `json:"message"`
}

func (m *MaxMessenger) getUpdates(ctx context.Context, marker int64) ([]MaxUpdate, int64, error) {
	url := fmt.Sprintf("%s/updates?marker=%d&timeout=%d&limit=100&types=message_created,message_callback,bot_started", maxAPIURL, marker, maxUpdatesTimeout)

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

	slog.Debug("Raw API response", "response", string(body))

	var response struct {
		Updates []MaxUpdate `json:"updates"`
		Marker  int64       `json:"marker"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, 0, err
	}

	return response.Updates, response.Marker, nil
}

func (m *MaxMessenger) handleUpdate(ctx context.Context, update MaxUpdate) {
	slog.Debug("Processing update", "type", update.UpdateType, "message_nil", update.Message == nil, "payload_len", len(update.Payload))

	msg := update.Message
	if msg == nil && len(update.Payload) > 0 {
		if err := json.Unmarshal(update.Payload, &msg); err != nil {
			slog.Debug("Failed to unmarshal payload as message", "error", err)
		}
	}
	if msg == nil {
		if update.UpdateType == "message_created" || update.UpdateType == "message_callback" {
			slog.Warn("Max: update has nil message and no payload", "type", update.UpdateType)
		}
		return
	}

	// Check if this is a forwarded message via link field
	if msg.Link != nil {
		slog.Debug("Link found in message", "type", msg.Link.Type)
		if msg.Link.Type == "forward" {
			slog.Debug("Forwarded message detected via link", "link", msg.Link)

			// Check attachments in the linked message at message level
			if len(msg.Link.Message.Attachments) > 0 {
				slog.Debug("Found attachments at message level in forwarded message", "count", len(msg.Link.Message.Attachments))
				for i, attachment := range msg.Link.Message.Attachments {
					slog.Debug("Forwarded attachment", "index", i, "type", attachment.Type, "payload", attachment.Payload)
					if attachment.Type == "audio" || attachment.Type == "voice" {
						slog.Debug("Found audio/voice attachment in forwarded message")
						m.handleAudioAttachment(ctx, msg, attachment)
						return
					}
					if attachment.Type == "video" {
						slog.Debug("Found video attachment in forwarded message")
						m.handleVideoAttachment(ctx, msg, attachment)
						return
					}
				}
			}

			// Also check attachments in the linked message body
			if len(msg.Link.Message.Body.Attachments) > 0 {
				slog.Debug("Found attachments in body of forwarded message", "count", len(msg.Link.Message.Body.Attachments))
				for i, attachment := range msg.Link.Message.Body.Attachments {
					slog.Debug("Forwarded body attachment", "index", i, "type", attachment.Type, "payload", attachment.Payload)
					if attachment.Type == "audio" || attachment.Type == "voice" {
						slog.Debug("Found audio/voice attachment in forwarded message body")
						m.handleAudioAttachment(ctx, msg, attachment)
						return
					}
					if attachment.Type == "video" {
						slog.Debug("Found video attachment in forwarded message body")
						m.handleVideoAttachment(ctx, msg, attachment)
						return
					}
				}
			}
		}
	}

	slog.Debug("Message body structure", "body", msg.Body)

	// Handle text commands
	if msg.Body.Text == "/start" {
		m.handleStart(ctx, msg)
		return
	}

	// Handle attachments (voice/audio/video) in main message
	slog.Debug("Checking attachments in main message", "count", len(msg.Body.Attachments))
	for i, attachment := range msg.Body.Attachments {
		slog.Debug("Attachment", "index", i, "type", attachment.Type, "payload", attachment.Payload)
		if attachment.Type == "audio" || attachment.Type == "voice" {
			slog.Debug("Found audio/voice attachment in main message")
			m.handleAudioAttachment(ctx, msg, attachment)
			return
		}
		if attachment.Type == "video" {
			slog.Debug("Found video attachment in main message")
			m.handleVideoAttachment(ctx, msg, attachment)
			return
		}
	}

	// Also check attachments at message level (for some message types)
	if len(msg.Attachments) > 0 {
		slog.Debug("Found attachments at message level", "count", len(msg.Attachments))
		for i, attachment := range msg.Attachments {
			slog.Debug("Message level attachment", "index", i, "type", attachment.Type, "payload", attachment.Payload)
			if attachment.Type == "audio" || attachment.Type == "voice" {
				slog.Debug("Found audio/voice attachment at message level")
				m.handleAudioAttachment(ctx, msg, attachment)
				return
			}
			if attachment.Type == "video" {
				slog.Debug("Found video attachment at message level")
				m.handleVideoAttachment(ctx, msg, attachment)
				return
			}
		}
	}

	slog.Debug("No audio/voice/video attachments found in message, link, or body")
}

func (m *MaxMessenger) handleStart(ctx context.Context, msg *MaxMessage) {
	if _, err := m.SendMessage(ctx, strconv.FormatInt(msg.Recipient.ChatID, 10), "", m.messages.StartMessage); err != nil {
		slog.Warn("Max: failed to send start message", "error", err)
	}
}

func (m *MaxMessenger) handleAudioAttachment(ctx context.Context, msg *MaxMessage, attachment MaxAttachment) {
	// Send event instead of creating Task
	event := &IncomingEvent{
		Type:      EventIncomingVoice,
		ChatID:    strconv.FormatInt(msg.Recipient.ChatID, 10),
		MessageID: msg.Body.Mid,
		FileID:    attachment.Payload.URL,
		UserID:    strconv.FormatInt(msg.Sender.UserID, 10),
		Timestamp: time.Now(),
		Messenger: MessengerMax,
		IsMP3:     true, // Max sends MP3
	}
	m.eventHandler(ctx, event)
}

func (m *MaxMessenger) handleVideoAttachment(ctx context.Context, msg *MaxMessage, attachment MaxAttachment) {
	// Send event instead of creating Task
	event := &IncomingEvent{
		Type:      EventIncomingVideo,
		ChatID:    strconv.FormatInt(msg.Recipient.ChatID, 10),
		MessageID: msg.Body.Mid,
		FileID:    attachment.Payload.URL,
		UserID:    strconv.FormatInt(msg.Sender.UserID, 10),
		Timestamp: time.Now(),
		Messenger: MessengerMax,
		IsMP3:     false, // Video notes are MP4
	}
	m.eventHandler(ctx, event)
}

func (m *MaxMessenger) SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error) {
	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid chat ID: %v", err)
	}

	// According to Max API docs, user_id/chat_id should be query parameters, not in body
	url := fmt.Sprintf("%s/messages?chat_id=%d", maxAPIURL, chatIDInt)

	requestBody := map[string]interface{}{
		"text": text,
	}

	// Add reply attachment if replyTo is provided
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

	slog.Debug("SendMessage request", "url", url, "body", string(jsonBody))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
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

	slog.Debug("SendMessage response", "status", resp.Status, "body", string(body))

	// If chat_id fails, try user_id format
	if resp.StatusCode == 400 && strings.Contains(string(body), "Unknown recipient") {
		slog.Debug("Max: chat_id failed, trying user_id format")

		url = fmt.Sprintf("%s/messages?user_id=%d", maxAPIURL, chatIDInt)

		slog.Debug("SendMessage retry URL", "url", url)

		req, err = http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
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

		slog.Debug("SendMessage retry response", "status", resp.Status, "body", string(body))
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
	// Apply formatting only for final result
	if formatted {
		text = m.formatText(text)
	}

	// Max API requires message_id as query parameter
	url := fmt.Sprintf("%s/messages?message_id=%s", maxAPIURL, messageID)

	requestBody := map[string]interface{}{
		"text": text,
	}

	// Enable HTML formatting only for final result
	if formatted {
		requestBody["format"] = "html"
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	slog.Debug("UpdateMessage", "url", url, "body", string(jsonBody))

	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(jsonBody))
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

	slog.Debug("UpdateMessage response", "status", resp.Status, "body", string(body))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("max API error (%s): %s", resp.Status, string(body))
	}

	return nil
}

// formatText formats text for Max messenger (bullet points + italics, no blockquote)
func (m *MaxMessenger) formatText(text string) string {
	// Convert multi-paragraph text to bullet points
	paragraphs := strings.Split(text, "\n\n")
	if len(paragraphs) <= 1 {
		return fmt.Sprintf("<i>%s</i>", text)
	}

	var builder strings.Builder
	for i, para := range paragraphs {
		if i == 0 {
			// First paragraph as main point
			builder.WriteString(strings.TrimSpace(para))
		} else {
			// Other paragraphs as bullet points
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
	// Max API doesn't have separate get file info endpoint
	// We need to use the URL directly from the attachment payload
	// For Max, fileID is actually the URL
	return &FileInfo{
		FilePath: fileID, // This is the URL from attachment payload
		FileSize: 0,      // Unknown size
	}, nil
}

func (m *MaxMessenger) DownloadFile(ctx context.Context, filePath string) (string, []byte, error) {
	// For Max, filePath is actually the direct URL from attachment payload
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
