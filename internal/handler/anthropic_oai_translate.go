package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
)

const defaultAnthropicChatMaxTokens = 4096

// buildAnthropicRequestFromChat translates an OpenAI Chat Completions request
// into the Anthropic Messages wire shape. The parsed Chat request is returned
// so dispatch can honor stream_options without decoding the body twice.
func buildAnthropicRequestFromChat(body []byte) (map[string]any, *chatRequest, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("decoding chat completions request: %w", err)
	}
	if req.Model == "" {
		return nil, nil, fmt.Errorf("missing model")
	}

	system, messages, err := translateChatMessagesToAnthropic(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("at least one user or assistant message is required")
	}

	out := map[string]any{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": defaultAnthropicChatMaxTokens,
		"stream":     req.Stream,
	}
	if system != "" {
		out["system"] = system
	}
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		out["max_tokens"] = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}
	if stops := translateChatStop(req.Stop); len(stops) > 0 {
		out["stop_sequences"] = stops
	}
	if req.User != "" {
		out["metadata"] = map[string]any{"user_id": req.User}
	}

	if len(req.Tools) > 0 {
		tools := translateChatToolsToAnthropic(req.Tools)
		if len(tools) > 0 {
			out["tools"] = tools
			if choice := translateChatToolChoiceToAnthropic(req.ToolChoice, req.ParallelToolCalls); choice != nil {
				out["tool_choice"] = choice
			}
		}
	}

	return out, &req, nil
}

