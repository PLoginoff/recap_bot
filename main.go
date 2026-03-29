package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	// Setup loggers
	loggers, err := setupLoggers()
	if err != nil {
		log.Fatalf("Failed to setup loggers: %v", err)
	}

	// Load configuration
	config, err := loadConfig("recap.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if config.NumWorkers == 0 {
		config.NumWorkers = 2
	}
	if config.WaitOnError == 0 {
		config.WaitOnError = 3 * time.Second
	}

	// Create task channel
	taskQueue := make(chan *Task, 100)

	// Create shared rate limiter from config
	rateLimiter := NewDefaultRateLimiter(config.RateLimit.MaxRequests, config.RateLimit.TimeWindow)

	// Create messenger clients from config
	bots := make(map[string]MessengerClient)

	for botID, botConfig := range config.Bots {
		switch botConfig.Messenger {
		case MessengerTelegram:
			telegramMessenger := NewTelegramMessenger(botID, botConfig, taskQueue, rateLimiter, config.Messages)
			bots[botID] = telegramMessenger
			log.Printf("Configured Telegram bot: %s", botConfig.ID)

		case MessengerMax:
			maxMessenger := NewMaxMessenger(botID, botConfig, taskQueue, rateLimiter, config.Messages)
			bots[botID] = maxMessenger
			log.Printf("Configured Max bot: %s", botConfig.ID)

		default:
			log.Printf("Unknown messenger type: %s", botConfig.Messenger)
		}
	}

	// Create hub
	log.Printf("Configured %d bots", len(config.Bots))

	statePath := config.StateFile
	if statePath == "" {
		statePath = "state.txt"
	}

	stateStore, err := NewStateStore(statePath)
	if err != nil {
		log.Fatalf("Failed to create state store: %v", err)
	}

	var recognizer SpeechRecognizer
	recognizerType := config.Recognizer
	if recognizerType == "" {
		recognizerType = "sber"
	}
	log.Printf("Using recognizer: %s", recognizerType)

	switch recognizerType {
	case "sber":
		if len(config.Sber.Tokens) == 0 {
			log.Fatalf("No Sber tokens provided in config")
		}
		tokens := make([]SberTokenConfig, 0, len(config.Sber.Tokens))
		for _, token := range config.Sber.Tokens {
			tokens = append(tokens, SberTokenConfig{
				Name:         token.Name,
				ClientID:     token.ClientID,
				ClientSecret: token.ClientSecret,
				Cooldown:     token.Cooldown,
				Limit:        token.Limit,
			})
		}
		sberConfig := SberConfig{
			Tokens: tokens,
		}
		recognizer = NewSberClient(sberConfig, stateStore)
	default:
		log.Fatalf("Unknown recognizer type: %s", config.Recognizer)
	}

	openrouterModels := make([]OpenRouterModel, 0, len(config.Openrouter.Models))
	for _, model := range config.Openrouter.Models {
		openrouterModels = append(openrouterModels, OpenRouterModel{
			Name:     model.Name,
			Cooldown: model.Cooldown,
		})
	}

	openrouterConfig := OpenRouterConfig{
		APIKey:       config.Openrouter.APIKey,
		Models:       openrouterModels,
		SystemPrompt: config.Prompts.SystemPrompt,
		UserPrompt:   config.Prompts.UserPrompt,
	}

	openrouterClient := NewOpenRouterClient(openrouterConfig, stateStore)

	// Create hub
	hub, err := NewHub(bots, config.Bots, recognizer, openrouterClient, taskQueue, config.FFmpegPath, config.SaveDebugMedia, config.Messages)
	if err != nil {
		log.Fatalf("Failed to create hub: %v", err)
	}

	// Create context for graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < config.NumWorkers; i++ {
		wg.Add(1)
		go worker(ctx, &wg, i, taskQueue, hub, config.WaitOnError, config.Messages.RetryMessage, loggers)
	}

	// Start hub
	log.Println("Starting hub...")
	hub.Start(ctx)

	// Wait for all workers to finish
	wg.Wait()
	log.Println("All workers stopped.")
}
