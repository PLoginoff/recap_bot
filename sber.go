package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const sberUploadURL = "https://smartspeech.sber.ru/rest/v1/data:upload"

type SberTokenConfig struct {
	Name         string        `yaml:"name"`
	ClientID     string        `yaml:"client_id"`
	ClientSecret string        `yaml:"client_secret"`
	Cooldown     time.Duration `yaml:"cooldown"`
	Limit        time.Duration `yaml:"limit"`
}

type SberConfig struct {
	Tokens []SberTokenConfig `yaml:"tokens"`
}

type SberClient struct {
	config     SberConfig
	store      *StateStore
	httpClient *http.Client
}

type authResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"`
}

type uploadResponse struct {
	Result struct {
		FileID string `json:"request_file_id"`
	} `json:"result"`
}

type sberAsyncRecognizeRequest struct {
	Options struct {
		Model                 string `json:"model"`
		AudioEncoding         string `json:"audio_encoding"`
		SampleRate            int    `json:"sample_rate"`
		Language              string `json:"language"`
		EnableProfanityFilter bool   `json:"enable_profanity_filter"`
		HypothesesCount       int    `json:"hypotheses_count"`
		NoSpeechTimeout       string `json:"no_speech_timeout"`
		MaxSpeechTimeout      string `json:"max_speech_timeout"`
	} `json:"options"`
	RequestFileID string `json:"request_file_id"`
}

type sberAsyncRecognizeResponse struct {
	Result struct {
		TaskID string `json:"id"`
	} `json:"result"`
}

type sberTaskStatusResponse struct {
	State  int `json:"status"`
	Result struct {
		Status         string `json:"status"`
		ID             string `json:"id"`
		ResponseFileID string `json:"response_file_id"`
	} `json:"result"`
}

type sberSpeechResult struct {
	Results []struct {
		Text           string `json:"text"`
		NormalizedText string `json:"normalized_text"`
	} `json:"results"`
}

func previewErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	preview := strings.TrimSpace(string(body))
	const maxPreviewLen = 500
	if len(preview) > maxPreviewLen {
		preview = preview[:maxPreviewLen] + "..."
	}
	return preview
}

func NewSberClient(config SberConfig, store *StateStore) *SberClient {
	normalized := make([]SberTokenConfig, 0, len(config.Tokens))
	for _, token := range config.Tokens {
		normalized = append(normalized, normalizeTokenConfig(token))
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	return &SberClient{
		config: SberConfig{Tokens: normalized},
		store:  store,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   2 * time.Minute,
		},
	}
}

func normalizeTokenConfig(cfg SberTokenConfig) SberTokenConfig {
	result := cfg
	if result.Cooldown == 0 {
		result.Cooldown = 10 * time.Minute
	}
	if result.Limit == 0 {
		result.Limit = 15 * time.Minute
	}
	return result
}

type sberTokenSelection struct {
	Config   SberTokenConfig
	Defaults ResourceDefaults
	StateKey string
}

type sberCooldownError struct {
	ResumeAt time.Time
}

func (e sberCooldownError) Error() string {
	if e.ResumeAt.IsZero() {
		return "no Sber tokens available"
	}
	return fmt.Sprintf("no Sber tokens available until %s", e.ResumeAt.Format(time.RFC3339))
}

type sberTemporaryError struct {
	Err error
}

func (e sberTemporaryError) Error() string {
	return e.Err.Error()
}

func (e sberTemporaryError) Unwrap() error {
	return e.Err
}

func isSberTemporary(err error) bool {
	var tempErr sberTemporaryError
	return errors.As(err, &tempErr)
}

func isSberAuthFatal(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "403")
}

func (c *SberClient) Recognize(ctx context.Context, audioData []byte) (result string, err error) {
	if len(c.config.Tokens) == 0 {
		return "", errors.New("no Sber tokens configured")
	}

	selection, err := c.selectToken(time.Now())
	if err != nil {
		return "", err
	}

	start := time.Now()
	defer func() {
		if c.store == nil || selection.StateKey == "" {
			return
		}
		if err != nil && isSberTemporary(err) {
			return
		}
		usage := time.Since(start)
		if _, releaseErr := c.store.Release("sber", selection.StateKey, usage, selection.Defaults, err == nil, time.Now()); releaseErr != nil {
			log.Printf("Sber: failed to release token %s: %v", selection.StateKey, releaseErr)
		}
	}()

	accessToken, err := c.authenticate(ctx, selection.Config)
	if err != nil {
		if isSberAuthFatal(err) {
			return "", err
		}
		return "", sberTemporaryError{Err: err}
	}

	fileID, err := c.uploadAudioData(ctx, accessToken, audioData)
	if err != nil {
		return "", sberTemporaryError{Err: err}
	}

	taskID, err := c.startAsyncRecognition(ctx, accessToken, fileID)
	if err != nil {
		return "", sberTemporaryError{Err: err}
	}

	resultFileID, err := c.waitForResult(ctx, accessToken, taskID)
	if err != nil {
		return "", sberTemporaryError{Err: err}
	}

	result, err = c.getResult(ctx, accessToken, resultFileID)
	if err != nil {
		return "", sberTemporaryError{Err: err}
	}

	return result, nil
}

