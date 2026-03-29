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
}

type Hub struct {
	bots           map[string]MessengerClient // BotID -> messenger
	botConfigs     map[string]ConfigBot       // BotID -> config
	recognizer     SpeechRecognizer
	openrouter     *OpenRouterClient
	taskQueue      <-chan *Task
	ffmpegPath     string
	saveDebugMedia bool
	messages       ConfigMessages
}

func NewHub(bots map[string]MessengerClient, botConfigs map[string]ConfigBot, recognizer SpeechRecognizer, openrouterClient *OpenRouterClient, taskQueue <-chan *Task, ffmpegPath string, saveDebugMedia bool, messages ConfigMessages) (*Hub, error) {
	return &Hub{
		bots:           bots,
		botConfigs:     botConfigs,
		recognizer:     recognizer,
		openrouter:     openrouterClient,
		taskQueue:      taskQueue,
		ffmpegPath:     ffmpegPath,
		saveDebugMedia: saveDebugMedia,
		messages:       messages,
	}, nil
}

func (h *Hub) Start(ctx context.Context) {
	log.Printf("Starting hub with %d bots...", len(h.bots))

	for _, bot := range h.bots {
		go func(m MessengerClient) {
			if err := m.Start(ctx); err != nil {
				log.Printf("Messenger client error: %v", err)
			}
		}(bot)
	}

	<-ctx.Done()
	log.Printf("Context cancelled, stopping bot: %v", ctx.Err())
}

func (h *Hub) getBot(botID string) (MessengerClient, error) {
	bot, ok := h.bots[botID]
	if !ok {
		return nil, fmt.Errorf("no bot found for ID %s", botID)
	}
	return bot, nil
}

func (h *Hub) getBotConfig(botID string) ConfigBot {
	config, _ := h.botConfigs[botID]
	return config
}

func (h *Hub) getPromptForBot(botID string) string {
	config := h.getBotConfig(botID)
	return config.Prompt
}

func (h *Hub) UpdateMessageForTask(ctx context.Context, task *Task, text string, formatted bool) error {
	bot, err := h.getBot(task.BotID)
	if err != nil {
		return err
	}
	return bot.UpdateMessage(ctx, task.ChatID, task.StatusMessageID, text, formatted)
}

func (h *Hub) DownloadFileForTask(ctx context.Context, task *Task) (string, []byte, error) {
	bot, err := h.getBot(task.BotID)
	if err != nil {
		return "", nil, err
	}

	// Get file info first
	fileInfo, err := bot.GetFile(ctx, task.FileID)
	if err != nil {
		return "", nil, err
	}

	// Download file using filePath
	return bot.DownloadFile(ctx, fileInfo.FilePath)
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
		if err := h.UpdateMessageForTask(ctx, task, h.messages.FailureMessage, false); err != nil {
			log.Printf("Failed to notify failure: %v", err)
		}
	}
}
