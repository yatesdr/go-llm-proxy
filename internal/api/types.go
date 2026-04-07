package api

import (
	"crypto/rand"
	"encoding/hex"
)

// ChatCompletionsPath is the backend endpoint for Chat Completions requests.
const ChatCompletionsPath = "/chat/completions"

const MaxRequestBodySize = 50 * 1024 * 1024

// MaxResponseBodySize limits total bytes proxied from upstream (100 MB).
// Prevents a broken or malicious backend from consuming unbounded resources.
// Normal LLM responses are well under this; even a very long streaming completion
// at 100K tokens is roughly 400 KB.
const MaxResponseBodySize = 100 * 1024 * 1024

// Chat Completions streaming chunk types.

type ChatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *ChunkUsage   `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type ChunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	Reasoning        *string         `json:"reasoning,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChunkToolCall `json:"tool_calls,omitempty"`
}

type ChunkToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function *ChunkToolFn `json:"function,omitempty"`
}

type ChunkToolFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ChunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Chat Completions non-streaming response types.

type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Created int64        `json:"created"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChunkUsage  `json:"usage,omitempty"`
}

type ChatChoice struct {
	Index        int           `json:"index"`
	Message      ChatChoiceMsg `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type ChatChoiceMsg struct {
	Role             string               `json:"role"`
	Content          *string              `json:"content"`
	Reasoning        *string              `json:"reasoning,omitempty"`
	ReasoningContent *string              `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatChoiceToolCall `json:"tool_calls,omitempty"`
}

// EffectiveReasoning returns the reasoning text from whichever field the backend
// populated: "reasoning" (OpenAI convention) or "reasoning_content" (DeepSeek convention).
func (m *ChatChoiceMsg) EffectiveReasoning() *string {
	if m.Reasoning != nil {
		return m.Reasoning
	}
	return m.ReasoningContent
}

// EffectiveReasoning returns the reasoning text from whichever field the backend
// populated: "reasoning" or "reasoning_content".
func (d *ChunkDelta) EffectiveReasoning() *string {
	if d.Reasoning != nil {
		return d.Reasoning
	}
	return d.ReasoningContent
}

type ChatChoiceToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// RandomID generates a random identifier with the given prefix.
func RandomID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
