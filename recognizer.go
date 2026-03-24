package main

import (
	"context"
)

// SpeechRecognizer defines the interface for a speech recognition service.
type SpeechRecognizer interface {
	Recognize(ctx context.Context, audioData []byte) (string, error)
}
