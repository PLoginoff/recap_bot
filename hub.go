package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

type TaskStatus string

const (
	StatusDownload TaskStatus = "download"
	StatusSTT      TaskStatus = "stt"
	StatusRecap    TaskStatus = "recap"
	StatusSent     TaskStatus = "sent"
	StatusDone     TaskStatus = "done"
)

type Task struct {
	BotID           string        `json:"bot_id"`
	Messenger       MessengerType `json:"messenger"`
	ChatID          string        `json:"chat_id"`
	MessageID       string        `json:"message_id"`
	FileID          string        `json:"file_id"`
	Status          TaskStatus    `json:"status"`
	Text            string        `json:"text"`
	Summary         string        `json:"summary"`
	AudioData       []byte        `json:"audio_data"`
	IsVideoNote     bool          `json:"is_video_note"`
	IsMP3           bool          `json:"is_mp3"`
	StatusMessageID string        `json:"status_message_id"`
	StatusText      string        `json:"status_text"`
	Wait            time.Duration `json:"wait"`
	ErrorCount      int           `json:"error_count"`
	InlineQueryID   string        `json:"inline_query_id"`
}

type Hub struct {
	bots           map[string]*Bot
	recognizer     SpeechRecognizer
	openrouter     *OpenRouterClient
	taskQueue      chan *Task
	ffmpegPath     string
	saveDebugMedia bool
}

func NewHub(bots map[string]*Bot, recognizer SpeechRecognizer, openrouterClient *OpenRouterClient, ffmpegPath string, saveDebugMedia bool) (*Hub, error) {
	taskQueue := make(chan *Task, 100)
	return &Hub{
		bots:           bots,
		recognizer:     recognizer,
		openrouter:     openrouterClient,
		taskQueue:      taskQueue,
		ffmpegPath:     ffmpegPath,
		saveDebugMedia: saveDebugMedia,
	}, nil
}

// createVoiceTask creates a task for voice messages
func (h *Hub) createVoiceTask(event *IncomingEvent, bot *Bot) *Task {
	task := &Task{
		BotID:       event.BotID,
		Messenger:   event.Messenger,
		ChatID:      event.ChatID,
		MessageID:   event.MessageID,
		FileID:      event.FileID,
		Status:      StatusDownload,
		IsVideoNote: false,
		IsMP3:       event.IsMP3,
		StatusText:  bot.Messages.Listening,
	}
	return task
}

// createVideoTask creates a task for video messages
func (h *Hub) createVideoTask(event *IncomingEvent, bot *Bot) *Task {
	task := &Task{
		BotID:       event.BotID,
		Messenger:   event.Messenger,
		ChatID:      event.ChatID,
		MessageID:   event.MessageID,
		FileID:      event.FileID,
		Status:      StatusDownload,
		IsVideoNote: true,
		IsMP3:       event.IsMP3,
		StatusText:  bot.Messages.Listening,
	}
	return task
}

// createInlineQueryTask creates a task for inline queries
func (h *Hub) createInlineQueryTask(event *IncomingEvent) *Task {
	return &Task{
		BotID:         event.BotID,
		Messenger:     event.Messenger,
		ChatID:        event.ChatID,
		MessageID:     event.MessageID,
		FileID:        "",
		Status:        StatusRecap,
		Wait:          0,
		AudioData:     nil,
		Text:          event.Text,
		IsVideoNote:   false,
		InlineQueryID: event.InlineQueryID,
	}
}

// sendStatusForTask sends status message and updates task
func (h *Hub) sendStatusForTask(ctx context.Context, task *Task, bot *Bot, statusText string) {
	if messageID := bot.SendStatus(ctx, task.ChatID, task.MessageID, statusText); messageID != "" {
		task.StatusMessageID = messageID
	}
}

