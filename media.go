package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (b *Bot) convertVideoNote(ctx context.Context, videoData []byte) ([]byte, error) {
	inputFile, err := os.CreateTemp("", "tg-video-*.mp4")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp input file: %w", err)
	}
	defer os.Remove(inputFile.Name())
	defer inputFile.Close()

	if _, err := inputFile.Write(videoData); err != nil {
		return nil, fmt.Errorf("failed to write to temp input file: %w", err)
	}
	if err := inputFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp input file: %w", err)
	}

	outputFile, err := os.CreateTemp("", "tg-audio-*.ogg")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp output file: %w", err)
	}
	defer os.Remove(outputFile.Name())
	outputPath := outputFile.Name()
	if err := outputFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp output file: %w", err)
	}

	cmd := exec.CommandContext(ctx, b.ffmpegPath,
		"-y",
		"-fflags", "+genpts",
		"-avoid_negative_ts", "make_zero",
		"-i", inputFile.Name(),
		"-map", "0:a:0",
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "libopus",
		"-b:a", "32k",
		"-vbr", "off",
		"-application", "audio",
		"-frame_duration", "20",
		"-sample_fmt", "s16",
		"-af", "aresample=async=1:first_pts=0",
		"-map_metadata", "-1",
		"-f", "ogg",
		outputFile.Name(),
	)

	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))
	if err != nil {
		return nil, fmt.Errorf("ffmpeg conversion failed: %w: %s", err, outputText)
	}
	if outputText != "" {
		log.Printf("ffmpeg output: %s", outputText)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat converted audio: %w", err)
	}
	if info.Size() == 0 {
		if outputText == "" {
			outputText = "no ffmpeg output"
		}
		return nil, fmt.Errorf("ffmpeg produced empty audio output: %s", outputText)
	}

	audioData, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read converted audio: %w", err)
	}

	return audioData, nil
}

func (b *Bot) saveDebugAudio(ctx context.Context, task *RecapTask) {
	if !b.saveDebugMedia || task.DebugAudioSaved {
		return
	}

	timestamp := time.Now().Format("20060102-150405")
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%s", task.ChatID, task.MessageID, timestamp)))
	hashStr := hex.EncodeToString(hash[:])[:8]

	debugDir := filepath.Join("debug", task.DebugDir)
	if err := os.MkdirAll(debugDir, 0755); err != nil {
		log.Printf("Failed to create debug directory: %v", err)
		return
	}

	filename := fmt.Sprintf("%s-%s-%s-%s.%s", hashStr, task.ChatID, task.MessageID, timestamp, task.DebugExt)
	filePath := filepath.Join(debugDir, filename)

	if err := os.WriteFile(filePath, task.AudioData, 0644); err != nil {
		log.Printf("Failed to save debug audio: %v", err)
		return
	}

	log.Printf("Saved debug audio to: %s", filePath)
	task.DebugAudioSaved = true
}
