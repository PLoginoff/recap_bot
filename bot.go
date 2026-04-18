package main

import (
	"context"
	"fmt"
	"log"
)

// Bot represents a bot instance with its configuration and messenger client.
// It separates business logic (prompts, messages) from transport layer (messenger).
type Bot struct {
	ID           string
	Prompt       string
	Messages     ConfigMessages
	messenger    MessengerClient
	rateLimiter  RateLimiter
	eventHandler EventHandler
	hub          *Hub
}

// NewBot creates a new bot instance with embedded messenger client.

func NewBot(id string, cfg ConfigBot, globalMessages ConfigMessages, hub *Hub, rateLimiter RateLimiter, debug bool) *Bot {
	bot := &Bot{
		ID:           id,
		Prompt:       cfg.Prompt,
		Messages:     globalMessages,
		rateLimiter:  rateLimiter,
		eventHandler: nil,
		hub:          hub,
	}

	switch cfg.Messenger {
	case MessengerTelegram:
		bot.messenger = NewTelegramMessenger(cfg.Token, globalMessages, bot.EventHandler, debug)
	case MessengerMax:
		bot.messenger = NewMaxMessenger(cfg.Token, globalMessages, bot.EventHandler, debug)
	}

	return bot
}

func (b *Bot) HandleEvent(ctx context.Context, event *IncomingEvent) {
	if event == nil {
		return
	}
	event.BotID = b.ID
	if b.hub != nil {
		b.hub.HandleEvent(ctx, event)
	}
}

// EventHandler implements EventHandler interface
func (b *Bot) EventHandler(ctx context.Context, event *IncomingEvent) {
	b.HandleEvent(ctx, event)
}

func (b *Bot) CheckRateLimit(ctx context.Context, event *IncomingEvent) bool {
	if b.rateLimiter == nil {
		return true
	}
	if event.UserID == "" {
		return true
	}
	if b.rateLimiter.IsAllowed(event.UserID) {
		return true
	}
	if event.ChatID != "" {
		_, _ = b.Messenger().SendMessage(ctx, event.ChatID, event.MessageID, b.Messages.RateLimitMessage)
	}
	return false
}

// Start begins the messenger polling.
func (b *Bot) Start(ctx context.Context) error {
	if b.messenger == nil {
		return nil
	}
	return b.messenger.Start(ctx)
}

// SendStatus sends a status message and returns message ID or empty string on error
func (b *Bot) SendStatus(ctx context.Context, chatID, replyTo, text string) string {
	if b.messenger == nil {
		return ""
	}
	messageID, err := b.messenger.SendMessage(ctx, chatID, replyTo, text)
	if err != nil {
		log.Printf("Failed to send status message: %v", err)
		return ""
	}
	return messageID
}

func (b *Bot) AnswerInlineQuery(ctx context.Context, inlineQueryID, text string) error {
	responder, ok := b.messenger.(MessengerWithInline)
	if !ok {
		return fmt.Errorf("messenger does not support inline queries")
	}
	return responder.AnswerInlineQuery(ctx, inlineQueryID, text)
}

// Messenger returns the underlying messenger client for API operations.
func (b *Bot) Messenger() MessengerClient {
	return b.messenger
}
