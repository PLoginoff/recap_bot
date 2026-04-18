package main

import (
	"context"
	"time"
)

type EventType string

const (
	EventIncomingText  EventType = "incoming_text"
	EventIncomingVoice EventType = "incoming_voice"
	EventIncomingVideo EventType = "incoming_video"
	EventInlineQuery   EventType = "inline_query"
)

type IncomingEvent struct {
	Type          EventType `json:"type"`
	BotID         string    `json:"bot_id"`
	ChatID        string    `json:"chat_id"`
	MessageID     string    `json:"message_id"`
	Text          string    `json:"text,omitempty"`
	FileID        string    `json:"file_id,omitempty"`
	UserID        string    `json:"user_id"`
	Timestamp     time.Time `json:"timestamp"`
	InlineQueryID string    `json:"inline_query_id,omitempty"`

	// Transport flags filled by messenger
	Messenger MessengerType `json:"messenger"`
	IsMP3     bool          `json:"is_mp3"`
}

type EventHandler func(ctx context.Context, event *IncomingEvent)
