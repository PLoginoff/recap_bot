package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type TaskStatus string

const (
	StatusDownload TaskStatus = "download"
	StatusSTT      TaskStatus = "stt"
	StatusRecap    TaskStatus = "recap"
	StatusSent     TaskStatus = "sent"
	StatusDone     TaskStatus = "done"
)

type RecapTask struct {
	ChatID          int
	MessageID       int
	FileID          string
	Status          TaskStatus
	Wait            time.Duration
	AudioData       []byte
	Text            string
	Summary         string
	ErrorCount      int
	IsVideoNote     bool
	StatusMessageID int
	StatusText      string
	DebugDir        string
	DebugExt        string
	DebugAudioSaved bool
	RawDebugDir     string
	RawDebugExt     string
	RawDebugSaved   bool
}


type Bot struct {
	token          string
	recognizer     SpeechRecognizer
	openrouter     *OpenRouterClient
	httpClient     *http.Client
	downloadClient *http.Client // separate client for file downloads
	taskQueue      chan<- *RecapTask
	userRequests   map[int64][]time.Time
	mu             sync.Mutex
	rateLimit      int
	rateLimitTime  time.Duration
	ffmpegPath     string
	saveDebugMedia bool
	messages       ConfigMessages
}

func NewBot(config BotConfig, recognizer SpeechRecognizer, openrouterClient *OpenRouterClient, taskQueue chan<- *RecapTask, ffmpegPath string, saveDebugMedia bool) (*Bot, error) {
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
	}
	
	downloadClient := &http.Client{
		Timeout: 300 * time.Second, // 5 minutes
	}

	return &Bot{
		token:          config.TelegramToken,
		recognizer:     recognizer,
		openrouter:     openrouterClient,
		httpClient:     httpClient,
		downloadClient: downloadClient,
		taskQueue:      taskQueue,
		userRequests:   make(map[int64][]time.Time),
		rateLimit:      10,
		rateLimitTime:  time.Hour,
		ffmpegPath:     ffmpegPath,
		saveDebugMedia: saveDebugMedia,
		messages:       config.Messages,
	}, nil
}

func (b *Bot) Start(ctx context.Context) {
	opts := []telegrambot.Option{
		telegrambot.WithMessageTextHandler("/start", telegrambot.MatchTypeExact, b.handleStart),
		telegrambot.WithDefaultHandler(b.handleAllMessages),
	}

	tgBot := telegrambot.New(b.token, opts...)

	log.Printf("Starting Telegram bot...")
	go func() {
		tgBot.Start(ctx)
	}()

	<-ctx.Done()
	log.Printf("Context cancelled, stopping bot: %v", ctx.Err())
}

func (b *Bot) handleStart(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	if _, err := b.SendMessage(ctx, update.Message.Chat.ID, update.Message.ID, b.messages.StartMessage); err != nil {
		log.Printf("Failed to send start message: %v", err)
	}
}

func (b *Bot) handleAllMessages(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	if update.Message != nil && update.Message.Voice != nil {
		b.handleVoiceMessage(ctx, bot, update)
		return
	}

	if update.Message != nil && update.Message.VideoNote != nil {
		b.handleVideoNote(ctx, bot, update)
		return
	}

	if update.InlineQuery != nil {
		b.handleInlineQuery(ctx, bot, update)
		return
	}
}

func (b *Bot) handleVoiceMessage(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	voice := update.Message.Voice
	if voice == nil {
		return
	}

	userID := update.Message.From.ID

	if !b.isAllowed(int64(userID)) {
		log.Printf("Rate limit exceeded for user %d", userID)
		return
	}

	task := &RecapTask{
		ChatID:      update.Message.Chat.ID,
		MessageID:   update.Message.ID,
		FileID:      voice.FileID,
		Status:      StatusDownload,
		IsVideoNote: false,
	}
	if b.saveDebugMedia {
		task.DebugDir = "voice_audio"
		task.DebugExt = "ogg"
		task.RawDebugDir = "voice_audio"
		task.RawDebugExt = "ogg"
	}

	statusText := b.messages.Listening
	if messageID, err := b.SendMessage(ctx, task.ChatID, task.MessageID, statusText); err != nil {
		log.Printf("Failed to send thinking message: %v", err)
	} else {
		task.StatusMessageID = messageID
		task.StatusText = statusText
	}

	b.taskQueue <- task
}

func (b *Bot) handleVideoNote(ctx context.Context, bot *telegrambot.Bot, update *models.Update) {
	video := update.Message.VideoNote
	if video == nil {
		return
	}

	userID := update.Message.From.ID

	if !b.isAllowed(int64(userID)) {
		log.Printf("Rate limit exceeded for user %d", userID)
		return
	}

	task := &RecapTask{
		ChatID:      update.Message.Chat.ID,
		MessageID:   update.Message.ID,
		FileID:      video.FileID,
		Status:      StatusDownload,
		IsVideoNote: true,
	}
	if b.saveDebugMedia {
		task.DebugDir = "video_note_audio"
		task.DebugExt = "ogg"
		task.RawDebugDir = "video_note_raw"
		task.RawDebugExt = "mp4"
	}

	statusText := b.messages.Listening
	if messageID, err := b.SendMessage(ctx, task.ChatID, task.MessageID, statusText); err != nil {
		log.Printf("Failed to send thinking message for video note: %v", err)
	} else {
		task.StatusMessageID = messageID
		task.StatusText = statusText
	}

	b.taskQueue <- task
}

func (b *Bot) handleInlineQuery(ctx context.Context, bBot *telegrambot.Bot, update *models.Update) {
	query := update.InlineQuery.Query
	if query == "" {
		return
	}

	userID := update.InlineQuery.From.ID

	if !b.isAllowed(int64(userID)) {
		log.Printf("Rate limit exceeded for user %d", userID)
		return
	}

	task := &RecapTask{
		ChatID:      0, // Inline queries don't have a chat
		MessageID:   0,
		FileID:      "",
		Status:      StatusRecap,
		Wait:        0,
		AudioData:   nil,
		Text:        query,
		IsVideoNote: false,
	}

	b.taskQueue <- task
}

func (b *Bot) isAllowed(userID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	requests := b.userRequests[userID]

	// Remove old requests
	var recentRequests []time.Time
	for _, t := range requests {
		if now.Sub(t) < b.rateLimitTime {
			recentRequests = append(recentRequests, t)
		}
	}

	if len(recentRequests) >= b.rateLimit {
		return false
	}

	recentRequests = append(recentRequests, now)
	b.userRequests[userID] = recentRequests
	return true
}

func (b *Bot) Recognize(ctx context.Context, audioData []byte) (string, error) {
	return b.recognizer.Recognize(ctx, audioData)
}

func (b *Bot) Summarize(ctx context.Context, text string) (string, error) {
	return b.openrouter.Summarize(ctx, text)
}

func (b *Bot) notifyFailure(ctx context.Context, task *RecapTask) {
	if task.StatusMessageID != 0 {
		if err := b.UpdateMessage(ctx, task.ChatID, task.StatusMessageID, b.messages.FailureMessage); err != nil {
			log.Printf("Failed to update failure message: %v", err)
			if _, sendErr := b.SendMessage(ctx, task.ChatID, task.MessageID, b.messages.FailureMessage); sendErr != nil {
				log.Printf("Failed to send failure message: %v", sendErr)
			}
		}
	} else {
		if _, err := b.SendMessage(ctx, task.ChatID, task.MessageID, b.messages.FailureMessage); err != nil {
			log.Printf("Failed to send failure message: %v", err)
		}
	}
}