func (c *SberClient) selectToken(now time.Time) (sberTokenSelection, error) {
	if c.store == nil {
		cfg := c.config.Tokens[0]
		return sberTokenSelection{
			Config:   cfg,
			Defaults: ResourceDefaults{Cooldown: cfg.Cooldown, Limit: cfg.Limit},
		}, nil
	}

	var (
		errMessages []string
		nextResume  time.Time
	)

	for _, token := range c.config.Tokens {
		defaults := ResourceDefaults{Cooldown: token.Cooldown, Limit: token.Limit}
		key := c.tokenStateKey(token)
		status, available, err := c.store.Acquire("sber", key, defaults, now)
		if err != nil {
			errMessages = append(errMessages, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		pausedUntil := "none"
		if !status.PausedUntil.IsZero() {
			pausedUntil = status.PausedUntil.Format(time.RFC3339)
		}
		log.Printf("Sber token %s: available=%t paused_until=%s used=%s window=%s cooldown=%s", key, available, pausedUntil, status.UsedSeconds, status.Window, status.Cooldown)
		if !available {
			if !status.PausedUntil.IsZero() && (nextResume.IsZero() || status.PausedUntil.Before(nextResume)) {
				nextResume = status.PausedUntil
			}
			continue
		}
		return sberTokenSelection{
			Config:   token,
			Defaults: defaults,
			StateKey: key,
		}, nil
	}

	if len(errMessages) > 0 {
		return sberTokenSelection{}, fmt.Errorf("no Sber tokens available: %s", strings.Join(errMessages, "; "))
	}
	if !nextResume.IsZero() {
		return sberTokenSelection{}, sberCooldownError{ResumeAt: nextResume}
	}
	return sberTokenSelection{}, errors.New("no Sber tokens available")
}

func (c *SberClient) tokenStateKey(token SberTokenConfig) string {
	if token.Name != "" {
		return token.Name
	}
	if token.ClientID != "" {
		return token.ClientID
	}
	return "token"
}

func (c *SberClient) authenticate(ctx context.Context, token SberTokenConfig) (string, error) {
	if token.ClientSecret == "" {
		return "", errors.New("sber token missing credential")
	}

	payload := strings.NewReader("scope=SALUTE_SPEECH_PERS")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://ngw.devices.sberbank.ru:9443/api/v2/oauth", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create auth request: %w", err)
	}

	var basic string
	if token.ClientID == "" {
		if _, err := base64.StdEncoding.DecodeString(token.ClientSecret); err != nil {
			return "", fmt.Errorf("sber token requires either client_id+client_secret pair or base64 credentials: %w", err)
		}
		basic = token.ClientSecret
		log.Printf("Sber: authenticating with pre-encoded credentials")
	} else {
		basic = base64.StdEncoding.EncodeToString([]byte(token.ClientID + ":" + token.ClientSecret))
		log.Printf("Sber: authenticating with client_id=%s", token.ClientID)
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("RqUID", uuid.New().String())
	req.Header.Add("Authorization", "Basic "+basic)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute auth request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read auth response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sber auth error (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var auth authResponse
	if err := json.Unmarshal(body, &auth); err != nil {
		return "", fmt.Errorf("failed to unmarshal auth response: %w", err)
	}

	log.Printf("Sber: authentication successful, token expires at %v", time.Unix(0, auth.ExpiresAt*int64(time.Millisecond)))
	return auth.AccessToken, nil
}

func (c *SberClient) uploadAudioData(ctx context.Context, accessToken string, audioData []byte) (string, error) {
	if len(audioData) == 0 {
		return "", errors.New("no audio data to upload")
	}

	const previewBytes = 16
	preview := audioData
	if len(preview) > previewBytes {
		preview = preview[:previewBytes]
	}

	sum := sha256.Sum256(audioData)
	log.Printf("Sber: upload payload size=%d bytes, sha256=%s, head=%s", len(audioData), hex.EncodeToString(sum[:]), strings.ToUpper(hex.EncodeToString(preview)))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sberUploadURL, bytes.NewReader(audioData))
	if err != nil {
		return "", fmt.Errorf("failed to create upload request: %w", err)
	}

	req.Header.Add("Content-Type", "audio/ogg;codecs=opus")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute upload request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read upload response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sber upload error (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var upload uploadResponse
	if err := json.Unmarshal(body, &upload); err != nil {
		return "", fmt.Errorf("failed to unmarshal upload response: %w", err)
	}

	if upload.Result.FileID == "" {
		return "", errors.New("empty file id returned by upload API")
	}

	log.Printf("Sber: uploaded file id %s", upload.Result.FileID)
	return upload.Result.FileID, nil
}

func (c *SberClient) startAsyncRecognition(ctx context.Context, accessToken, fileID string) (string, error) {
	request := sberAsyncRecognizeRequest{RequestFileID: fileID}
	request.Options.Model = "general"
	request.Options.AudioEncoding = "OPUS"
	request.Options.SampleRate = 48000
	request.Options.Language = "ru-RU"
	request.Options.EnableProfanityFilter = false
	request.Options.HypothesesCount = 2
	request.Options.NoSpeechTimeout = "3s"
	request.Options.MaxSpeechTimeout = "60s"

	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal async recognition request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://smartspeech.sber.ru/rest/v1/speech:async_recognize", bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create async recognition request: %w", err)
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute async recognition request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read async recognition response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sber async recognition error (%s): %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	var result sberAsyncRecognizeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to unmarshal async recognition response: %w", err)
	}

	if result.Result.TaskID == "" {
		return "", errors.New("empty task id returned by async recognition API")
	}

	log.Printf("Sber: async recognition task id %s", result.Result.TaskID)
	return result.Result.TaskID, nil
}

func (c *SberClient) waitForResult(ctx context.Context, accessToken, taskID string) (string, error) {
	deadline := time.After(5 * time.Minute)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", errors.New("recognition timeout after 5 minutes")
		case <-ticker.C:
			fileID, err := c.checkTaskStatus(ctx, accessToken, taskID)
			if err != nil {
				return "", err
			}
			if fileID != "" {
				return fileID, nil
			}
		}
	}
}

