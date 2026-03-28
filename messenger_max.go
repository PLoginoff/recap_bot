// Max Messenger client implementation
// State-mandated messenger in Russia for government oversight
// Documentation: https://dev.max.ru/
// Go SDK: https://github.com/max-messenger/max-bot-api-client-go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

const MessengerMax MessengerType = "max"

const maxAPIURL = "https://platform-api.max.ru"
const maxUploadURL = "https://upload-api.max.ru"

type MaxMessenger struct {
	token       string
	taskQueue   chan *RecapTask
	rateLimiter RateLimiter
	messages    ConfigMessages
	httpClient  *http.Client
}

func NewMaxMessenger(token string, taskQueue chan *RecapTask, rateLimiter RateLimiter, messages ConfigMessages) *MaxMessenger {
	return &MaxMessenger{
		token:       token,
		taskQueue:   taskQueue,
		rateLimiter: rateLimiter,
		messages:    messages,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (m *MaxMessenger) Start(ctx context.Context) error {
	// Start long polling for updates
	go m.pollUpdates(ctx)
	return nil
}

func (m *MaxMessenger) pollUpdates(ctx context.Context) {
	var lastUpdateID int64 = 0
	
	for {
		select {
		case <-ctx.Done():
			return
		default:
			updates, err := m.getUpdates(ctx, lastUpdateID)
			if err != nil {
				log.Printf("Max: error getting updates: %v", err)
				time.Sleep(3 * time.Second)
				continue
			}
			
			for _, update := range updates {
				lastUpdateID = update.UpdateID
				m.handleUpdate(ctx, update)
			}
			
			time.Sleep(500 * time.Millisecond)
		}
	}
}

type MaxUpdate struct {
	UpdateID   int64           `json:"update_id"`
	Message    *MaxMessage     `json:"message"`
	Callback   *MaxCallback    `json:"callback"`
}

type MaxMessage struct {
	MessageID int64           `json:"message_id"`
	ChatID    int64           `json:"chat_id"`
	UserID    int64           `json:"user_id"`
	Text      string          `json:"text"`
	Audio     *MaxAudio       `json:"audio"`
	Voice     *MaxVoice       `json:"voice"`
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

type MaxCallback struct {
	CallbackID string     `json:"callback_id"`
	Message    MaxMessage `json:"message"`
}

func (m *MaxMessenger) getUpdates(ctx context.Context, offset int64) ([]MaxUpdate, error) {
	url := fmt.Sprintf("%s/updates?offset=%d&timeout=30", maxAPIURL, offset)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", m.token)
	
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var response struct {
		OK      bool        `json:"ok"`
		Updates []MaxUpdate `json:"updates"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	
	if !response.OK {
		return nil, fmt.Errorf("max API error: %s", string(body))
	}
	
	return response.Updates, nil
}

func (m *MaxMessenger) handleUpdate(ctx context.Context, update MaxUpdate) {
	if update.Message == nil {
		return
	}
	
	msg := update.Message
	
	// Handle voice/audio messages
	if msg.Voice != nil {
		m.handleVoiceMessage(ctx, msg)
	} else if msg.Audio != nil {
		m.handleAudioMessage(ctx, msg)
	} else if msg.Text == "/start" {
		m.handleStart(ctx, msg)
	}
}

func (m *MaxMessenger) handleStart(ctx context.Context, msg *MaxMessage) {
	if _, err := m.SendMessage(ctx, strconv.FormatInt(msg.ChatID, 10), "", m.messages.StartMessage); err != nil {
		log.Printf("Max: failed to send start message: %v", err)
	}
}

func (m *MaxMessenger) handleVoiceMessage(ctx context.Context, msg *MaxMessage) {
	if !m.rateLimiter.IsAllowed(strconv.FormatInt(msg.UserID, 10)) {
		m.SendMessage(ctx, strconv.FormatInt(msg.ChatID, 10), "", "Rate limit exceeded. Please try again later.")
		return
	}
	
	// Send initial status message
	statusMsgID, err := m.SendMessage(ctx, strconv.FormatInt(msg.ChatID, 10), "", m.messages.Listening)
	if err != nil {
		log.Printf("Max: failed to send status message: %v", err)
		return
	}
	
	task := &RecapTask{
		Messenger:       MessengerMax,
		ChatID:          strconv.FormatInt(msg.ChatID, 10),
		MessageID:       strconv.FormatInt(msg.MessageID, 10),
		FileID:          msg.Voice.FileID,
		Status:          StatusDownload,
		StatusMessageID: statusMsgID,
		StatusText:      m.messages.Listening,
		IsVideoNote:     false,
	}
	
	m.taskQueue <- task
}

func (m *MaxMessenger) handleAudioMessage(ctx context.Context, msg *MaxMessage) {
	if !m.rateLimiter.IsAllowed(strconv.FormatInt(msg.UserID, 10)) {
		m.SendMessage(ctx, strconv.FormatInt(msg.ChatID, 10), "", "Rate limit exceeded. Please try again later.")
		return
	}
	
	// Send initial status message
	statusMsgID, err := m.SendMessage(ctx, strconv.FormatInt(msg.ChatID, 10), "", m.messages.Listening)
	if err != nil {
		log.Printf("Max: failed to send status message: %v", err)
		return
	}
	
	task := &RecapTask{
		Messenger:       MessengerMax,
		ChatID:          strconv.FormatInt(msg.ChatID, 10),
		MessageID:       strconv.FormatInt(msg.MessageID, 10),
		FileID:          msg.Audio.FileID,
		Status:          StatusDownload,
		StatusMessageID: statusMsgID,
		StatusText:      m.messages.Listening,
		IsVideoNote:     false,
	}
	
	m.taskQueue <- task
}

func (m *MaxMessenger) SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error) {
	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid chat ID: %v", err)
	}
	
	requestBody := map[string]interface{}{
		"chat_id": chatIDInt,
		"text":    text,
	}
	
	if replyTo != "" {
		replyToInt, err := strconv.ParseInt(replyTo, 10, 64)
		if err == nil {
			requestBody["reply_to_message_id"] = replyToInt
		}
	}
	
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}
	
	req, err := http.NewRequestWithContext(ctx, "POST", maxAPIURL+"/messages", bytes.NewBuffer(jsonBody))
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
	
	var response struct {
		OK      bool `json:"ok"`
		Message struct {
			MessageID int64 `json:"message_id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}
	
	if !response.OK {
		return "", fmt.Errorf("max API error: %s", string(body))
	}
	
	return strconv.FormatInt(response.Message.MessageID, 10), nil
}

func (m *MaxMessenger) UpdateMessage(ctx context.Context, chatID, messageID, text string) error {
	chatIDInt, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat ID: %v", err)
	}
	
	messageIDInt, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid message ID: %v", err)
	}
	
	requestBody := map[string]interface{}{
		"chat_id":    chatIDInt,
		"message_id": messageIDInt,
		"text":       text,
	}
	
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	
	req, err := http.NewRequestWithContext(ctx, "PUT", maxAPIURL+"/messages", bytes.NewBuffer(jsonBody))
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
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	
	var response struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	
	if !response.OK {
		return fmt.Errorf("max API error: %s", string(body))
	}
	
	return nil
}

func (m *MaxMessenger) GetFile(ctx context.Context, fileID string) (*FileInfo, error) {
	url := fmt.Sprintf("%s/files/%s/info", maxAPIURL, fileID)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", m.token)
	
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	var response struct {
		OK   bool `json:"ok"`
		File struct {
			FileID   string `json:"file_id"`
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		} `json:"file"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	
	if !response.OK {
		return nil, fmt.Errorf("max API error: %s", string(body))
	}
	
	return &FileInfo{
		FilePath: response.File.FilePath,
		FileSize: response.File.FileSize,
	}, nil
}

func (m *MaxMessenger) DownloadFile(ctx context.Context, filePath string) (string, []byte, error) {
	url := fmt.Sprintf("%s/files/%s", maxAPIURL, filePath)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", m.token)
	
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	
	return filePath, data, nil
}