// translateChatMessagesToAnthropic extracts system/developer prompts and
// converts the remaining messages to Anthropic content blocks. Consecutive
// OpenAI tool messages are grouped into one user turn of tool_result blocks.
func translateChatMessagesToAnthropic(rawMessages []json.RawMessage) (string, []map[string]any, error) {
	var systemParts []string
	var messages []map[string]any
	var pendingToolResults []map[string]any

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": pendingToolResults,
		})
		pendingToolResults = nil
	}

	for _, raw := range rawMessages {
		var msg struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			ToolCalls  []json.RawMessage `json:"tool_calls,omitempty"`
			ToolCallID string            `json:"tool_call_id,omitempty"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			return "", nil, fmt.Errorf("decoding chat message: %w", err)
		}

		switch msg.Role {
		case "system", "developer":
			flushToolResults()
			if text := chatContentToText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}

		case "user":
			flushToolResults()
			content := translateChatUserContentToAnthropic(msg.Content)
			if len(content) == 0 {
				content = []map[string]any{{"type": "text", "text": ""}}
			}
			messages = append(messages, map[string]any{"role": "user", "content": content})

		case "assistant":
			flushToolResults()
			content := translateChatAssistantContentToAnthropic(msg.Content, msg.ToolCalls)
			if len(content) == 0 {
				content = []map[string]any{{"type": "text", "text": ""}}
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": content})

		case "tool":
			if msg.ToolCallID == "" {
				slog.Debug("skipping chat tool message without tool_call_id")
				continue
			}
			pendingToolResults = append(pendingToolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": msg.ToolCallID,
				"content":     chatToolResultText(msg.Content),
			})

		default:
			slog.Debug("skipping unsupported chat message role for anthropic backend", "role", msg.Role)
		}
	}
	flushToolResults()

	return strings.Join(systemParts, "\n"), messages, nil
}

func translateChatUserContentToAnthropic(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"type": "text", "text": text}}
	}

	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return []map[string]any{{"type": "text", "text": string(raw)}}
	}
	result := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		var partType string
		json.Unmarshal(part["type"], &partType)
		var translated map[string]any
		switch partType {
		case "text", "input_text":
			var value string
			if json.Unmarshal(part["text"], &value) == nil {
				translated = map[string]any{"type": "text", "text": value}
			}
		case "image_url", "input_image":
			translated = translateChatImageToAnthropic(part)
		default:
			slog.Debug("skipping unsupported chat content part for anthropic backend", "type", partType)
		}
		if translated == nil {
			continue
		}
		if cacheControl, ok := part["cache_control"]; ok && len(cacheControl) > 0 {
			translated["cache_control"] = json.RawMessage(cacheControl)
		}
		result = append(result, translated)
	}
	return result
}

func translateChatAssistantContentToAnthropic(content json.RawMessage, toolCalls []json.RawMessage) []map[string]any {
	var result []map[string]any
	if text := chatContentToText(content); text != "" {
		result = append(result, map[string]any{"type": "text", "text": text})
	}
	for _, raw := range toolCalls {
		var call struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &call) != nil || call.Function.Name == "" {
			continue
		}
		var input any = map[string]any{}
		if call.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
				input = map[string]any{}
			}
		}
		result = append(result, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": input,
		})
	}
	return result
}

func chatToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	if text = chatContentToText(raw); text != "" {
		return text
	}
	return string(raw)
}

func translateChatImageToAnthropic(part map[string]json.RawMessage) map[string]any {
	var imageURL string
	for _, key := range []string{"image_url", "url"} {
		raw, ok := part[key]
		if !ok {
			continue
		}
		if json.Unmarshal(raw, &imageURL) != nil {
			var nested struct {
				URL string `json:"url"`
			}
			json.Unmarshal(raw, &nested)
			imageURL = nested.URL
		}
		if imageURL != "" {
			break
		}
	}
	if imageURL == "" {
		return nil
	}
	if strings.HasPrefix(imageURL, "data:") {
		mediaType, data, ok := parseDataURL(imageURL)
		if !ok {
			return nil
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": mediaType, "data": data,
			},
		}
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": imageURL},
	}
}

func translateChatToolsToAnthropic(rawTools []json.RawMessage) []map[string]any {
	var tools []map[string]any
	for _, raw := range rawTools {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &tool) != nil || (tool.Type != "" && tool.Type != "function") || tool.Function.Name == "" {
			continue
		}
		translated := map[string]any{
			"name":         tool.Function.Name,
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}
		if tool.Function.Description != "" {
			translated["description"] = tool.Function.Description
		}
		if len(tool.Function.Parameters) > 0 && string(tool.Function.Parameters) != "null" {
			translated["input_schema"] = json.RawMessage(tool.Function.Parameters)
		}
		tools = append(tools, translated)
	}
	return tools
}

func translateChatToolChoiceToAnthropic(raw json.RawMessage, parallel *bool) map[string]any {
	choice := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			switch value {
			case "auto", "none":
				choice["type"] = value
			case "required":
				choice["type"] = "any"
			}
		} else {
			var named struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(raw, &named) == nil && named.Type == "function" && named.Function.Name != "" {
				choice["type"] = "tool"
				choice["name"] = named.Function.Name
			}
		}
	}
	if parallel != nil && !*parallel {
		if len(choice) == 0 {
			choice["type"] = "auto"
		}
		choice["disable_parallel_tool_use"] = true
	}
	if len(choice) == 0 {
		return nil
	}
	return choice
}

type anthropicChatUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicChatResponse struct {
	ID         string                       `json:"id"`
	Model      string                       `json:"model"`
	Content    []map[string]json.RawMessage `json:"content"`
	StopReason string                       `json:"stop_reason"`
	Usage      anthropicChatUsage           `json:"usage"`
}

// buildChatResponseFromAnthropic converts a non-streaming Anthropic Message
// into the OpenAI ChatCompletion shape expected by the caller.
func buildChatResponseFromAnthropic(body []byte, modelName string) (*api.ChatResponse, *api.ChunkUsage, error) {
	var upstream anthropicChatResponse
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, nil, fmt.Errorf("decoding anthropic response: %w", err)
	}

	var textParts []string
	var reasoningParts []string
	var toolCalls []api.ChatChoiceToolCall
	for _, block := range upstream.Content {
		var blockType string
		json.Unmarshal(block["type"], &blockType)
		switch blockType {
		case "text":
			var text string
			if json.Unmarshal(block["text"], &text) == nil {
				textParts = append(textParts, text)
			}
		case "thinking":
			var thinking string
			if json.Unmarshal(block["thinking"], &thinking) == nil && thinking != "" {
				reasoningParts = append(reasoningParts, thinking)
			}
		case "tool_use":
			var id, name string
			json.Unmarshal(block["id"], &id)
			json.Unmarshal(block["name"], &name)
			arguments := "{}"
			if input := block["input"]; len(input) > 0 && string(input) != "null" {
				arguments = string(input)
			}
			call := api.ChatChoiceToolCall{ID: id, Type: "function"}
			call.Function.Name = name
			call.Function.Arguments = arguments
			toolCalls = append(toolCalls, call)
		}
	}

	var content *string
	if len(textParts) > 0 {
		joined := strings.Join(textParts, "")
		content = &joined
	}
	message := api.ChatChoiceMsg{Role: "assistant", Content: content, ToolCalls: toolCalls}
	if len(reasoningParts) > 0 {
		reasoning := strings.Join(reasoningParts, "")
		message.Reasoning = &reasoning
	}

	promptTokens := upstream.Usage.InputTokens + upstream.Usage.CacheCreationInputTokens + upstream.Usage.CacheReadInputTokens
	usage := &api.ChunkUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: upstream.Usage.OutputTokens,
		TotalTokens:      promptTokens + upstream.Usage.OutputTokens,
	}
	if upstream.Usage.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &api.PromptTokensDetails{CachedTokens: upstream.Usage.CacheReadInputTokens}
	}

	return &api.ChatResponse{
		ID:      translatedChatCompletionID(upstream.ID),
		Object:  "chat.completion",
		Model:   modelName,
		Created: time.Now().Unix(),
		Choices: []api.ChatChoice{{
			Index: 0, Message: message, FinishReason: mapAnthropicStopReasonToChat(upstream.StopReason),
		}},
		Usage: usage,
	}, usage, nil
}

func translatedChatCompletionID(upstreamID string) string {
	if strings.HasPrefix(upstreamID, "chatcmpl-") {
		return upstreamID
	}
	return api.RandomID("chatcmpl-")
}

func mapAnthropicStopReasonToChat(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "refusal":
		return "content_filter"
	case "end_turn", "stop_sequence", "pause_turn", "":
		return "stop"
	default:
		return "stop"
	}
}
