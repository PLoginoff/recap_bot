package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

type OpenRouterModel struct {
	Name     string        `yaml:"name"`
	Cooldown time.Duration `yaml:"cooldown"`
}

type OpenRouterConfig struct {
	APIKey       string
	Models       []OpenRouterModel
	SystemPrompt string
	UserPrompt   string
}

type OpenRouterClient struct {
	config     OpenRouterConfig
	store      *StateStore
	models     []OpenRouterModel
	httpClient *http.Client
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type OpenRouterResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func NewOpenRouterClient(config OpenRouterConfig, store *StateStore) *OpenRouterClient {
	models := make([]OpenRouterModel, 0, len(config.Models))
	for _, model := range config.Models {
		models = append(models, normalizeModelConfig(model))
	}

	return &OpenRouterClient{
		config: config,
		store:  store,
		models: models,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func normalizeModelConfig(cfg OpenRouterModel) OpenRouterModel {
	result := cfg
	if result.Cooldown == 0 {
		result.Cooldown = 10 * time.Minute
	}
	return result
}

func (c *OpenRouterClient) Summarize(ctx context.Context, text string) (string, error) {
	log.Printf("Starting summarization, %d models configured", len(c.models))
	if len(c.models) == 0 {
		return "", fmt.Errorf("no OpenRouter models configured")
	}

	var lastErr error
	for _, model := range c.models {
		result, err := c.tryModel(ctx, model, text)
		if err == nil {
			return result, nil
		}
		lastErr = err
		log.Printf("OpenRouter model %s failed: %v", model.Name, err)
	}

	if lastErr == nil {
		return "", fmt.Errorf("all OpenRouter models are unavailable")
	}

	return "", fmt.Errorf("all OpenRouter models failed: %w", lastErr)
}

func (c *OpenRouterClient) tryModel(ctx context.Context, model OpenRouterModel, text string) (string, error) {
	defaults := ResourceDefaults{
		Cooldown: model.Cooldown,
	}

	if c.store != nil {
		status, available, err := c.store.Acquire("openrouter", model.Name, defaults, time.Now())
		if err != nil {
			return "", fmt.Errorf("failed to acquire model %s: %w", model.Name, err)
		}
		if !available {
			log.Printf("OpenRouter model %s paused until %s", model.Name, status.PausedUntil.Format(time.RFC3339))
			return "", fmt.Errorf("model %s on cooldown until %s", model.Name, status.PausedUntil.Format(time.RFC3339))
		}
	}

	start := time.Now()
	result, err := c.invokeModel(ctx, model.Name, text)
	usage := time.Since(start)

	if c.store != nil {
		if _, releaseErr := c.store.Release("openrouter", model.Name, usage, defaults, err == nil, time.Now()); releaseErr != nil {
			log.Printf("Failed to update state for model %s: %v", model.Name, releaseErr)
		}
	}

	return result, err
}

func (c *OpenRouterClient) invokeModel(ctx context.Context, modelName, text string) (string, error) {
	log.Printf("OpenRouter: using model %s", modelName)
	log.Printf("Text length: %d characters", len(text))

	reqPayload := Request{
		Model: modelName,
		Messages: []Message{
			{
				Role:    "system",
				Content: c.config.SystemPrompt,
			},
			{
				Role:    "user",
				Content: fmt.Sprintf(c.config.UserPrompt, text),
			},
		},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("failed to read response body: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(responseBody))
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return "", fmt.Errorf("OpenRouter API error (%s): %s", resp.Status, snippet)
	}

	var response OpenRouterResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	result := response.Choices[0].Message.Content
	log.Printf("Summarization completed with model %s, result length: %d characters", modelName, len(result))
	return result, nil
}
