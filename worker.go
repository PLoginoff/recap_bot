package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"strings"
	"sync"
	"time"
)

func worker(ctx context.Context, wg *sync.WaitGroup, id int, taskQueue chan *Task, hub *Hub, waitOnError time.Duration, retryMessage string) {
	defer wg.Done()
	log.Printf("Worker %d started", id)
	const maxErrors = 5

	for {
		select {
		case task := <-taskQueue:
			if task == nil {
				continue
			}
			if task.Wait > 0 {
				select {
				case <-time.After(task.Wait):
				case <-ctx.Done():
					log.Printf("Worker %d stopping", id)
					return
				}
				task.Wait = 0
			}

			startTime := time.Now()
			slog.Info("Processing task", "worker", id, "message", task.MessageID, "status", task.Status)

			var err error
			switch task.Status {
			case StatusDownload:
				// Download file and convert if needed (one stage)
				if task.AudioData == nil {
					_, task.AudioData, err = hub.DownloadFileForTask(ctx, task)
					if err != nil {
						slog.Error("Failed to download file", "worker", id, "error", err)
						break
					}
					slog.Debug("Downloaded file", "worker", id, "bytes", len(task.AudioData), "is_video_note", task.IsVideoNote)
				}

				// Convert video note to audio if needed
				if task.IsVideoNote {
					slog.Debug("Converting video note", "worker", id, "input_bytes", len(task.AudioData))
					task.AudioData, err = convertVideoNote(ctx, hub.ffmpegPath, task.AudioData)
					if err != nil {
						slog.Error("Failed to convert video note", "worker", id, "error", err)
						break
					}
					slog.Debug("Converted video note", "worker", id, "output_bytes", len(task.AudioData))
				}

				// Convert MP3 to OGG for Sber if needed
				if task.IsMP3 && !task.IsVideoNote {
					slog.Debug("Converting MP3 to OGG", "worker", id, "input_bytes", len(task.AudioData))
					task.AudioData, err = convertMP3ToOGG(ctx, hub.ffmpegPath, task.AudioData)
					if err != nil {
						slog.Error("Failed to convert MP3 to OGG", "worker", id, "error", err)
						break
					}
					slog.Debug("Converted MP3 to OGG", "worker", id, "output_bytes", len(task.AudioData))
				}

				task.Status = StatusSTT
				hub.addDotToStatus(ctx, task)

			case StatusSTT:
				// Speech to text
				if hub.saveDebugMedia {
					saveDebugAudio(task.MessageID, task.AudioData, task.Messenger)
				}
				task.Text, err = hub.Recognize(ctx, task.AudioData)
				if err != nil {
					slog.Error("Failed to recognize audio", "worker", id, "error", err)
					break
				}
				task.Status = StatusRecap
				hub.addDotToStatus(ctx, task)

			case StatusRecap:
				task.Summary, err = hub.Summarize(ctx, task.Text, task.BotID)
				if err != nil {
					slog.Error("Failed to summarize text", "worker", id, "error", err)
					break
				}
				task.Status = StatusSent
				hub.addDotToStatus(ctx, task)

			case StatusSent:
				// Update status message with result - formatting handled by messenger
				if task.InlineQueryID != "" {
					bot, err := hub.getBot(task.BotID)
					if err != nil {
						slog.Error("Failed to get bot", "worker", id, "error", err)
						break
					}
					if err := bot.AnswerInlineQuery(ctx, task.InlineQueryID, task.Summary); err != nil {
						slog.Error("Failed to answer inline query", "worker", id, "error", err)
						break
					}
					task.Status = StatusDone
					break
				}
				if task.StatusMessageID == "" {
					bot, err := hub.getBot(task.BotID)
					if err != nil {
						slog.Error("Failed to get bot", "worker", id, "error", err)
						break
					}
					if _, err := bot.Messenger().SendMessage(ctx, task.ChatID, task.MessageID, task.Summary); err != nil {
						slog.Error("Failed to send message", "worker", id, "error", err)
						break
					}
					task.Status = StatusDone
					break
				}
				if err := hub.UpdateMessageForTask(ctx, task, task.Summary, true); err != nil {
					slog.Error("Failed to update message", "worker", id, "error", err)
					bot, getErr := hub.getBot(task.BotID)
					if getErr != nil {
						slog.Error("Failed to get bot", "worker", id, "error", getErr)
						break
					}
					if _, sendErr := bot.Messenger().SendMessage(ctx, task.ChatID, task.MessageID, task.Summary); sendErr != nil {
						slog.Error("Failed to send message after update error", "worker", id, "error", sendErr)
						break
					}
				}
				task.Status = StatusDone
			}

			if err != nil {
				var cooldownErr sberCooldownError
				if errors.As(err, &cooldownErr) {
					wait := time.Until(cooldownErr.ResumeAt)
					if wait < 0 {
						wait = 0
					}
					slog.Info("Sber cooldown", "worker", id, "until", cooldownErr.ResumeAt, "wait", wait)
					task.Wait = wait
					// Return task to queue without burning retry attempts
					select {
					case taskQueue <- task:
					default:
						slog.Error("Task queue full, dropping cooldown task", "worker", id, "message", task.MessageID)
					}
					continue
				}

				var tempErr sberTemporaryError
				if errors.As(err, &tempErr) {
					slog.Debug("Temporary Sber error", "worker", id, "message", task.MessageID, "error", err)
					task.ErrorCount++
					applyRetryBackoff(ctx, hub, task, waitOnError, retryMessage, id)
					select {
					case taskQueue <- task:
					default:
						slog.Error("Task queue full, dropping retry task", "worker", id, "message", task.MessageID)
					}
					continue
				}

				slog.Error("Error processing task", "worker", id, "message", task.MessageID, "status", task.Status, "error", err)
				task.ErrorCount++
				if task.ErrorCount >= maxErrors {
					slog.Error("Reached max retries", "worker", id, "message", task.MessageID)
					hub.notifyFailure(ctx, task)
					continue
				}

				// Increase wait time on each new error
				applyRetryBackoff(ctx, hub, task, waitOnError, retryMessage, id)

				// Return task to queue
				select {
				case taskQueue <- task:
				default:
					slog.Error("Task queue full, dropping error retry task", "worker", id, "message", task.MessageID)
				}
			} else {
				// Return task to queue for next stage (except for done tasks)
				if task.Status != StatusDone {
					select {
					case taskQueue <- task:
					default:
						slog.Error("Task queue full, dropping next stage task", "worker", id, "message", task.MessageID)
					}
				}
			}

			slog.Debug("Completed task", "worker", id, "message", task.MessageID, "duration", time.Since(startTime))

		case <-ctx.Done():
			slog.Info("Worker stopping", "worker", id)
			return
		}
	}
}

func retryThinkingMessage(attempt int, retryMessage string) string {
	if attempt < 1 {
		attempt = 1
	}
	return retryMessage + strings.Repeat(" 💪", attempt)
}

func applyRetryBackoff(ctx context.Context, hub *Hub, task *Task, waitOnError time.Duration, retryMessage string, workerID int) {
	if task.StatusMessageID != "" {
		thinking := retryThinkingMessage(task.ErrorCount, retryMessage)
		if updateErr := hub.UpdateMessageForTask(ctx, task, thinking, false); updateErr != nil {
			log.Printf("Worker %d: Failed to refresh thinking message: %v", workerID, updateErr)
		}
	}

	task.Wait = waitOnError * time.Duration(task.ErrorCount)
}
