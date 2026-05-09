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
	"time"
)

// BedrockClient implements the AIProvider interface for AWS Bedrock OpenAI-compatible endpoints.
type BedrockClient struct {
	APIKey     string
	APIURL     string
	Model      string
	HTTPClient *http.Client
}

// NewBedrockClient creates a new Bedrock client with a base URL and model.
func NewBedrockClient(apiKey, baseURL, model string) *BedrockClient {
	if baseURL == "" {
		baseURL = "https://bedrock-mantle.eu-north-1.api.aws/v1"
	}

	if model == "" {
		model = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	}

	return &BedrockClient{
		APIKey:     apiKey,
		APIURL:     buildBedrockAPIURL(baseURL),
		Model:      model,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
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

	var response Response
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	if response.Choices[0].Message.Content != "" {
		return response.Choices[0].Message.Content, nil
	}
	if response.Choices[0].Text != "" {
		return response.Choices[0].Text, nil
	}
	return "", fmt.Errorf("no content in bedrock response")
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
