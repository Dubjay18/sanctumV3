package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BuildAIContext takes the last 20 messages and formats them into a single contextual prompt.
func BuildAIContext(messages []string, prompt string) string {
	start := len(messages) - 20
	if start < 0 {
		start = 0
	}
	history := messages[start:]
	historyStr := strings.Join(history, "\n")
	return fmt.Sprintf("Context from Sanctum chat:\n%s\n\nUser question: %s", historyStr, prompt)
}

// QueryGemini calls Google's Gemini API via HTTP POST.
func QueryGemini(apiKey, contextualPrompt string) (string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=%s", apiKey)

	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type request struct {
		Contents []content `json:"contents"`
	}

	reqBody := request{
		Contents: []content{
			{
				Parts: []part{
					{Text: contextualPrompt},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini API returned status %s: %s", resp.Status, string(bodyBytes))
	}

	type geminiResponse struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	var geminiResp geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		return "", err
	}

	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from gemini API")
	}

	return geminiResp.Candidates[0].Content.Parts[0].Text, nil
}

// QueryAnthropic calls Anthropic's Claude API via HTTP POST.
func QueryAnthropic(apiKey, contextualPrompt string) (string, error) {
	url := "https://api.anthropic.com/v1/messages"

	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type request struct {
		Model     string    `json:"model"`
		MaxTokens int       `json:"max_tokens"`
		Messages  []message `json:"messages"`
	}

	reqBody := request{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1000,
		Messages: []message{
			{
				Role:    "user",
				Content: contextualPrompt,
			},
		},
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic API returned status %s: %s", resp.Status, string(bodyBytes))
	}

	type anthropicResponse struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	var anthropicResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		return "", err
	}

	if len(anthropicResp.Content) == 0 {
		return "", fmt.Errorf("empty response from anthropic API")
	}

	return anthropicResp.Content[0].Text, nil
}

// QueryOpenAI calls OpenAI's GPT API via HTTP POST.
func QueryOpenAI(apiKey, contextualPrompt string) (string, error) {
	url := "https://api.openai.com/v1/chat/completions"

	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type request struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}

	reqBody := request{
		Model: "gpt-4o",
		Messages: []message{
			{
				Role:    "user",
				Content: contextualPrompt,
			},
		},
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai API returned status %s: %s", resp.Status, string(bodyBytes))
	}

	type openAIResponse struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	var openAIResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openAIResp); err != nil {
		return "", err
	}

	if len(openAIResp.Choices) == 0 {
		return "", fmt.Errorf("empty response from openai API")
	}

	return openAIResp.Choices[0].Message.Content, nil
}
