package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const MessengerTelegram MessengerType = "telegram"

const telegramAPIURL = "https://api.telegram.org/bot%s/sendMessage"
const telegramFileURL = "https://api.telegram.org/file/bot%s/%s"
const telegramGetFileURL = "https://api.telegram.org/bot%s/getFile"
const telegramEditMessageURL = "https://api.telegram.org/bot%s/editMessageText"
const telegramAnswerInlineQueryURL = "https://api.telegram.org/bot%s/answerInlineQuery"

const telegramHTTPTimeout = 30 * time.Second
const telegramDownloadTimeout = 120 * time.Second

type TelegramMessenger struct {
	token          string
	bot            *telegrambot.Bot
	eventHandler   EventHandler
	messages       ConfigMessages
	httpClient     *http.Client
	downloadClient *http.Client
	debug          bool
}

func NewTelegramMessenger(token string, messages ConfigMessages, eventHandler EventHandler, debug bool) *TelegramMessenger {
	return &TelegramMessenger{
		token:          token,
		messages:       messages,
		eventHandler:   eventHandler,
		debug:          debug,
		httpClient:     &http.Client{Timeout: telegramHTTPTimeout},
		downloadClient: &http.Client{Timeout: telegramDownloadTimeout},
	}
}

func (tc *TelegramMessenger) Start(ctx context.Context) error {
	opts := []telegrambot.Option{
		telegrambot.WithMessageTextHandler("/start", telegrambot.MatchTypeExact, tc.handleStart),
		telegrambot.WithDefaultHandler(tc.handleAllMessages),
	}

	tc.bot = telegrambot.New(tc.token, opts...)
	tc.bot.Start(ctx)

	return nil
}

func (tc *TelegramMessenger) SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error) {
	apiURL := fmt.Sprintf(telegramAPIURL, tc.token)
	data := url.Values{}
	data.Set("chat_id", chatID)
	if replyTo != "" {
		data.Set("reply_to_message_id", replyTo)
	}
	data.Set("text", text)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var apiResponse struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return "", err
	}
	if !apiResponse.OK {
		return "", fmt.Errorf("telegram API error: %s", string(body))
	}

	return strconv.Itoa(apiResponse.Result.MessageID), nil
}

func (tc *TelegramMessenger) UpdateMessage(ctx context.Context, chatID, messageID, text string, formatted bool) error {
	if formatted {
		text = tc.formatText(text)
	}

	apiURL := fmt.Sprintf(telegramEditMessageURL, tc.token)
	data := url.Values{}
	data.Set("chat_id", chatID)
	data.Set("message_id", messageID)
	data.Set("text", text)

	// Add HTML parsing only for final result
	if formatted {
		data.Set("parse_mode", "HTML")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var apiResponse struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return err
	}
	if !apiResponse.OK {
		return fmt.Errorf("telegram API error: %s", string(body))
	}

	return nil
}

// formatText formats text for Telegram messenger (expandable blockquote for long text)
func (tc *TelegramMessenger) formatText(text string) string {
	// Use expandable blockquote for multi-paragraph text
	paragraphs := strings.Split(text, "\n\n")
	if len(paragraphs) <= 1 {
		return html.EscapeString(text)
	}

	return fmt.Sprintf("<blockquote expandable>%s</blockquote>", html.EscapeString(text))
}

