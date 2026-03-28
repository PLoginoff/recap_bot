package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
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

type RecapTask struct {
	Messenger       MessengerType
	ChatID          string
	MessageID       string
	FileID          string
	Status          TaskStatus
	Wait            time.Duration
	AudioData       []byte
	Text            string
	Summary         string
	ErrorCount      int
	IsVideoNote     bool
	StatusMessageID string
	StatusText      string
	DebugDir        string
	DebugExt        string
	DebugAudioSaved bool
	RawDebugDir     string
	RawDebugExt     string
	RawDebugSaved   bool
}


type Bot struct {
	messengers      map[MessengerType]MessengerClient
	recognizer     SpeechRecognizer
	openrouter     *OpenRouterClient
	httpClient     *http.Client
	downloadClient *http.Client // separate client for file downloads
	taskQueue      chan<- *RecapTask
	ffmpegPath     string
	saveDebugMedia bool
	messages       ConfigMessages
}

func NewBot(messengers map[MessengerType]MessengerClient, recognizer SpeechRecognizer, openrouterClient *OpenRouterClient, taskQueue chan<- *RecapTask, ffmpegPath string, saveDebugMedia bool, messages ConfigMessages) (*Bot, error) {
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
	}
	
	downloadClient := &http.Client{
		Timeout: 300 * time.Second, // 5 minutes
	}

	return &Bot{
		messengers:      messengers,
		recognizer:     recognizer,
		openrouter:     openrouterClient,
		httpClient:     httpClient,
		downloadClient: downloadClient,
		taskQueue:      taskQueue,
		ffmpegPath:     ffmpegPath,
		saveDebugMedia: saveDebugMedia,
		messages:       messages,
	}, nil
}

func (b *Bot) Start(ctx context.Context) {
	log.Printf("Starting bot with %d messengers...", len(b.messengers))
	
	for _, messenger := range b.messengers {
		go func(m MessengerClient) {
			if err := m.Start(ctx); err != nil {
				log.Printf("Messenger client error: %v", err)
			}
		}(messenger)
	}

	<-ctx.Done()
	log.Printf("Context cancelled, stopping bot: %v", ctx.Err())
}

func (b *Bot) getMessenger(messengerType MessengerType) (MessengerClient, error) {
	messenger, ok := b.messengers[messengerType]
	if !ok {
		return nil, fmt.Errorf("no messenger found for type %s", messengerType)
	}
	return messenger, nil
}

func (b *Bot) SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error) {
	// For direct calls, use Telegram as default
	messenger, err := b.getMessenger(MessengerTelegram)
	if err != nil {
		return "", fmt.Errorf("no messengers available")
	}
	return messenger.SendMessage(ctx, chatID, replyTo, text)
}

func (b *Bot) SendMessageWithMessenger(ctx context.Context, messengerType MessengerType, chatID, replyTo, text string) (string, error) {
	messenger, err := b.getMessenger(messengerType)
	if err != nil {
		return "", err
	}
	return messenger.SendMessage(ctx, chatID, replyTo, text)
}

func (b *Bot) SendMessageForTask(ctx context.Context, task *RecapTask, text string) (string, error) {
	messenger, err := b.getMessenger(task.Messenger)
	if err != nil {
		return "", err
	}
	return messenger.SendMessage(ctx, task.ChatID, task.MessageID, text)
}

func (b *Bot) UpdateMessage(ctx context.Context, chatID, messageID, text string) error {
	// For direct calls, use Telegram as default
	messenger, err := b.getMessenger(MessengerTelegram)
	if err != nil {
		return fmt.Errorf("no messengers available")
	}
	return messenger.UpdateMessage(ctx, chatID, messageID, text)
}

func (b *Bot) UpdateMessageWithMessenger(ctx context.Context, messengerType MessengerType, chatID, messageID, text string) error {
	messenger, err := b.getMessenger(messengerType)
	if err != nil {
		return err
	}
	return messenger.UpdateMessage(ctx, chatID, messageID, text)
}

func (b *Bot) UpdateMessageForTask(ctx context.Context, task *RecapTask, text string) error {
	messenger, err := b.getMessenger(task.Messenger)
	if err != nil {
		return err
	}
	return messenger.UpdateMessage(ctx, task.ChatID, task.StatusMessageID, text)
}

func (b *Bot) DownloadFile(ctx context.Context, filePath string) ([]byte, error) {
	// For direct calls, use Telegram as default
	messenger, err := b.getMessenger(MessengerTelegram)
	if err != nil {
		return nil, fmt.Errorf("no messengers available")
	}
	_, data, err := messenger.DownloadFile(ctx, filePath)
	return data, err
}

func (b *Bot) GetFile(ctx context.Context, fileID string) (*FileInfo, error) {
	// For direct calls, use Telegram as default
	messenger, err := b.getMessenger(MessengerTelegram)
	if err != nil {
		return nil, fmt.Errorf("no messengers available")
	}
	return messenger.GetFile(ctx, fileID)
}

func (b *Bot) DownloadFileForTask(ctx context.Context, task *RecapTask) (string, []byte, error) {
	messenger, err := b.getMessenger(task.Messenger)
	if err != nil {
		return "", nil, err
	}

	// Get file info first
	fileInfo, err := messenger.GetFile(ctx, task.FileID)
	if err != nil {
		return "", nil, err
	}

	// Download file using filePath
	return messenger.DownloadFile(ctx, fileInfo.FilePath)
}

func (b *Bot) Recognize(ctx context.Context, audioData []byte) (string, error) {
	return b.recognizer.Recognize(ctx, audioData)
}

func (b *Bot) Summarize(ctx context.Context, text string) (string, error) {
	return b.openrouter.Summarize(ctx, text)
}

func (b *Bot) addDotToStatus(ctx context.Context, task *RecapTask) {
	if task.StatusMessageID != "" {
		newStatus := task.StatusText + "."
		if err := b.UpdateMessageForTask(ctx, task, newStatus); err != nil {
			log.Printf("Failed to add dot to status message: %v", err)
		} else {
			task.StatusText = newStatus
		}
	}
}

func (b *Bot) notifyFailure(ctx context.Context, task *RecapTask) {
	if task.StatusMessageID != "" {
		if err := b.UpdateMessageForTask(ctx, task, b.messages.FailureMessage); err != nil {
			log.Printf("Failed to update failure message: %v", err)
			if _, sendErr := b.SendMessageForTask(ctx, task, b.messages.FailureMessage); sendErr != nil {
				log.Printf("Failed to send failure message: %v", sendErr)
			}
		}
	} else {
		if _, err := b.SendMessageForTask(ctx, task, b.messages.FailureMessage); err != nil {
			log.Printf("Failed to send failure message: %v", err)
		}
	}
}
