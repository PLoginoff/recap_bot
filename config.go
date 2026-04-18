package main

import (
	"fmt"
	"os"
	"time"

	"github.com/creasty/defaults"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Recognizer     string               `yaml:"recognizer" default:"sber"`
	Sber           ConfigSber           `yaml:"sber"`
	Openrouter     ConfigOpenrouter     `yaml:"openrouter"`
	Prompts        ConfigPrompts        `yaml:"prompts"`
	Messages       ConfigMessages       `yaml:"messages"`
	RateLimit      ConfigRateLimit      `yaml:"rate_limit"`
	NumWorkers     int                  `yaml:"num_workers" default:"2"`
	WaitOnError    time.Duration        `yaml:"wait_on_error" default:"3s"`
	FFmpegPath     string               `yaml:"ffmpeg_path" default:"ffmpeg"`
	SaveDebugMedia bool                 `yaml:"save_debug_media" default:"false"`
	Debug          bool                 `yaml:"debug" default:"false"`
	StateFile      string               `yaml:"state_file" default:"state.txt"`
	Bots           map[string]ConfigBot `yaml:"bots"`
}

type ConfigBot struct {
	ID        string        `yaml:"id"`
	Messenger MessengerType `yaml:"messenger"`
	Token     string        `yaml:"token"`
	Prompt    string        `yaml:"prompt"`
}

type ConfigSber struct {
	Tokens []struct {
		Name         string        `yaml:"name"`
		ClientID     string        `yaml:"client_id"`
		ClientSecret string        `yaml:"client_secret"`
		Cooldown     time.Duration `yaml:"cooldown"`
		Limit        time.Duration `yaml:"limit"`
	} `yaml:"tokens"`
}

type ConfigOpenrouter struct {
	APIKey string `yaml:"api_key"`
	Models []struct {
		Name     string        `yaml:"name"`
		Cooldown time.Duration `yaml:"cooldown"`
	} `yaml:"models"`
}

type ConfigPrompts struct {
	SystemPrompt string `yaml:"system"`
	UserPrompt   string `yaml:"user"`
}

type ConfigMessages struct {
	Listening        string `yaml:"listening" default:"Listening"`
	StartMessage     string `yaml:"start" default:"Send me a voice message and I'll transcribe it for you!"`
	ErrorMessage     string `yaml:"error" default:"An error occurred while processing your message. Please try again."`
	FailureMessage   string `yaml:"failure" default:"Failed to transcribe the audio. Please check the audio quality and try again."`
	RetryMessage     string `yaml:"retry" default:"Retrying..."`
	RateLimitMessage string `yaml:"rate_limit" default:"Rate limit exceeded. Please try again later."`
}

type ConfigRateLimit struct {
	MaxRequests int           `yaml:"max_requests" default:"10"`
	TimeWindow  time.Duration `yaml:"time_window" default:"1h"`
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	if err := defaults.Set(&config); err != nil {
		return nil, err
	}

	// Validate that at least one bot is configured
	if len(config.Bots) == 0 {
		return nil, fmt.Errorf("at least one bot must be configured")
	}

	// Fill ID from map keys if not set
	for botID, botConfig := range config.Bots {
		if botConfig.ID == "" {
			botConfig.ID = botID
			config.Bots[botID] = botConfig
		}
	}

	return &config, nil
}