// HandleEvent implements EventSink interface
func (h *Hub) HandleEvent(ctx context.Context, event *IncomingEvent) {
	bot, ok := h.bots[event.BotID]
	if !ok {
		log.Printf("No bot found for ID %s", event.BotID)
		return
	}
	if !bot.CheckRateLimit(ctx, event) {
		return
	}

	var task *Task

	switch event.Type {
	case EventIncomingVoice:
		task = h.createVoiceTask(event, bot)
		h.sendStatusForTask(ctx, task, bot, bot.Messages.Listening)

	case EventIncomingVideo:
		task = h.createVideoTask(event, bot)
		h.sendStatusForTask(ctx, task, bot, bot.Messages.Listening)

	case EventInlineQuery:
		task = h.createInlineQueryTask(event)

	default:
		log.Printf("Unknown event type: %s", event.Type)
		return
	}

	if task != nil {
		select {
		case h.taskQueue <- task:
		default:
			log.Printf("Task queue full, dropping task for bot %s", task.BotID)
			// Notify user that bot is overloaded
			if task.ChatID != "" {
				bot.SendStatus(ctx, task.ChatID, task.MessageID, "Бот перегружен, попробуйте позже")
			}
		}
	}
}

// GetTaskQueue returns the task queue for workers
func (h *Hub) GetTaskQueue() chan *Task {
	return h.taskQueue
}

func (h *Hub) Start(ctx context.Context) {
	log.Printf("Starting hub with %d bots...", len(h.bots))

	for _, bot := range h.bots {
		go func(b *Bot) {
			if err := b.Start(ctx); err != nil {
				log.Printf("Bot client error: %v", err)
			}
		}(bot)
	}

	<-ctx.Done()
	log.Printf("Context cancelled, stopping bot: %v", ctx.Err())
}

func (h *Hub) getBot(botID string) (*Bot, error) {
	bot, ok := h.bots[botID]
	if !ok {
		return nil, fmt.Errorf("no bot found for ID %s", botID)
	}
	return bot, nil
}

func (h *Hub) getPromptForBot(botID string) string {
	bot, ok := h.bots[botID]
	if !ok {
		return ""
	}
	return bot.Prompt
}

func (h *Hub) UpdateMessageForTask(ctx context.Context, task *Task, text string, formatted bool) error {
	bot, err := h.getBot(task.BotID)
	if err != nil {
		return err
	}
	return bot.Messenger().UpdateMessage(ctx, task.ChatID, task.StatusMessageID, text, formatted)
}

func (h *Hub) DownloadFileForTask(ctx context.Context, task *Task) (string, []byte, error) {
	bot, err := h.getBot(task.BotID)
	if err != nil {
		return "", nil, err
	}

	// Get file info first
	fileInfo, err := bot.Messenger().GetFile(ctx, task.FileID)
	if err != nil {
		return "", nil, err
	}

	// Download file using filePath
	return bot.Messenger().DownloadFile(ctx, fileInfo.FilePath)
}

func (h *Hub) Recognize(ctx context.Context, audioData []byte) (string, error) {
	return h.recognizer.Recognize(ctx, audioData)
}

func (h *Hub) Summarize(ctx context.Context, text string, botID string) (string, error) {
	prompt := h.getPromptForBot(botID)
	return h.openrouter.Summarize(ctx, text, prompt)
}

func (h *Hub) addDotToStatus(ctx context.Context, task *Task) {
	if task.StatusMessageID != "" {
		newStatus := task.StatusText + "."
		if err := h.UpdateMessageForTask(ctx, task, newStatus, false); err != nil {
			log.Printf("Failed to add dot to status message: %v", err)
		} else {
			task.StatusText = newStatus
		}
	}
}

func (h *Hub) notifyFailure(ctx context.Context, task *Task) {
	if task.StatusMessageID != "" {
		bot, err := h.getBot(task.BotID)
		if err != nil {
			return
		}
		if err := h.UpdateMessageForTask(ctx, task, bot.Messages.FailureMessage, false); err != nil {
			log.Printf("Failed to notify failure: %v", err)
		}
	}
}
