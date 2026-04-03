package main

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// --- Anthropic Messages API request types ---

type messagesRequest struct {
	Model         string            `json:"model"`
	Messages      []json.RawMessage `json:"messages"`
	System        json.RawMessage   `json:"system,omitempty"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	MaxTokens     int               `json:"max_tokens"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	TopK          *int              `json:"top_k,omitempty"`
	Stream        bool              `json:"stream"`
	Metadata      json.RawMessage   `json:"metadata,omitempty"`
	Thinking      json.RawMessage   `json:"thinking,omitempty"`
}

// --- Translation functions ---

// translateAnthropicSystem converts the Anthropic top-level system field
// to a plain string for use as a Chat Completions system message.
// Handles both string and array-of-text-blocks formats.
func translateAnthropicSystem(system json.RawMessage) string {
	if len(system) == 0 || string(system) == "null" {
		return ""
	}

	// Try string first.
	var s string
	if json.Unmarshal(system, &s) == nil {
		return s
	}

	// Array of text blocks: [{"type":"text","text":"...","cache_control":...}]
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(system, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// translateAnthropicMessages converts Anthropic message format to Chat
// Completions messages. Handles text, tool_use, tool_result, and strips
// thinking/redacted_thinking blocks.
func translateAnthropicMessages(msgs []json.RawMessage) ([]map[string]any, error) {
	var result []map[string]any

	for _, raw := range msgs {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Role {
		case "user":
			result = append(result, translateUserMessage(msg.Content)...)
		case "assistant":
			if m := translateAssistantMessage(msg.Content); m != nil {
				result = append(result, m)
			}
		}
	}

	return result, nil
}

// translateUserMessage converts an Anthropic user message to one or more
// Chat Completions messages. tool_result blocks become separate role:tool messages.
func translateUserMessage(content json.RawMessage) []map[string]any {
	// String content: pass through directly.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []map[string]any{{"role": "user", "content": s}}
	}

	// Array of content blocks.
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return []map[string]any{{"role": "user", "content": string(content)}}
	}

	var userParts []any
	var toolResults []map[string]any

	for _, block := range blocks {
		var blockType string
		json.Unmarshal(block["type"], &blockType)

		switch blockType {
		case "text":
			var text string
			json.Unmarshal(block["text"], &text)
			userParts = append(userParts, map[string]any{"type": "text", "text": text})

		case "image":
			userParts = append(userParts, translateImageBlock(block))

		case "tool_result":
			var toolUseID string
			json.Unmarshal(block["tool_use_id"], &toolUseID)
			content := extractToolResultContent(block["content"])
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      content,
			})

		case "thinking", "redacted_thinking":
			slog.Debug("stripping thinking block from user message", "type", blockType)
			continue

		default:
			slog.Debug("skipping unknown user content block type", "type", blockType)
		}
	}

	var result []map[string]any

	// Emit user message if there are any text/image parts.
	if len(userParts) > 0 {
		// If only one text part, simplify to string content.
		if len(userParts) == 1 {
			if tp, ok := userParts[0].(map[string]any); ok && tp["type"] == "text" {
				result = append(result, map[string]any{"role": "user", "content": tp["text"]})
			} else {
				result = append(result, map[string]any{"role": "user", "content": userParts})
			}
		} else {
			result = append(result, map[string]any{"role": "user", "content": userParts})
		}
	}

	// Emit tool result messages after the user message.
	result = append(result, toolResults...)

	return result
}

// translateAssistantMessage converts an Anthropic assistant message to a
// Chat Completions message. text blocks become content, tool_use blocks
// become tool_calls, thinking blocks are stripped.
func translateAssistantMessage(content json.RawMessage) map[string]any {
	// String content: pass through.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return map[string]any{"role": "assistant", "content": s}
	}

	// Array of content blocks.
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return map[string]any{"role": "assistant", "content": string(content)}
	}

	var textParts []string
	var toolCalls []map[string]any

	for _, block := range blocks {
		var blockType string
		json.Unmarshal(block["type"], &blockType)

		switch blockType {
		case "text":
			var text string
			json.Unmarshal(block["text"], &text)
			textParts = append(textParts, text)

		case "tool_use":
			var id, name string
			json.Unmarshal(block["id"], &id)
			json.Unmarshal(block["name"], &name)
			// input is an object in Anthropic, needs to be JSON string in OpenAI.
			argsBytes, _ := json.Marshal(json.RawMessage(block["input"]))
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsBytes),
				},
			})

		case "thinking", "redacted_thinking":
			slog.Debug("stripping thinking block from assistant message", "type", blockType)

		default:
			slog.Debug("skipping unknown assistant content block type", "type", blockType)
		}
	}

	msg := map[string]any{"role": "assistant"}

	text := strings.Join(textParts, "")
	if text != "" {
		msg["content"] = text
	} else {
		msg["content"] = nil
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	return msg
}

