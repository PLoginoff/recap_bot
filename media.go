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

func convertMP3ToOGG(ctx context.Context, ffmpegPath string, audioData []byte) ([]byte, error) {
	inputFile, err := os.CreateTemp("", "max-mp3-*.mp3")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp input file: %w", err)
	}
	defer os.Remove(inputFile.Name())
	defer inputFile.Close()

	if _, err := inputFile.Write(audioData); err != nil {
		return nil, fmt.Errorf("failed to write to temp input file: %w", err)
	}
	if err := inputFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp input file: %w", err)
	}

	outputFile, err := os.CreateTemp("", "max-ogg-*.ogg")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp output file: %w", err)
	}
	defer os.Remove(outputFile.Name())
	outputPath := outputFile.Name()
	if err := outputFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp output file: %w", err)
	}

	cmd := exec.CommandContext(ctx, ffmpegPath,
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
		return nil, fmt.Errorf("ffmpeg MP3 to OGG conversion failed: %w: %s", err, outputText)
	}
	if outputText != "" {
		log.Printf("ffmpeg MP3 to OGG output: %s", outputText)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat converted OGG: %w", err)
	}
	if info.Size() == 0 {
		if outputText == "" {
			outputText = "no ffmpeg output"
		}
		return nil, fmt.Errorf("ffmpeg produced empty OGG output: %s", outputText)
	}

	oggData, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read converted OGG: %w", err)
	}

	return oggData, nil
}

func convertVideoNote(ctx context.Context, ffmpegPath string, audioData []byte) ([]byte, error) {
	inputFile, err := os.CreateTemp("", "tg-video-*.mp4")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp input file: %w", err)
	}
	defer os.Remove(inputFile.Name())
	defer inputFile.Close()

	if _, err := inputFile.Write(audioData); err != nil {
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

	cmd := exec.CommandContext(ctx, ffmpegPath,
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

	audioData, err = os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read converted audio: %w", err)
	}

	return audioData, nil
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
