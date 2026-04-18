package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// convertToOGG converts audio/video data to OGG Opus format using ffmpeg.
func convertToOGG(ctx context.Context, ffmpegPath string, audioData []byte) ([]byte, error) {
	inputFile, err := os.CreateTemp("", "recap-input-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp input file: %w", err)
	}
	inputPath := inputFile.Name()
	defer os.Remove(inputPath)

	if _, err := inputFile.Write(audioData); err != nil {
		inputFile.Close()
		return nil, fmt.Errorf("failed to write to temp input file: %w", err)
	}
	inputFile.Close()

	outputFile, err := os.CreateTemp("", "recap-output-*.ogg")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp output file: %w", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close()
	defer os.Remove(outputPath)

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-y",
		"-fflags", "+genpts",
		"-avoid_negative_ts", "make_zero",
		"-i", inputPath,
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
		outputPath,
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
		return nil, fmt.Errorf("ffmpeg produced empty output: %s", outputText)
	}

	return os.ReadFile(outputPath)
}

func convertMP3ToOGG(ctx context.Context, ffmpegPath string, audioData []byte) ([]byte, error) {
	return convertToOGG(ctx, ffmpegPath, audioData)
}

func convertVideoNote(ctx context.Context, ffmpegPath string, audioData []byte) ([]byte, error) {
	return convertToOGG(ctx, ffmpegPath, audioData)
}

func saveDebugAudio(taskID string, audioData []byte, messengerType MessengerType) {
	debugDir := filepath.Join("debug", string(messengerType))
	if err := os.MkdirAll(debugDir, 0755); err != nil {
		log.Printf("Failed to create debug directory: %v", err)
		return
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("audio_%s_%s.ogg", taskID, timestamp)
	filepath := filepath.Join(debugDir, filename)

	if err := os.WriteFile(filepath, audioData, 0644); err != nil {
		log.Printf("Failed to save debug audio: %v", err)
		return
	}

	log.Printf("Debug audio saved: %s", filepath)
}
