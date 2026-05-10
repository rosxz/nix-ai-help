package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"nix-ai-help/pkg/logger"
)

// BedrockClient implements the AIProvider interface for AWS Bedrock OpenAI-compatible endpoints.
type BedrockClient struct {
	APIKey     string
	APIURL     string
	Model      string
	HTTPClient *http.Client
	Logger     *logger.Logger
	CostConfig BedrockCostConfig
	Totals     BedrockCostTotals
	mu         sync.Mutex
}

// BedrockCostConfig defines per-1M token pricing for Bedrock models.
type BedrockCostConfig struct {
	PromptPer1M       float64
	CompletionPer1M   float64
	CachedPromptPer1M float64
}

// BedrockCostTotals tracks aggregate token usage and cost.
type BedrockCostTotals struct {
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	TotalCostUSD     float64
}

// NewBedrockClient creates a new Bedrock client with a base URL and model.
func NewBedrockClient(apiKey, baseURL, model string, costConfig BedrockCostConfig, log *logger.Logger) *BedrockClient {
	if baseURL == "" {
		baseURL = "https://bedrock-mantle.eu-north-1.api.aws/v1"
	}

	if model == "" {
		model = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	}

	if log == nil {
		log = logger.NewLogger()
	}
	if costConfig.CachedPromptPer1M == 0 {
		costConfig.CachedPromptPer1M = costConfig.PromptPer1M
	}

	return &BedrockClient{
		APIKey:     apiKey,
		APIURL:     buildBedrockAPIURL(baseURL),
		Model:      model,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Logger:     log,
		CostConfig: costConfig,
	}
}

type bedrockResponse struct {
	Choices []Choice      `json:"choices"`
	Usage   *bedrockUsage `json:"usage,omitempty"`
	Error   *bedrockError `json:"error,omitempty"`
}

type bedrockError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type bedrockUsage struct {
	PromptTokens     int                      `json:"prompt_tokens"`
	CompletionTokens int                      `json:"completion_tokens"`
	TotalTokens      int                      `json:"total_tokens"`
	PromptDetails    *bedrockPromptTokenUsage `json:"prompt_tokens_details,omitempty"`
}

type bedrockPromptTokenUsage struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

func buildBedrockAPIURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if strings.Contains(trimmed, "/chat/completions") {
		return trimmed
	}
	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed + "/chat/completions"
	}
	return trimmed + "/v1/chat/completions"
}

// GenerateResponseFromMessagesContext generates a response from Bedrock with context support.
func (client *BedrockClient) GenerateResponseFromMessagesContext(ctx context.Context, messages []Message) (string, error) {
	request := Request{
		Model:    client.Model,
		Messages: messages,
	}

	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", client.APIURL, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+client.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bedrock request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(bodyBytes))
		if bodyText == "" {
			return "", fmt.Errorf("bedrock returned status %d", resp.StatusCode)
		}
		return "", fmt.Errorf("bedrock returned status %d: %s", resp.StatusCode, bodyText)
	}

	var response bedrockResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if response.Error != nil {
		return "", fmt.Errorf("bedrock error: %s", response.Error.Message)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	if response.Choices[0].Message.Content != "" {
		client.recordUsage(response.Usage)
		return response.Choices[0].Message.Content, nil
	}
	if response.Choices[0].Text != "" {
		client.recordUsage(response.Usage)
		return response.Choices[0].Text, nil
	}
	return "", fmt.Errorf("no content in bedrock response")
}

func (client *BedrockClient) recordUsage(usage *bedrockUsage) {
	if usage == nil {
		return
	}

	cachedTokens := 0
	if usage.PromptDetails != nil {
		cachedTokens = usage.PromptDetails.CachedTokens
	}

	promptTokens := usage.PromptTokens
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens
	}

	effectivePrompt := promptTokens - cachedTokens
	completionTokens := usage.CompletionTokens

	cost := (float64(effectivePrompt)/1000000.0)*client.CostConfig.PromptPer1M +
		(float64(cachedTokens)/1000000.0)*client.CostConfig.CachedPromptPer1M +
		(float64(completionTokens)/1000000.0)*client.CostConfig.CompletionPer1M

	client.mu.Lock()
	client.Totals.PromptTokens += promptTokens
	client.Totals.CachedTokens += cachedTokens
	client.Totals.CompletionTokens += completionTokens
	client.Totals.TotalCostUSD += cost
	client.mu.Unlock()

	if client.Logger != nil {
		client.Logger.Info(fmt.Sprintf("Bedrock usage model=%s prompt=%d cached=%d completion=%d cost=$%.6f", client.Model, promptTokens, cachedTokens, completionTokens, cost))
	}
}

// GetCostTotals returns a snapshot of aggregated usage and costs.
func (client *BedrockClient) GetCostTotals() BedrockCostTotals {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.Totals
}

// Query implements the AIProvider interface (legacy signature for compatibility).
func (client *BedrockClient) Query(prompt string) (string, error) {
	messages := []Message{{Role: "user", Content: prompt}}
	return client.GenerateResponseFromMessagesContext(context.Background(), messages)
}

// QueryWithContext implements the Provider interface with context support for BedrockClient.
func (client *BedrockClient) QueryWithContext(ctx context.Context, prompt string) (string, error) {
	messages := []Message{{Role: "user", Content: prompt}}
	return client.GenerateResponseFromMessagesContext(ctx, messages)
}

// GenerateResponse implements the Provider interface with context support for BedrockClient.
func (client *BedrockClient) GenerateResponse(ctx context.Context, prompt string) (string, error) {
	return client.QueryWithContext(ctx, prompt)
}

// StreamResponse implements streaming for Bedrock OpenAI-compatible API.
func (client *BedrockClient) StreamResponse(ctx context.Context, prompt string) (<-chan StreamResponse, error) {
	responseChan := make(chan StreamResponse, 100)

	go func() {
		defer close(responseChan)

		messages := []Message{{Role: "user", Content: prompt}}
		request := StreamRequest{
			Model:    client.Model,
			Messages: messages,
			Stream:   true,
		}

		body, err := json.Marshal(request)
		if err != nil {
			responseChan <- StreamResponse{Error: fmt.Errorf("failed to marshal request: %w", err), Done: true}
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", client.APIURL, bytes.NewBuffer(body))
		if err != nil {
			responseChan <- StreamResponse{Error: fmt.Errorf("failed to create request: %w", err), Done: true}
			return
		}

		req.Header.Set("Authorization", "Bearer "+client.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.HTTPClient.Do(req)
		if err != nil {
			responseChan <- StreamResponse{Error: fmt.Errorf("bedrock request failed: %w", err), Done: true}
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			bodyText := strings.TrimSpace(string(bodyBytes))
			if bodyText == "" {
				responseChan <- StreamResponse{Error: fmt.Errorf("bedrock returned status %d", resp.StatusCode), Done: true}
				return
			}
			responseChan <- StreamResponse{Error: fmt.Errorf("bedrock returned status %d: %s", resp.StatusCode, bodyText), Done: true}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			if line == "" || !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				responseChan <- StreamResponse{Done: true}
				return
			}

			var streamResp OpenAIStreamResponse
			if err := json.Unmarshal([]byte(data), &streamResp); err != nil {
				continue
			}

			if len(streamResp.Choices) > 0 {
				content := streamResp.Choices[0].Delta.Content
				if content != "" {
					responseChan <- StreamResponse{
						Content: content,
						Done:    false,
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			responseChan <- StreamResponse{Error: fmt.Errorf("error reading stream: %w", err), Done: true}
		}
	}()

	return responseChan, nil
}
