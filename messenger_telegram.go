package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const MessengerTelegram MessengerType = "telegram"

const telegramAPIURL = "https://api.telegram.org/bot%s/sendMessage"
const telegramFileURL = "https://api.telegram.org/file/bot%s/%s"
const telegramGetFileURL = "https://api.telegram.org/bot%s/getFile"
const telegramEditMessageURL = "https://api.telegram.org/bot%s/editMessageText"

type TelegramMessenger struct {
	token       string
	bot         *telegrambot.Bot
	taskQueue   chan<- *RecapTask
	rateLimiter RateLimiter
	saveDebug   bool
	messages    ConfigMessages
}

type TelegramMessage struct {
	ChatID      string
	MessageID   string
	FileID      string
	Text        string
	UserID      string
	IsVoice     bool
	IsVideoNote bool
}

func NewTelegramMessenger(token string, taskQueue chan<- *RecapTask, rateLimiter RateLimiter, saveDebug bool, messages ConfigMessages) *TelegramMessenger {
	return &TelegramMessenger{
		token:       token,
		taskQueue:   taskQueue,
		rateLimiter: rateLimiter,
		saveDebug:   saveDebug,
		messages:    messages,
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

	resp, err := http.PostForm(apiURL, data)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var apiResponse struct {
		OK          bool `json:"ok"`
		Result      struct {
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

func (tc *TelegramMessenger) UpdateMessage(ctx context.Context, chatID, messageID, text string) error {
	apiURL := fmt.Sprintf(telegramEditMessageURL, tc.token)
	data := url.Values{}
	data.Set("chat_id", chatID)
	data.Set("message_id", messageID)
	data.Set("text", text)
	data.Set("parse_mode", "HTML")

	resp, err := http.PostForm(apiURL, data)
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

func (tc *TelegramMessenger) DownloadFile(ctx context.Context, filePath string) (string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(telegramFileURL, tc.token, filePath), nil)
	if err != nil {
		return "", nil, err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
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

func (tc *TelegramMessenger) GetFile(ctx context.Context, fileID string) (*FileInfo, error) {
	apiURL := fmt.Sprintf(telegramGetFileURL, tc.token)
	data := url.Values{}
	data.Set("file_id", fileID)

	resp, err := http.PostForm(apiURL, data)
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
	if _, err := tc.SendMessage(ctx, strconv.Itoa(update.Message.Chat.ID), "", tc.messages.StartMessage); err != nil {
		log.Printf("Failed to send start message: %v", err)
	}
}

func (tc *TelegramMessenger) handleAllMessages(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
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

	userID := strconv.Itoa(update.Message.From.ID)

	if !tc.rateLimiter.IsAllowed(userID) {
		log.Printf("Rate limit exceeded for user %s", userID)
		return
	}

	task := &RecapTask{
		Messenger:     MessengerTelegram,
		ChatID:        strconv.Itoa(update.Message.Chat.ID),
		MessageID:     strconv.Itoa(update.Message.ID),
		FileID:        voice.FileID,
		Status:        StatusDownload,
		IsVideoNote:   false,
	}
	if tc.saveDebug {
		task.DebugDir = "voice_audio"
		task.DebugExt = "ogg"
		task.RawDebugDir = "voice_audio"
		task.RawDebugExt = "ogg"
	}

	statusText := tc.messages.Listening
	if messageID, err := tc.SendMessage(ctx, task.ChatID, task.MessageID, statusText); err != nil {
		log.Printf("Failed to send thinking message: %v", err)
	} else {
		task.StatusMessageID = messageID
		task.StatusText = statusText
	}

	tc.taskQueue <- task
}

func (tc *TelegramMessenger) handleVideoNote(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	video := update.Message.VideoNote
	if video == nil {
		return
	}

	userID := strconv.Itoa(update.Message.From.ID)

	if !tc.rateLimiter.IsAllowed(userID) {
		log.Printf("Rate limit exceeded for user %s", userID)
		return
	}

	task := &RecapTask{
		Messenger:     MessengerTelegram,
		ChatID:        strconv.Itoa(update.Message.Chat.ID),
		MessageID:     strconv.Itoa(update.Message.ID),
		FileID:        video.FileID,
		Status:        StatusDownload,
		IsVideoNote:   true,
	}
	if tc.saveDebug {
		task.DebugDir = "video_note_audio"
		task.DebugExt = "ogg"
		task.RawDebugDir = "video_note_raw"
		task.RawDebugExt = "mp4"
	}

	statusText := tc.messages.Listening
	if messageID, err := tc.SendMessage(ctx, task.ChatID, task.MessageID, statusText); err != nil {
		log.Printf("Failed to send thinking message for video note: %v", err)
	} else {
		task.StatusMessageID = messageID
		task.StatusText = statusText
	}

	tc.taskQueue <- task
}

func (tc *TelegramMessenger) handleInlineQuery(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	query := update.InlineQuery.Query
	if query == "" {
		return
	}

	userID := strconv.Itoa(update.InlineQuery.From.ID)

	if !tc.rateLimiter.IsAllowed(userID) {
		log.Printf("Rate limit exceeded for user %s", userID)
		return
	}

	task := &RecapTask{
		Messenger:   MessengerTelegram,
		ChatID:      "", // Inline queries don't have a chat
		MessageID:   "",
		FileID:      "",
		Status:      StatusRecap,
		Wait:        0,
		AudioData:   nil,
		Text:        query,
		IsVideoNote: false,
	}

	tc.taskQueue <- task
}
