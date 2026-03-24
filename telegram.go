package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-telegram/bot/models"
)

func (b *Bot) SendMessage(ctx context.Context, chatID int, replyTo int, text string) (int, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	data := url.Values{}
	data.Set("chat_id", strconv.Itoa(chatID))
	if replyTo != 0 {
		data.Set("reply_to_message_id", strconv.Itoa(replyTo))
	}
	data.Set("text", text)

	resp, err := http.PostForm(apiURL, data)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var apiResponse struct {
		OK          bool `json:"ok"`
		Result      struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return 0, err
	}
	if !apiResponse.OK {
		return 0, fmt.Errorf("telegram API error: %s", string(body))
	}

	return apiResponse.Result.MessageID, nil
}

func (b *Bot) sendErrorMessage(ctx context.Context, chatID int, messageID int) {
	if _, err := b.SendMessage(ctx, chatID, messageID, b.messages.ErrorMessage); err != nil {
		log.Printf("Failed to send error message: %v", err)
	}
}

func (b *Bot) addDotToStatus(ctx context.Context, task *RecapTask) {
	if task.StatusMessageID != 0 {
		// Add a dot to current status
		task.StatusText += "."
		if err := b.UpdateMessage(ctx, task.ChatID, task.StatusMessageID, task.StatusText); err != nil {
			log.Printf("Failed to update listening status: %v", err)
		}
	}
}

func (b *Bot) UpdateMessage(ctx context.Context, chatID int, messageID int, text string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", b.token)
	data := url.Values{}
	data.Set("chat_id", strconv.Itoa(chatID))
	data.Set("message_id", strconv.Itoa(messageID))
	data.Set("text", text)
	data.Set("parse_mode", "HTML")

	resp, err := http.PostForm(apiURL, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var apiResponse struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return err
	}
	if !apiResponse.OK {
		return fmt.Errorf("telegram API error: %s", string(body))
	}

	return nil
}

func (b *Bot) GetFile(ctx context.Context, fileID string) (*models.File, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile", b.token)
	data := url.Values{}
	data.Set("file_id", fileID)

	resp, err := http.PostForm(apiURL, data)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResponse struct {
		OK     bool        `json:"ok"`
		Result *models.File `json:"result"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, err
	}
	if !apiResponse.OK {
		return nil, fmt.Errorf("telegram API error: %s", string(body))
	}

	return apiResponse.Result, nil
}

func (b *Bot) DownloadFile(ctx context.Context, filePath string) ([]byte, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.token, filePath)
	
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.downloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download error (%s): %s", resp.Status, string(body))
	}
	if len(body) == 0 {
		return nil, errors.New("telegram download returned empty file")
	}
	return body, nil
}
