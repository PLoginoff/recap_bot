package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Yandex SpeechKit v3 endpoints. Region "kz" (default) uses yandexcloud.kz,
// region "ru" uses cloud.yandex.net.
const (
	yandexKzHost = "https://stt.api.ml.yandexcloud.kz"
	yandexRuHost = "https://stt.api.cloud.yandex.net"

	yandexDefaultModel        = "general"
	yandexDefaultPollInterval = 5 * time.Second
)

type YandexConfig struct {
	APIKey       string
	FolderID     string
	Region       string
	Model        string
	PollInterval time.Duration
}

type YandexClient struct {
	config  YandexConfig
	baseURL string
	httpCli *http.Client
}

func NewYandexClient(config YandexConfig) *YandexClient {
	if config.Region == "" {
		config.Region = "kz"
	}
	if config.Model == "" {
		config.Model = yandexDefaultModel
	}
	if config.PollInterval == 0 {
		config.PollInterval = yandexDefaultPollInterval
	}

	baseURL := yandexKzHost
	if config.Region == "ru" {
		baseURL = yandexRuHost
	}

	return &YandexClient{
		config:  config,
		baseURL: baseURL,
		httpCli: &http.Client{Timeout: 5 * time.Minute},
	}
}

// v3 recognizeFileAsync request body.
type yandexRecognizeFileRequest struct {
	Content          string                 `json:"content"`
	RecognitionModel yandexRecognitionModel `json:"recognitionModel"`
}

type yandexRecognitionModel struct {
	Model       string                   `json:"model"`
	AudioFormat yandexAudioFormatOptions `json:"audioFormat"`
}

type yandexAudioFormatOptions struct {
	ContainerAudio yandexContainerAudio `json:"containerAudio"`
}

type yandexContainerAudio struct {
	ContainerAudioType string `json:"containerAudioType"`
}

// Operation returned by recognizeFileAsync.
type yandexOperation struct {
	ID string `json:"id"`
}

// StreamingResponse chunk returned by /stt/v3/getRecognition as NDJSON stream.
type yandexStreamingChunk struct {
	Result          *yandexStreamingResult   `json:"result,omitempty"`
	Final           *yandexAlternativeUpdate `json:"final,omitempty"`
	FinalRefinement *yandexFinalRefinement   `json:"finalRefinement,omitempty"`
}

// Some deployments wrap StreamingResponse into {"result": ...}; support both.
type yandexStreamingResult struct {
	Final           *yandexAlternativeUpdate `json:"final,omitempty"`
	FinalRefinement *yandexFinalRefinement   `json:"finalRefinement,omitempty"`
}

type yandexAlternativeUpdate struct {
	Alternatives []yandexAlternative `json:"alternatives"`
	ChannelTag   string              `json:"channelTag"`
}

type yandexFinalRefinement struct {
	FinalIndex     string                   `json:"finalIndex"`
	NormalizedText *yandexAlternativeUpdate `json:"normalizedText,omitempty"`
}

type yandexAlternative struct {
	Text string `json:"text"`
}

func (c *YandexClient) Recognize(ctx context.Context, audioData []byte) (string, error) {
	if c.config.APIKey == "" {
		return "", fmt.Errorf("yandex: api_key is not configured")
	}
	if c.config.FolderID == "" {
		return "", fmt.Errorf("yandex: folder_id is not configured")
	}

	slog.Info("Yandex: submitting async recognition", "bytes", len(audioData), "model", c.config.Model)
	start := time.Now()

	opID, err := c.submitRecognition(ctx, audioData)
	if err != nil {
		return "", err
	}
	slog.Info("Yandex: operation created", "op_id", opID)

	text, err := c.fetchRecognition(ctx, opID)
	if err != nil {
		return "", err
	}

	slog.Info("Yandex: recognition complete", "op_id", opID, "text_len", len(text), "duration", time.Since(start))
	return text, nil
}

