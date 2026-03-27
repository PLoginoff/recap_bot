package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v2"
)

// Test phrases
var testPhrases = []string{
	"План на день: 1. Проснуться в 7 утра 2. Завтрак 3. Работа 4. Обед 5. Прогулка 6. Ужин 7. Отдых 8. Сон",
	"Ну вот, паш, хочется надеяться, что если вкатываешься в какой нибудь приличный проект, то там тоже будет все хорошо. Вот не придется говнокот изобретать, велосипед, копипаст эти дурацкие, это все прошлый век. Конечно. Мне кажется, люди, перекатившийся в головы, должны быть грамотные ребята. Знающие другие высокоуровне языки, вот java, перекатывающиеся из, ну где ооп нормальный, вот поэтому они должны тоже бороться за качественный код уже в этом гошке, где по определению сложно что то качественно сделать, да, вот. (резервный вариант распознования: ну вот паш хочется надеяться что если вкатываешься в какой нибудь приличный проект то там тоже будет все хорошо вот не придется говнокот изобретать велосипед копипаст эти дурацкие это все прошлый век конечно мне кажется люди перекатившийся в голову должны быть грамотные ребята знающие другие высокоуровне языки вот java перекатывающиеся из ну где ооп нормальный вот поэтому они должны тоже бороться за качественный код уже в этом гошке где по определению сложное что то качественно сделать да вот)",
	"Пацаны! Всем привет! Всем хорошего дня!",
	"Че то какая-то фигня, паш, у тебя получилось. Ну прости, без Обид, но исходного текста в сообщении, которое GigaChat перевёл, было меньше, чем в рекапе, который ты сделал.",
	"Вон видишь, написано has no access to messages. Вот поэтому и не работает. Я не нашёл другого способа. Надо повысить до админа можешь полезать все права по максимуму и все. Вот. То есть уже видят, что как бы для больших компаний дают права, для своих Ботов уже нет.",
	" 💬 Паш, ты не понесли зачем это нужно? Я подумал что есть же амнезия, её вполне достаточно полностью она хорошо быстро работает, но я так полагаю что сервер нужен для например смарт приложений где api нужно как просить чтобы оно работало не из России да, вот это чтобы ограничения снять тогда имеет смысл конечно то есть чтобы умные приложения писать для работы с нейронками популярными. И вот в этом, да, тогда я готов присоединиться тоже, наверное, если речь про разработку.",
}

type TestConfig struct {
	Openrouter struct {
		APIKey string `yaml:"api_key"`
		Models []struct {
			Name     string        `yaml:"name"`
			Cooldown time.Duration `yaml:"cooldown"`
			Limit    time.Duration `yaml:"limit"`
		} `yaml:"models"`
	} `yaml:"openrouter"`
	Prompts struct {
		SystemPrompt string `yaml:"system_prompt"`
		UserPrompt   string `yaml:"user_prompt"`
	} `yaml:"prompts"`
}

// OpenRouter client structures
type OpenrouterConfig struct {
	APIKey       string
	Model        string
	SystemPrompt string
	UserPrompt   string
}

type OpenrouterClient struct {
	config OpenrouterConfig
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type Response struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func NewOpenrouterClient(config OpenrouterConfig) *OpenrouterClient {
	return &OpenrouterClient{
		config: config,
	}
}

func (c *OpenrouterClient) Summarize(ctx context.Context, text string) (string, error) {
	log.Printf("Starting summarization with model: %s", c.config.Model)
	log.Printf("Text length: %d characters", len(text))

	// Use prompts from config or defaults
	systemPrompt := c.config.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = "Вы - помощник, который создает краткое содержание текста. Нужно сделать сначала краткое содержание спича в одну строчку. Затем, если спич длинный - дополнить развернутый пересказ в трех-семи строчках. Если же спич оригинальный короткий - ограничится только кратки. Эти два раздела назови как *Кратко* и *Подробнее* выделив звездочками для форматирования и добавив один разрыв"
	}

	userPrompt := c.config.UserPrompt
	if userPrompt == "" {
		userPrompt = "Create a summary of the following text:\n\n%s"
	}

	// Create request to OpenRouter
	request := Request{
		Model: c.config.Model,
		Messages: []Message{
			{
				Role:    "system",
				Content: systemPrompt,
			},
			{
				Role:    "user",
				Content: fmt.Sprintf(userPrompt, text),
			},
		},
	}

	// Convert request to JSON
	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// Record start time
	startTime := time.Now()

	// Execute request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	// Calculate execution time
	executionTime := time.Since(startTime)
	log.Printf("OpenRouter API call completed in %.2f seconds", executionTime.Seconds())

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenRouter API error: %s", resp.Status)
	}

	// Parse response
	var response Response
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	// Return content from first choice
	if len(response.Choices) > 0 {
		result := response.Choices[0].Message.Content
		log.Printf("Summarization completed, result length: %d characters", len(result))
		return result, nil
	}

	log.Printf("Summarization failed: no choices in response")
	return "", fmt.Errorf("no choices in response")
}

func main() {
	// Load configuration
	config, err := loadTestConfig("../recap.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	primaryModel := ""
	if len(config.Openrouter.Models) > 0 {
		primaryModel = config.Openrouter.Models[0].Name
	}
	if primaryModel == "" {
		log.Fatalf("No OpenRouter model configured")
	}

	// Create base OpenRouter configuration
	baseConfig := OpenrouterConfig{
		APIKey:       config.Openrouter.APIKey,
		Model:        primaryModel,
		SystemPrompt: config.Prompts.SystemPrompt,
		UserPrompt:   config.Prompts.UserPrompt,
	}

	modelsToTest := make([]string, 0, len(config.Openrouter.Models)+2)
	if primaryModel != "" {
		modelsToTest = append(modelsToTest, primaryModel)
	}
	for _, model := range config.Openrouter.Models {
		if model.Name != "" && !containsString(modelsToTest, model.Name) {
			modelsToTest = append(modelsToTest, model.Name)
		}
	}
	if len(modelsToTest) == 0 {
		log.Fatalf("No OpenRouter models available for testing")
	}

	// Different system prompt variants for testing
	systemPrompts := map[string]string{}
	if config.Prompts.SystemPrompt != "" {
		systemPrompts["configured"] = config.Prompts.SystemPrompt
	} else {
		systemPrompts["default"] = ""
	}

	fmt.Println("=== OpenRouter prompt testing ===")

	// Test each prompt with each phrase
	for promptName, systemPrompt := range systemPrompts {
		fmt.Printf("\n=== Testing prompt: %s ===\n", promptName)

		for _, modelName := range modelsToTest {
			fmt.Printf("\n>>> Model: %s <<<\n", modelName)
			for i, phrase := range testPhrases {
				fmt.Printf("\n--- Phrase %d ---\n", i+1)
				fmt.Printf("Original phrase: %s\n", phrase)

				// Create OpenRouter client with current system prompt
				clientConfig := OpenrouterConfig{
					APIKey:       baseConfig.APIKey,
					Model:        modelName,
					SystemPrompt: systemPrompt,
					UserPrompt:   baseConfig.UserPrompt,
				}

				client := NewOpenrouterClient(clientConfig)

				// Get summary
				ctx := context.Background()
				summary, err := client.Summarize(ctx, phrase)
				if err != nil {
					fmt.Printf("Error: %v\n", err)
				} else {
					fmt.Printf("Result:\n%s\n", summary)
				}
			}
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func loadTestConfig(filename string) (*TestConfig, error) {
	// Read configuration file
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	// Parse YAML
	var config TestConfig
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
