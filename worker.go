package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

func worker(ctx context.Context, wg *sync.WaitGroup, id int, taskQueue chan *Task, hub *Hub, waitOnError time.Duration, retryMessage string, loggers *Loggers) {
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
				time.Sleep(task.Wait)
				task.Wait = 0
			}

			startTime := time.Now()
			loggers.Status.Printf("Worker %d: Processing task for message %s, status %s", id, task.MessageID, task.Status)

			var err error
			switch task.Status {
			case StatusDownload:
				// Download file and convert if needed (one stage)
				if task.AudioData == nil {
					_, task.AudioData, err = hub.DownloadFileForTask(ctx, task)
					if err != nil {
						loggers.Error.Printf("Worker %d: Failed to download file: %v", id, err)
						break
					}
					loggers.Status.Printf("Worker %d: Downloaded file bytes=%d is_video_note=%t", id, len(task.AudioData), task.IsVideoNote)
				}

				// Convert video note to audio if needed
				if task.IsVideoNote {
					loggers.Status.Printf("Worker %d: Converting video note, input bytes=%d", id, len(task.AudioData))
					task.AudioData, err = convertVideoNote(ctx, hub.ffmpegPath, task.AudioData)
					if err != nil {
						loggers.Error.Printf("Worker %d: Failed to convert video note: %v", id, err)
						break
					}
					loggers.Status.Printf("Worker %d: Converted video note, output bytes=%d", id, len(task.AudioData))
				}

				// Convert MP3 to OGG for Sber if needed
				if task.IsMP3 && !task.IsVideoNote {
					loggers.Status.Printf("Worker %d: Converting MP3 to OGG, input bytes=%d", id, len(task.AudioData))
					task.AudioData, err = convertMP3ToOGG(ctx, hub.ffmpegPath, task.AudioData)
					if err != nil {
						loggers.Error.Printf("Worker %d: Failed to convert MP3 to OGG: %v", id, err)
						break
					}
					loggers.Status.Printf("Worker %d: Converted MP3 to OGG, output bytes=%d", id, len(task.AudioData))
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
					loggers.Error.Printf("Worker %d: Failed to recognize audio: %v", id, err)
					break
				}
				task.Status = StatusRecap
				hub.addDotToStatus(ctx, task)

			case StatusRecap:
				task.Summary, err = hub.Summarize(ctx, task.Text, task.BotID)
				if err != nil {
					loggers.Error.Printf("Worker %d: Failed to summarize text: %v", id, err)
					break
				}
				task.Status = StatusSent
				hub.addDotToStatus(ctx, task)

			case StatusSent:
				// Update status message with result - formatting handled by messenger
				if err := hub.UpdateMessageForTask(ctx, task, task.Summary, true); err != nil {
					loggers.Error.Printf("Worker %d: Failed to update message: %v", id, err)
					break
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
					loggers.Status.Printf("Worker %d: Sber cooldown until %s, waiting %v", id, cooldownErr.ResumeAt.Format(time.RFC3339), wait)
					task.Wait = wait
					// Return task to queue without burning retry attempts
					taskQueue <- task
					continue
				}

				var tempErr sberTemporaryError
				if errors.As(err, &tempErr) {
					loggers.Status.Printf("Worker %d: Temporary Sber error for message %s: %v", id, task.MessageID, err)
					applyRetryBackoff(ctx, hub, task, waitOnError, retryMessage, id)
					taskQueue <- task
					continue
				}

				loggers.Error.Printf("Worker %d: Error processing task for message %s, status %s: %v", id, task.MessageID, task.Status, err)
				task.ErrorCount++
				if task.ErrorCount >= maxErrors {
					loggers.Error.Printf("Worker %d: Reached max retries for message %s", id, task.MessageID)
					hub.notifyFailure(ctx, task)
					continue
				}

				// Increase wait time on each new error
				applyRetryBackoff(ctx, hub, task, waitOnError, retryMessage, id)

				// Return task to queue
				taskQueue <- task
			} else {
				// Return task to queue for next stage (except for done tasks)
				if task.Status != StatusDone {
					taskQueue <- task
				}
			}

			loggers.Status.Printf("Worker %d: Completed task for message %s in %v", id, task.MessageID, time.Since(startTime))

		case <-ctx.Done():
			log.Printf("Worker %d stopping", id)
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
