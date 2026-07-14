package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// WebhookServer is a single HTTP(S) endpoint that routes webhooks to registered handlers.
type WebhookServer struct {
	listen      string
	tlsCert     string
	tlsKey      string
	secret      string
	publicURL   string   // e.g. https://77.246.247.130:8443
	handlers    sync.Map // path -> EventHandler
	rawHandlers sync.Map // path -> func(context.Context, *MaxUpdate)
	srv         *http.Server
}

// NewWebhookServer creates a shared webhook receiver.
func NewWebhookServer(listen, tlsCert, tlsKey, secret string) *WebhookServer {
	return &WebhookServer{
		listen:  listen,
		tlsCert: tlsCert,
		tlsKey:  tlsKey,
		secret:  secret,
	}
}

// SetPublicURL sets the externally visible URL (IP or domain) used when building webhook URLs.
func (w *WebhookServer) SetPublicURL(url string) {
	w.publicURL = url
}

// Register binds a path to an event handler.
func (w *WebhookServer) Register(path string, handler EventHandler) {
	w.handlers.Store(path, handler)
}

// RegisterMaxHandler binds a path to a Max Update handler.
func (w *WebhookServer) RegisterMaxHandler(path string, handler func(context.Context, *MaxUpdate)) {
	w.rawHandlers.Store(path, handler)
}

// Start runs the HTTP(S) server and returns immediately (server runs in background).
func (w *WebhookServer) Start(ctx context.Context) error {
	w.srv = &http.Server{Addr: w.listen, Handler: http.HandlerFunc(w.handleRoot)}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.srv.Shutdown(shutdownCtx)
	}()

	go func() {
		var err error
		if w.tlsCert != "" && w.tlsKey != "" {
			slog.Info("Webhook server starting (HTTPS)", "addr", w.listen)
			err = w.srv.ListenAndServeTLS(w.tlsCert, w.tlsKey)
		} else {
			slog.Info("Webhook server starting (HTTP)", "addr", w.listen)
			err = w.srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("Webhook server error", "error", err)
		}
	}()

	return nil
}

func (w *WebhookServer) handleRoot(wr http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(wr, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path

	// Verify optional global secret
	if w.secret != "" && r.Header.Get("X-Max-Bot-Api-Secret") != w.secret {
		http.Error(wr, "Unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(wr, "Bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Try raw Max handler first (preferred for Max API)
	if rawHandler, ok := w.rawHandlers.Load(path); ok {
		var update MaxUpdate
		if err := json.Unmarshal(body, &update); err != nil {
			slog.Warn("Webhook: failed to parse Max update", "error", err, "body", string(body))
			http.Error(wr, "Bad request", http.StatusBadRequest)
			return
		}
		mid := ""
		if update.Message != nil {
			mid = update.Message.Body.Mid
		}
		slog.Debug("Webhook received", "from", r.RemoteAddr, "path", path, "size", len(body), "type", update.UpdateType, "mid", mid)
		// Respond 200 OK immediately, process async to avoid Max API retry loop
		wr.WriteHeader(http.StatusOK)
		go rawHandler.(func(context.Context, *MaxUpdate))(context.Background(), &update)
		return
	}

	// Fallback to generic EventHandler
	handler, ok := w.handlers.Load(path)
	if !ok {
		slog.Warn("Webhook: no handler for path", "path", path)
		http.Error(wr, "Not found", http.StatusNotFound)
		return
	}

	var event IncomingEvent
	if err := json.Unmarshal(body, &event); err == nil && event.Messenger != "" {
		wr.WriteHeader(http.StatusOK)
		go handler.(EventHandler)(context.Background(), &event)
		return
	}
	// Try parsing as Max Update and converting
	var update MaxUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		slog.Warn("Webhook: failed to parse update", "error", err, "body", string(body))
		http.Error(wr, "Bad request", http.StatusBadRequest)
		return
	}
	wr.WriteHeader(http.StatusOK)
	go handler.(EventHandler)(context.Background(), maxUpdateToEvent(&update))
}

func maxUpdateToEvent(update *MaxUpdate) *IncomingEvent {
	event := &IncomingEvent{
		Type:      EventType(update.UpdateType),
		Messenger: MessengerMax,
	}
	if update.Message != nil {
		event.ChatID = fmt.Sprintf("%d", update.Message.Recipient.ChatID)
		event.MessageID = update.Message.Body.Mid
		event.UserID = fmt.Sprintf("%d", update.Message.Sender.UserID)
		if len(update.Message.Body.Attachments) > 0 {
			att := update.Message.Body.Attachments[0]
			if att.Type == "audio" || att.Type == "voice" {
				event.Type = EventIncomingVoice
				event.FileID = att.Payload.URL
				event.IsMP3 = true
			}
			if att.Type == "video" {
				event.Type = EventIncomingVideo
				event.FileID = att.Payload.URL
			}
		}
	}
	return event
}