func (c *YandexClient) submitRecognition(ctx context.Context, audioData []byte) (string, error) {
	body := yandexRecognizeFileRequest{
		Content: base64.StdEncoding.EncodeToString(audioData),
		RecognitionModel: yandexRecognitionModel{
			Model: c.config.Model,
			AudioFormat: yandexAudioFormatOptions{
				ContainerAudio: yandexContainerAudio{ContainerAudioType: "OGG_OPUS"},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("yandex: marshal request: %w", err)
	}

	url := c.baseURL + "/stt/v3/recognizeFileAsync"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("yandex: create request: %w", err)
	}
	c.setAuthHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", fmt.Errorf("yandex: submit request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("yandex: read submit response: %w", err)
	}
	slog.Debug("Yandex: submit response", "status", resp.StatusCode, "body", string(respBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("yandex: recognizeFileAsync HTTP %d: %s", resp.StatusCode, previewErrorMessage(respBody))
	}

	var op yandexOperation
	if err := json.Unmarshal(respBody, &op); err != nil {
		return "", fmt.Errorf("yandex: parse operation: %w", err)
	}
	if op.ID == "" {
		return "", fmt.Errorf("yandex: empty operation id in response: %s", previewErrorMessage(respBody))
	}
	return op.ID, nil
}

// fetchRecognition calls the streaming getRecognition endpoint, which blocks
// until the operation completes and then streams NDJSON results. If the
// operation is not yet registered (HTTP 404/400), retries with poll_interval.
func (c *YandexClient) fetchRecognition(ctx context.Context, opID string) (string, error) {
	const maxWait = 30 * time.Minute
	deadline := time.Now().Add(maxWait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	url := fmt.Sprintf("%s/stt/v3/getRecognition?operation_id=%s", c.baseURL, opID)

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("yandex: getRecognition cancelled: %w", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("yandex: operation %s timed out after %s", opID, maxWait)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("yandex: create getRecognition request: %w", err)
		}
		c.setAuthHeaders(req)

		resp, err := c.httpCli.Do(req)
		if err != nil {
			return "", fmt.Errorf("yandex: getRecognition request: %w", err)
		}

		// Operation not ready yet — retry after poll interval.
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			slog.Debug("Yandex: operation not ready, retrying", "op_id", opID, "status", resp.StatusCode, "attempt", attempt, "body", previewErrorMessage(body))
			select {
			case <-time.After(c.config.PollInterval):
			case <-ctx.Done():
				return "", fmt.Errorf("yandex: getRecognition cancelled: %w", ctx.Err())
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return "", fmt.Errorf("yandex: getRecognition HTTP %d: %s", resp.StatusCode, previewErrorMessage(body))
		}

		// 200 OK — read NDJSON stream of recognition results.
		text, err := c.readRecognitionStream(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		return text, nil
	}
}

func (c *YandexClient) readRecognitionStream(r io.Reader) (string, error) {
	var sb strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		text := extractChunkText(line)
		if text != "" {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("yandex: read recognition stream: %w", err)
	}
	return sb.String(), nil
}

// extractChunkText pulls recognized text out of a single NDJSON chunk.
// Prefers finalRefinement.normalizedText (post-processed) over raw final.
func extractChunkText(data []byte) string {
	var chunk yandexStreamingChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return ""
	}
	// Wrapped form: {"result": {...}}
	if chunk.Result != nil {
		if txt := altText(chunk.Result.FinalRefinement, true); txt != "" {
			return txt
		}
		return altText(chunk.Result.Final, false)
	}
	// Flat form
	if txt := altText(chunk.FinalRefinement, true); txt != "" {
		return txt
	}
	return altText(chunk.Final, false)
}

// altText concatenates alternatives from an AlternativeUpdate. When refined
// is true we expect normalizedText to be set on FinalRefinement.
func altText(src interface{}, refined bool) string {
	var upd *yandexAlternativeUpdate
	switch v := src.(type) {
	case *yandexFinalRefinement:
		if v == nil || v.NormalizedText == nil {
			return ""
		}
		upd = v.NormalizedText
	case *yandexAlternativeUpdate:
		if v == nil {
			return ""
		}
		upd = v
	default:
		return ""
	}
	if upd == nil || len(upd.Alternatives) == 0 {
		return ""
	}
	// Use only the first (best) alternative; channel tag irrelevant here.
	return strings.TrimSpace(upd.Alternatives[0].Text)
}

func (c *YandexClient) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Api-Key "+c.config.APIKey)
	if c.config.FolderID != "" {
		req.Header.Set("x-folder-id", c.config.FolderID)
	}
}