func (tc *TelegramMessenger) DownloadFile(ctx context.Context, filePath string) (string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(telegramFileURL, tc.token, filePath), nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := tc.downloadClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("telegram download error: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	return filePath, data, nil
}

func (tc *TelegramMessenger) GetFile(ctx context.Context, fileID string) (*FileInfo, error) {
	apiURL := fmt.Sprintf(telegramGetFileURL, tc.token)
	data := url.Values{}
	data.Set("file_id", fileID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResponse struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, err
	}
	if !apiResponse.OK {
		return nil, fmt.Errorf("telegram API error: %s", string(body))
	}

	return &FileInfo{
		FilePath: apiResponse.Result.FilePath,
		FileSize: apiResponse.Result.FileSize,
	}, nil
}

func (tc *TelegramMessenger) handleStart(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	if tc.debug {
		log.Printf("[TG] Received /start from chat %d", update.Message.Chat.ID)
	}
	if _, err := tc.SendMessage(ctx, strconv.Itoa(update.Message.Chat.ID), "", tc.messages.StartMessage); err != nil {
		log.Printf("Failed to send start message: %v", err)
	}
}

func (tc *TelegramMessenger) handleAllMessages(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	if tc.debug && update.Message != nil {
		log.Printf("[TG] Incoming message from %d", update.Message.From.ID)
	}
	if update.Message != nil && update.Message.Voice != nil {
		tc.handleVoiceMessage(ctx, bot, update)
		return
	}
	if update.Message != nil && update.Message.VideoNote != nil {
		tc.handleVideoNote(ctx, bot, update)
		return
	}
	if update.InlineQuery != nil {
		tc.handleInlineQuery(ctx, bot, update)
		return
	}
}

func (tc *TelegramMessenger) handleVoiceMessage(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	voice := update.Message.Voice
	if voice == nil {
		return
	}
	if tc.debug {
		log.Printf("[TG] Voice message from %d, duration: %d sec, file_id: %s", update.Message.From.ID, voice.Duration, voice.FileID)
	}
	userID := strconv.Itoa(update.Message.From.ID)

	// Send event instead of creating Task
	event := &IncomingEvent{
		Type:      EventIncomingVoice,
		ChatID:    strconv.Itoa(update.Message.Chat.ID),
		MessageID: strconv.Itoa(update.Message.ID),
		FileID:    voice.FileID,
		UserID:    userID,
		Timestamp: time.Now(),
		Messenger: MessengerTelegram,
		IsMP3:     false, // Telegram sends OGG
	}
	tc.eventHandler(ctx, event)
}

func (tc *TelegramMessenger) handleVideoNote(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	video := update.Message.VideoNote
	if video == nil {
		return
	}
	if tc.debug {
		log.Printf("[TG] Video note from %d, duration: %d sec, file_id: %s", update.Message.From.ID, video.Duration, video.FileID)
	}
	userID := strconv.Itoa(update.Message.From.ID)

	// Send event instead of creating Task
	event := &IncomingEvent{
		Type:      EventIncomingVideo,
		ChatID:    strconv.Itoa(update.Message.Chat.ID),
		MessageID: strconv.Itoa(update.Message.ID),
		FileID:    video.FileID,
		UserID:    userID,
		Timestamp: time.Now(),
		Messenger: MessengerTelegram,
		IsMP3:     false, // Video notes are MP4
	}
	tc.eventHandler(ctx, event)
}

func (tc *TelegramMessenger) handleInlineQuery(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	query := update.InlineQuery.Query
	if query == "" {
		return
	}
	if tc.debug {
		log.Printf("[TG] Inline query from %d: %q", update.InlineQuery.From.ID, query)
	}
	userID := strconv.Itoa(update.InlineQuery.From.ID)

	// Send event instead of creating Task
	event := &IncomingEvent{
		Type:          EventInlineQuery,
		ChatID:        strconv.Itoa(update.InlineQuery.From.ID),
		MessageID:     "", // Inline queries don't have message IDs
		Text:          query,
		UserID:        userID,
		Timestamp:     time.Now(),
		InlineQueryID: update.InlineQuery.ID,
		Messenger:     MessengerTelegram,
		IsMP3:         false, // No audio
	}
	tc.eventHandler(ctx, event)
}

func (tc *TelegramMessenger) AnswerInlineQuery(ctx context.Context, inlineQueryID, text string) error {
	apiURL := fmt.Sprintf(telegramAnswerInlineQueryURL, tc.token)
	formatted := tc.formatText(text)
	result := map[string]interface{}{
		"type":  "article",
		"id":    "recap",
		"title": "Recap",
		"input_message_content": map[string]string{
			"message_text": formatted,
			"parse_mode":   "HTML",
		},
	}
	payload := map[string]interface{}{
		"inline_query_id": inlineQueryID,
		"results":         []interface{}{result},
		"cache_time":      0,
		"is_personal":     true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tc.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var apiResponse struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &apiResponse); err != nil {
		return err
	}
	if !apiResponse.OK {
		return fmt.Errorf("telegram API error: %s", apiResponse.Description)
	}

	return nil
}
