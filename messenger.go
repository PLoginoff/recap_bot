package main

import "context"

type MessengerType string

type MessengerClient interface {
	Start(ctx context.Context) error
	SendMessage(ctx context.Context, chatID, replyTo, text string) (string, error)
	UpdateMessage(ctx context.Context, chatID, messageID, text string) error
	DownloadFile(ctx context.Context, fileID string) (string, []byte, error)
	GetFile(ctx context.Context, fileID string) (*FileInfo, error)
}

type FileInfo struct {
	FilePath string
	FileSize int64
}
