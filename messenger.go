package main

import "context"

type MessengerType string

type MessengerClient interface {
	Start(ctx context.Context) error
	SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error)
	UpdateMessage(ctx context.Context, chatID, messageID, text string, formatted bool) error
	GetFile(ctx context.Context, fileID string) (*FileInfo, error)
	DownloadFile(ctx context.Context, filePath string) (string, []byte, error)
}

type FileInfo struct {
	FilePath string
	FileSize int64
}