func (c *SberClient) checkTaskStatus(ctx context.Context, accessToken, taskID string) (string, error) {
	url := fmt.Sprintf("https://smartspeech.sber.ru/rest/v1/task:get?id=%s", taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create task status request: %w", err)
	}

	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute task status request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read task status response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sber task status error (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}

	preview := previewErrorMessage(body)
	if preview != "" {
		log.Printf("Sber: task %s status raw: %s", taskID, preview)
	}

	var status sberTaskStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return "", fmt.Errorf("failed to unmarshal task status response: %w", err)
	}

	if strings.EqualFold(status.Result.Status, "ERROR") {
		detail := status.Result.Status
		if status.Result.ID != "" {
			detail = fmt.Sprintf("task %s", status.Result.ID)
		}
		if status.Result.ResponseFileID != "" {
			detail += fmt.Sprintf(" response=%s", status.Result.ResponseFileID)
		}
		return "", fmt.Errorf("sber task error (%s): %s", detail, preview)
	}

	if status.Result.Status == "DONE" && status.Result.ResponseFileID != "" {
		log.Printf("Sber: task %s completed, file id %s", taskID, status.Result.ResponseFileID)
		return status.Result.ResponseFileID, nil
	}

	return "", nil
}

func (c *SberClient) getResult(ctx context.Context, accessToken, fileID string) (string, error) {
	url := fmt.Sprintf("https://smartspeech.sber.ru/rest/v1/data:download?response_file_id=%s", fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create download request: %w", err)
	}

	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute download request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read download response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sber download error (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var results []sberSpeechResult
	if err := json.Unmarshal(body, &results); err != nil {
		return "", fmt.Errorf("failed to unmarshal download response: %w", err)
	}

	var normalized []string
	for _, result := range results {
		for _, item := range result.Results {
			if item.NormalizedText != "" {
				normalized = append(normalized, item.NormalizedText)
			}
		}
	}

	if len(normalized) == 0 {
		return "", errors.New("empty recognition result from Sber")
	}

	return strings.Join(normalized, " "), nil
}
