package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// titleSystemPrompt is the persona/instruction for title distillation. Kept in
// sync with the legacy claude-CLI prompt: concise, no punctuation, no prefix —
// just the title text. Sent as the system message so the user message carries
// only the raw requirement content.
const titleSystemPrompt = `根据以下需求内容，提炼一个简洁的需求标题。
要求：不超过20个汉字（或15个英文单词）；不要加标点符号、引号或换行；直接输出标题文本，不要任何额外说明或前缀。`

// chatRequest is the OpenAI-compatible chat completions request body. Model
// uses omitempty so a self-hosted gateway with a default model can omit it.
type chatRequest struct {
	Model       string         `json:"model,omitempty"`
	Messages    []chatMessage  `json:"messages"`
	Temperature float64        `json:"temperature"`
	MaxTokens   int            `json:"max_tokens"`
	Stream      bool           `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the minimal slice of the OpenAI-compatible response needed
// to extract the assistant text.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// generateTitleViaHTTP distills a requirement title via a direct HTTP call to
// an OpenAI-compatible /chat/completions endpoint (e.g. DeepSeek). It bypasses
// the claude CLI for speed — title distillation needs no tool use. baseURL
// should be the API root (with or without a trailing slash, with or without
// /v1); the path /chat/completions is appended. A 10s timeout keeps the
// request fail-fast so the caller's fallback path is not stalled.
func generateTitleViaHTTP(baseURL, apiKey, model, content string) (string, error) {
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"

	body := chatRequest{
		Messages: []chatMessage{
			{Role: "system", Content: titleSystemPrompt},
			{Role: "user", Content: content},
		},
		Temperature: 0,
		MaxTokens:   64,
		Stream:      false,
	}
	if model != "" {
		body.Model = model
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("llm http: marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm http: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm http: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap, defensive
	if err != nil {
		return "", fmt.Errorf("llm http: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Truncate body to keep the error readable; it should never contain the
		// api key (the key is sent in the Authorization header, not the body).
		snippet := string(raw)
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return "", fmt.Errorf("llm http: status %d: %s", resp.StatusCode, strings.TrimSpace(snippet))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("llm http: decode response: %w", err)
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("llm http: empty response")
	}

	// Strip any surrounding quotes / whitespace the model may add despite
	// instructions (same post-processing as the legacy CLI path).
	title := strings.Trim(cr.Choices[0].Message.Content, "\"'` \n\r\t")
	if title == "" {
		return "", fmt.Errorf("llm http: empty title after trim")
	}
	return title, nil
}
