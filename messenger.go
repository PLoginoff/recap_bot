package main

import "context"

type MessengerType string

// constructor: NewMessenger(token string, messages ConfigMessages, eventHandler EventHandler, debug bool)

type MessengerClient interface {
	Start(ctx context.Context) error
	SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error)
	UpdateMessage(ctx context.Context, chatID, messageID, text string, formatted bool) error
	GetFile(ctx context.Context, fileID string) (*FileInfo, error)
	DownloadFile(ctx context.Context, filePath string) (string, []byte, error)
}

type MessengerWithInline interface {
	MessengerClient
	AnswerInlineQuery(ctx context.Context, inlineQueryID, text string) error
}

type FileInfo struct {
	FilePath string
	FileSize int64
}
