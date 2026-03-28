package main

import (
	"os"
	"time"

	"github.com/creasty/defaults"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Recognizer    string           `yaml:"recognizer" default:"sber"`
	Telegram      struct {
		Token string `yaml:"token"`
	} `yaml:"telegram"`
	Max           struct {
		Token string `yaml:"token"`
	} `yaml:"max"`
	Sber struct {
		Tokens []struct {
			Name         string        `yaml:"name"`
			ClientID     string        `yaml:"client_id"`
			ClientSecret string        `yaml:"client_secret"`
			Cooldown     time.Duration `yaml:"cooldown"`
			Limit        time.Duration `yaml:"limit"`
		} `yaml:"tokens"`
	} `yaml:"sber"`
	Openrouter    ConfigOpenrouter  `yaml:"openrouter"`
	Prompts       ConfigPrompts     `yaml:"prompts"`
	Messages      ConfigMessages    `yaml:"messages"`
	RateLimit     ConfigRateLimit   `yaml:"rate_limit"`
	NumWorkers    int               `yaml:"num_workers" default:"2"`
	WaitOnError   time.Duration     `yaml:"wait_on_error" default:"3s"`
	FFmpegPath    string            `yaml:"ffmpeg_path"`
	SaveDebugMedia bool             `yaml:"save_debug_media" default:"false"`
	StateFile     string            `yaml:"state_file"`
}

type ConfigOpenrouter struct {
	APIKey string `yaml:"api_key"`
	Models []struct {
		Name     string        `yaml:"name"`
		Cooldown time.Duration `yaml:"cooldown"`
		Limit    time.Duration `yaml:"limit"`
	} `yaml:"models"`
}

type ConfigPrompts struct {
	Prompts struct {
		SystemPrompt string `yaml:"system"`
		UserPrompt   string `yaml:"user"`
	} `yaml:"prompts"`
}

type ConfigMessages struct {
	Listening      string `yaml:"listening" default:"Listening"`
	StartMessage   string `yaml:"start" default:"Send me a voice message and I'll transcribe it for you!"`
	ErrorMessage   string `yaml:"error" default:"An error occurred while processing your message. Please try again."`
	FailureMessage string `yaml:"failure" default:"Failed to transcribe the audio. Please check the audio quality and try again."`
	RetryMessage   string `yaml:"retry" default:"Retrying..."`
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

	return &config, nil
}
