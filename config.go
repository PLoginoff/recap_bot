package main

import (
	"os"
	"time"

	"github.com/creasty/defaults"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Recognizer  string `yaml:"recognizer" default:"sber"`
	Telegram  struct {
		Token string `yaml:"token"`
	} `yaml:"telegram"`
	Sber struct {
		Tokens []struct {
			Name         string        `yaml:"name"`
			ClientID     string        `yaml:"client_id"`
			ClientSecret string        `yaml:"client_secret"`
			Cooldown     time.Duration `yaml:"cooldown"`
			Limit        time.Duration `yaml:"limit"`
		} `yaml:"tokens"`
	} `yaml:"sber"`
	Openrouter struct {
		APIKey string `yaml:"api_key"`
		Models []struct {
			Name     string        `yaml:"name"`
			Cooldown time.Duration `yaml:"cooldown"`
			Limit    time.Duration `yaml:"limit"`
		} `yaml:"models"`
		Model        string `yaml:"model"`
		ModelReserve string `yaml:"model_reserve"`
		SystemPrompt string `yaml:"system_prompt"`
		UserPrompt   string `yaml:"user_prompt"`
	} `yaml:"openrouter"`
	Messages    ConfigMessages `yaml:"messages"`
	NumWorkers  int            `yaml:"num_workers" default:"3"`
	WaitOnError time.Duration  `yaml:"wait_on_error" default:"3s"`
	FFmpegPath     string `yaml:"ffmpeg_path" default:"ffmpeg"`
	SaveDebugMedia bool   `yaml:"save_debug_media"`
	StateFile string `yaml:"state_file" default:"recap.state"`
}

type BotConfig struct {
	TelegramToken string
	Messages     ConfigMessages
}

type ConfigMessages struct {
	Listening      string `yaml:"listening" default:"Listening"`
	StartMessage   string `yaml:"start_message" default:"Send me a voice message and I'll transcribe it for you!"`
	ErrorMessage   string `yaml:"error_message" default:"An error occurred while processing your message. Please try again."`
	FailureMessage string `yaml:"failure_message" default:"Failed to transcribe the audio. Please check the audio quality and try again."`
	RetryMessage   string `yaml:"retry_message" default:"Retrying..."`
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
