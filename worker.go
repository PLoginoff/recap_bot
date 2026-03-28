package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"strings"
	"sync"
	"time"
)

func worker(ctx context.Context, wg *sync.WaitGroup, id int, tasks chan *RecapTask, b *Bot, waitOnError time.Duration, retryMessage string, loggers *Loggers) {
	defer wg.Done()
	log.Printf("Worker %d started", id)
	const maxErrors = 5

	for {
		select {
		case task := <-tasks:
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
					_, task.AudioData, err = b.DownloadFileForTask(ctx, task)
					if err != nil {
						loggers.Error.Printf("Worker %d: Failed to download file: %v", id, err)
						break
					}
					loggers.Status.Printf("Worker %d: Downloaded file bytes=%d is_video_note=%t", id, len(task.AudioData), task.IsVideoNote)
				}

				// Convert video note to audio if needed
				if task.IsVideoNote {
					loggers.Status.Printf("Worker %d: Converting video note, input bytes=%d", id, len(task.AudioData))
					task.AudioData, err = b.convertVideoNote(ctx, task.AudioData)
					if err != nil {
						loggers.Error.Printf("Worker %d: Failed to convert video note: %v", id, err)
						break
					}
					loggers.Status.Printf("Worker %d: Converted video note, output bytes=%d", id, len(task.AudioData))
				}

				task.Status = StatusSTT
				b.addDotToStatus(ctx, task)

			case StatusSTT:
				// Speech to text
				if b.saveDebugMedia {
					b.saveDebugAudio(ctx, task)
				}

				task.Text, err = b.Recognize(ctx, task.AudioData)
				if err != nil {
					loggers.Error.Printf("Worker %d: Failed to recognize audio: %v", id, err)
					break
				}
				task.Status = StatusRecap
				b.addDotToStatus(ctx, task)

			case StatusRecap:
				task.Summary, err = b.Summarize(ctx, task.Text)
				if err != nil {
					loggers.Error.Printf("Worker %d: Failed to summarize text: %v", id, err)
					break
				}
				task.Status = StatusSent
				b.addDotToStatus(ctx, task)

			case StatusSent:
				// Format summary - use expandable blockquote for long text
				paragraphs := strings.Split(task.Summary, "\n\n")
				safeSummary := html.EscapeString(task.Summary)
				formattedSummary := safeSummary
				if len(paragraphs) > 1 {
					formattedSummary = fmt.Sprintf("<blockquote expandable>%s</blockquote>", safeSummary)
				}
				
				if err := b.UpdateMessageForTask(ctx, task, formattedSummary); err != nil {
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
					tasks <- task
					continue
				}

				var tempErr sberTemporaryError
				if errors.As(err, &tempErr) {
					loggers.Status.Printf("Worker %d: Temporary Sber error for message %s: %v", id, task.MessageID, err)
					applyRetryBackoff(ctx, b, task, waitOnError, retryMessage, id)
					tasks <- task
					continue
				}

				loggers.Error.Printf("Worker %d: Error processing task for message %s, status %s: %v", id, task.MessageID, task.Status, err)
				task.ErrorCount++
				if task.ErrorCount >= maxErrors {
					loggers.Error.Printf("Worker %d: Reached max retries for message %s", id, task.MessageID)
					b.notifyFailure(ctx, task)
					continue
				}

				// Increase wait time on each new error
				applyRetryBackoff(ctx, b, task, waitOnError, retryMessage, id)

				// Return task to queue
				tasks <- task
			} else {
				// Return task to queue for next stage (except for done tasks)
				if task.Status != StatusDone {
					tasks <- task
				}
			}

			loggers.Status.Printf("Worker %d: Finished task for message %s, status %s, duration %v", id, task.MessageID, task.Status, time.Since(startTime))

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

func applyRetryBackoff(ctx context.Context, b *Bot, task *RecapTask, waitOnError time.Duration, retryMessage string, workerID int) {
	task.ErrorCount++
	task.Wait = waitOnError * time.Duration(task.ErrorCount)
	if task.StatusMessageID != "" {
		thinking := retryThinkingMessage(task.ErrorCount, retryMessage)
		if updateErr := b.UpdateMessageForTask(ctx, task, thinking); updateErr != nil {
			log.Printf("Worker %d: Failed to refresh thinking message: %v", workerID, updateErr)
		}
	}
}
