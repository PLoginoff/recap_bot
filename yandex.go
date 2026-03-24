package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

type YandexConfig struct {
	APIKey   string
	FolderID string
	Name     string
	Cooldown time.Duration
	Limit    time.Duration
	Store    *StateStore
}

type YandexClient struct {
	config YandexConfig
}

func NewYandexClient(config YandexConfig) *YandexClient {
	return &YandexClient{
		config: config,
	}
}

type yandexResponse struct {
	Result string `json:"result"`
}

func (c *YandexClient) Recognize(ctx context.Context, audioData []byte) (string, error) {
	log.Printf("Sending %d bytes to Yandex SpeechKit", len(audioData))

	defaults := ResourceDefaults{
		Cooldown: c.config.Cooldown,
		Limit:    c.config.Limit,
	}

	key := c.stateKey()
	if c.config.Store != nil {
		if _, available, err := c.config.Store.Acquire("yandex", key, defaults, time.Now()); err != nil {
			return "", fmt.Errorf("failed to acquire yandex token: %w", err)
		} else if !available {
			return "", fmt.Errorf("yandex recognizer on cooldown")
		}
	}

	start := time.Now()
	success := false
	defer func() {
		if c.config.Store == nil {
			return
		}
		usage := time.Since(start)
		if _, err := c.config.Store.Release("yandex", key, usage, defaults, success, time.Now()); err != nil {
			log.Printf("Yandex: failed to update state: %v", err)
		}
	}()

	url := fmt.Sprintf("https://stt.api.cloud.yandex.net/speech/v1/stt:recognize?topic=general&lang=ru-RU%s", "")
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(audioData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Api-Key "+c.config.APIKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Yandex SpeechKit API error: %s", string(body))
	}

	var yandexResp yandexResponse
	if err := json.Unmarshal(body, &yandexResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
	}

	success = true
	return yandexResp.Result, nil
}

func (c *YandexClient) stateKey() string {
	if c.config.Name != "" {
		return c.config.Name
	}
	return "default"
}
