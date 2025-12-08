package main

import (
	"encoding/json"
	"fmt"
)

// --- OpenAI API Structs ---

type OpenAIMessage struct {
	Role          string           `json:"role"`
	Content       json.RawMessage  `json:"content"` // Use RawMessage to defer parsing
	ParsedContent []OpenAIContentPart `json:"-"`         // To store the unmarshaled content
}

// Custom UnmarshalJSON for OpenAIMessage to handle flexible Content field
func (m *OpenAIMessage) UnmarshalJSON(data []byte) error {
	type Alias OpenAIMessage // Create an alias to avoid infinite recursion

	aux := &struct{
		*Alias
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Try to unmarshal Content as a string
	var contentStr string
	if err := json.Unmarshal(m.Content, &contentStr); err == nil {
		// It's a string, convert to a single text part
		m.ParsedContent = []OpenAIContentPart{{
			Type: "text",
			Text: contentStr,
		}}
	} else {
		// It's not a string, try to unmarshal as an array of parts
		var contentParts []OpenAIContentPart
		if err := json.Unmarshal(m.Content, &contentParts); err == nil {
			m.ParsedContent = contentParts
		} else {
			return fmt.Errorf("messages.content must be a string or an array of content parts")
		}
	}
	return nil
}

type OpenAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

type OpenAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // low, high, auto
}

type OpenAICompletionRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
}

type OpenAICompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage,omitempty"`
}

type OpenAICompletionChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message,omitempty"`
	Delta        OpenAIDelta   `json:"delta,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

type OpenAIDelta struct {
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIModelsResponse struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}

type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// --- Gemini CLI Internal Structs (stream-json and json output) ---

type GeminiStreamEvent struct {
	Type      string `json:"type"` // e.g., "init", "message", "result"
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id,omitempty"` // Present in "init" event
	Model     string `json:"model,omitempty"`

	// For type "message"
	Role    string `json:"role,omitempty"` // e.g., "user", "assistant"
	Content string `json:"content,omitempty"`
	Delta   bool   `json:"delta,omitempty"` // true if it's a content delta

	// For type "result"
	Status string `json:"status,omitempty"` // e.g., "success"
	Stats  struct {
		TotalTokens  int `json:"total_tokens"`
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		DurationMs   int `json:"duration_ms"`
	} `json:"stats,omitempty"`
}

type GeminiCompletionResponse struct {
	Response string `json:"response"`
	Stats    struct {
		Models map[string]struct {
			API struct {
				TotalRequests  int `json:"totalRequests"`
				TotalErrors    int `json:"totalErrors"`
				TotalLatencyMs int `json:"totalLatencyMs"`
			} `json:"api"`
			Tokens struct {
				Prompt     int `json:"prompt"`
				Candidates int `json:"candidates"`
				Total      int `json:"total"`
				Cached     int `json:"cached"`
				Thoughts   int `json:"thoughts"`
				Tool       int `json:"tool"`
			} `json:"tokens"`
		} `json:"models"`
		Tools struct {
			TotalCalls     int `json:"totalCalls"`
			TotalSuccess   int `json:"totalSuccess"`
			TotalFail      int `json:"totalFail"`
			TotalDurationMs int `json:"totalDurationMs"`
		} `json:"tools"`
	} `json:"stats"`
}