// translateImageBlock converts an Anthropic image content block to OpenAI format.
func translateImageBlock(block map[string]json.RawMessage) map[string]any {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	json.Unmarshal(block["source"], &source)

	switch source.Type {
	case "base64":
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + source.MediaType + ";base64," + source.Data,
			},
		}
	case "url":
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": source.URL},
		}
	}
	return map[string]any{"type": "text", "text": "[unsupported image format]"}
}

// extractToolResultContent extracts a string from a tool_result content field,
// which can be a string or an array of content blocks.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Array of blocks: concatenate text parts.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// --- Tool translation ---

// translateAnthropicToolsToChat converts Anthropic tool definitions to Chat
// Completions format: {name, input_schema} → {type: "function", function: {name, parameters}}.
// Server tools (web_search_20250305 etc.) are stripped.
func translateAnthropicToolsToChat(tools []json.RawMessage) []map[string]any {
	var result []map[string]any
	for _, raw := range tools {
		var tool struct {
			Name        string          `json:"name"`
			Type        string          `json:"type"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		// Skip server tools (web_search, code_execution, etc.)
		if tool.Type != "" && tool.Type != "custom" {
			slog.Debug("stripping server tool from translated request", "type", tool.Type, "name", tool.Name)
			continue
		}
		fn := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			fn["description"] = tool.Description
		}
		if len(tool.InputSchema) > 0 {
			fn["parameters"] = json.RawMessage(tool.InputSchema)
		}
		result = append(result, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return result
}

// translateAnthropicToolChoice converts Anthropic tool_choice to OpenAI format.
func translateAnthropicToolChoice(tc json.RawMessage, hasTools bool) json.RawMessage {
	if len(tc) == 0 || string(tc) == "null" || !hasTools {
		return nil
	}
	var choice struct {
		Type                    string `json:"type"`
		Name                    string `json:"name"`
		DisableParallelToolUse  bool   `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(tc, &choice) != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		b, _ := json.Marshal("auto")
		return b
	case "any":
		b, _ := json.Marshal("required")
		return b
	case "tool":
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": choice.Name},
		})
		return b
	case "none":
		b, _ := json.Marshal("none")
		return b
	}
	return nil
}

// --- Full request builder ---

// buildChatRequestFromAnthropic translates a complete Anthropic Messages API
// request into a Chat Completions request.
func buildChatRequestFromAnthropic(req messagesRequest, backendModel string) (map[string]any, error) {
	var chatMessages []map[string]any

	// System prompt → system message.
	if sys := translateAnthropicSystem(req.System); sys != "" {
		chatMessages = append(chatMessages, map[string]any{
			"role":    "system",
			"content": sys,
		})
	}

	// Translate conversation messages.
	translated, err := translateAnthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	chatMessages = append(chatMessages, translated...)

	chatReq := map[string]any{
		"model":    backendModel,
		"messages": chatMessages,
		"stream":   req.Stream,
	}

	if req.Stream {
		chatReq["stream_options"] = map[string]any{"include_usage": true}
	}

	// Tools.
	if len(req.Tools) > 0 {
		if tools := translateAnthropicToolsToChat(req.Tools); len(tools) > 0 {
			chatReq["tools"] = tools
			if tc := translateAnthropicToolChoice(req.ToolChoice, true); tc != nil {
				chatReq["tool_choice"] = json.RawMessage(tc)
			}
		}
	}

	// max_tokens → max_completion_tokens (required in Anthropic, map to OpenAI).
	if req.MaxTokens > 0 {
		chatReq["max_completion_tokens"] = req.MaxTokens
	}

	// stop_sequences → stop.
	if len(req.StopSequences) > 0 {
		chatReq["stop"] = req.StopSequences
	}

	// Temperature (Anthropic max 1.0, OpenAI max 2.0 — pass through).
	if req.Temperature != nil {
		chatReq["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chatReq["top_p"] = *req.TopP
	}
	// top_k: no OpenAI equivalent, dropped.
	if req.TopK != nil {
		slog.Debug("dropping top_k (no OpenAI equivalent)", "top_k", *req.TopK)
	}

	// thinking config: dropped for Chat Completions backends.
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		slog.Debug("dropping thinking config for translated request")
	}

	// metadata.user_id → user.
	if len(req.Metadata) > 0 {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if json.Unmarshal(req.Metadata, &meta) == nil && meta.UserID != "" {
			chatReq["user"] = meta.UserID
		}
	}

	slog.Debug("built translated chat request",
		"model", backendModel,
		"messages", len(chatMessages),
		"tools", len(req.Tools),
		"stream", req.Stream,
		"max_tokens", req.MaxTokens)

	return chatReq, nil
}

// mapFinishToStopReason maps OpenAI finish_reason to Anthropic stop_reason.
func mapFinishToStopReason(finishReason string) string {
	switch finishReason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn" // no Anthropic equivalent, treat as end
	default:
		return "end_turn"
	}
}
