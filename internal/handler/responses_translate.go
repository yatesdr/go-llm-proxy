// responses_translate.go contains the translation logic for converting
// Responses API requests into Chat Completions format.
package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"go-llm-proxy/internal/pipeline"
)

// --- Request types ---

type responsesRequest struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream"`
	Reasoning         *reasoningConfig  `json:"reasoning,omitempty"`
	Text              json.RawMessage   `json:"text,omitempty"`
	User              string            `json:"user,omitempty"`
}

type reasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// inputItem represents an item in the Responses API input array.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"` // function_call
	Input     string          `json:"input"`     // custom_tool_call
	Output    json.RawMessage `json:"output"`    // *_output items (string, array, or object)
	Action    json.RawMessage `json:"action"`    // local_shell_call
	Status    string          `json:"status"`
}

// --- Input translation (Responses API -> Chat Completions) ---

// translateInput converts Responses API input items into Chat Completions messages.
func translateInput(input json.RawMessage, instructions string) ([]map[string]any, error) {
	var messages []map[string]any

	if instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	// String input: wrap as a single user message.
	var inputStr string
	if json.Unmarshal(input, &inputStr) == nil {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": inputStr,
		})
		return messages, nil
	}

	// Array input.
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or array")
	}

	for _, raw := range items {
		var item inputItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}

		switch {
		case item.Type == "function_call" || item.Type == "local_shell_call" || item.Type == "custom_tool_call":
			args := item.Arguments
			if args == "" && item.Input != "" {
				args = item.Input
			}
			if args == "" && len(item.Action) > 0 && string(item.Action) != "null" {
				args = string(item.Action)
			}
			if args == "" {
				args = "{}"
			}
			name := item.Name
			if name == "" && item.Type == "local_shell_call" {
				name = "shell"
			}

			tc := map[string]any{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}

			// Merge into the last message if it's an assistant message.
			merged := false
			if n := len(messages); n > 0 {
				last := messages[n-1]
				if last["role"] == "assistant" {
					existing, _ := last["tool_calls"].([]any)
					last["tool_calls"] = append(existing, tc)
					merged = true
				}
			}
			if !merged {
				messages = append(messages, map[string]any{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": []any{tc},
				})
			}

		case item.Type == "function_call_output" || item.Type == "local_shell_call_output" ||
			item.Type == "custom_tool_call_output" || item.Type == "mcp_tool_call_output":
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      translateToolOutput(item.Output),
			})

		case item.Type == "reasoning" || item.Type == "compaction" ||
			item.Type == "tool_search_call" || item.Type == "tool_search_output" ||
			item.Type == "web_search_call" || item.Type == "image_generation_call" ||
			item.Type == "mcp_list_tools":
			continue // no Chat Completions equivalent

		case item.Role != "":
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			content := translateContentForChat(item.Content, item.Role)
			messages = append(messages, map[string]any{
				"role":    role,
				"content": content,
			})

		default:
			continue
		}
	}

	if len(messages) == 0 {
		return nil, fmt.Errorf("no valid input items")
	}
	return messages, nil
}

// translateContentForChat converts Responses API content to Chat Completions format.
func translateContentForChat(content json.RawMessage, role string) any {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}

	// String content: pass through.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}

	// Array of content parts.
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return string(content)
	}

	// For assistant messages, extract text from output_text parts.
	if role == "assistant" {
		var texts []string
		for _, p := range parts {
			var partType string
			json.Unmarshal(p["type"], &partType)
			if partType == "output_text" {
				var text string
				json.Unmarshal(p["text"], &text)
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "")
	}

	// For user messages, translate input part types.
	var translated []map[string]any
	for _, p := range parts {
		var partType string
		json.Unmarshal(p["type"], &partType)

		switch partType {
		case "input_text":
			var text string
			json.Unmarshal(p["text"], &text)
			translated = append(translated, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image":
			var url string
			json.Unmarshal(p["image_url"], &url)
			part := map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			}
			if detail, ok := p["detail"]; ok {
				var d string
				json.Unmarshal(detail, &d)
				part["image_url"].(map[string]any)["detail"] = d
			}
			translated = append(translated, part)
		default:
			var part map[string]any
			raw, _ := json.Marshal(p)
			json.Unmarshal(raw, &part)
			translated = append(translated, part)
		}
	}
	if len(translated) > 0 {
		return translated
	}
	return ""
}

// --- Tool output translation ---

// translateToolOutput converts a Responses API tool output value into Chat Completions
// content. Handles three formats:
//   - string: passed through as-is (common case)
//   - array: content items like [{"type":"input_image","image_url":"data:..."}] — translated
//     to Chat Completions multimodal content parts
//   - object: structured output like {"content":"text","success":true} or
//     {"content":[...],"success":true} — content field extracted and translated
func translateToolOutput(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	// Try string first (most common).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try array of content items.
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return translateOutputContentItems(arr)
	}

	// Try object with content + success fields.
	var obj struct {
		Content json.RawMessage `json:"content"`
		Success *bool           `json:"success"`
	}
	if json.Unmarshal(raw, &obj) == nil && len(obj.Content) > 0 {
		// Content can be a string or an array.
		var contentStr string
		if json.Unmarshal(obj.Content, &contentStr) == nil {
			return contentStr
		}
		var contentArr []map[string]json.RawMessage
		if json.Unmarshal(obj.Content, &contentArr) == nil {
			return translateOutputContentItems(contentArr)
		}
	}

	// Fallback: pass raw JSON as string.
	return string(raw)
}

// translateOutputContentItems converts Responses API content items (input_text, input_image)
// to Chat Completions format (text, image_url).
func translateOutputContentItems(items []map[string]json.RawMessage) []map[string]any {
	var parts []map[string]any
	for _, item := range items {
		var partType string
		json.Unmarshal(item["type"], &partType)

		switch partType {
		case "input_text":
			var text string
			json.Unmarshal(item["text"], &text)
			parts = append(parts, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image":
			var url string
			json.Unmarshal(item["image_url"], &url)
			part := map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			}
			if detail, ok := item["detail"]; ok {
				var d string
				json.Unmarshal(detail, &d)
				if d != "" {
					part["image_url"].(map[string]any)["detail"] = d
				}
			}
			parts = append(parts, part)
		default:
			// Pass through unknown types as-is.
			var part map[string]any
			raw, _ := json.Marshal(item)
			json.Unmarshal(raw, &part)
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// --- Tool translation ---

// translateTools converts Responses API tool definitions to Chat Completions format.
// Non-function tools (web_search_preview, etc.) are stripped and returned in the second value.
func translateTools(tools []json.RawMessage) ([]map[string]any, []string) {
	var result []map[string]any
	var strippedToolTypes []string
	for _, raw := range tools {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		}
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		if tool.Type != "function" {
			slog.Debug("stripping non-function tool from translated request", "type", tool.Type)
			strippedToolTypes = append(strippedToolTypes, tool.Type)
			continue
		}
		fn := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			fn["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 {
			fn["parameters"] = json.RawMessage(tool.Parameters)
		}
		if tool.Strict != nil {
			fn["strict"] = *tool.Strict
		}
		result = append(result, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return result, strippedToolTypes
}

// --- Text format translation ---

// translateTextFormat converts Responses API text.format to Chat Completions response_format.
func translateTextFormat(text json.RawMessage) json.RawMessage {
	if len(text) == 0 {
		return nil
	}
	var tf struct {
		Format struct {
			Type   string          `json:"type"`
			Name   string          `json:"name,omitempty"`
			Schema json.RawMessage `json:"schema,omitempty"`
			Strict *bool           `json:"strict,omitempty"`
		} `json:"format"`
	}
	if json.Unmarshal(text, &tf) != nil {
		return nil
	}
	switch tf.Format.Type {
	case "json_schema":
		schema := map[string]any{"name": tf.Format.Name}
		if len(tf.Format.Schema) > 0 {
			schema["schema"] = json.RawMessage(tf.Format.Schema)
		}
		if tf.Format.Strict != nil {
			schema["strict"] = *tf.Format.Strict
		}
		result, _ := json.Marshal(map[string]any{
			"type":        "json_schema",
			"json_schema": schema,
		})
		return result
	case "json_object":
		result, _ := json.Marshal(map[string]any{"type": "json_object"})
		return result
	}
	return nil // "text" format needs no response_format
}

// --- Chat Completions request builder ---

func buildChatRequest(req responsesRequest, backendModel string, messages []map[string]any) map[string]any {
	chatReq := map[string]any{
		"model":    backendModel,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.Stream {
		chatReq["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		tools, strippedToolTypes := translateTools(req.Tools)
		if len(tools) > 0 {
			chatReq["tools"] = tools
			// Only include tool_choice and parallel_tool_calls when tools are present.
			if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
				chatReq["tool_choice"] = json.RawMessage(req.ToolChoice)
			}
			if req.ParallelToolCalls != nil {
				chatReq["parallel_tool_calls"] = *req.ParallelToolCalls
			}
		}
		if len(strippedToolTypes) > 0 {
			chatReq[pipeline.InternalKeyStrippedTools] = strippedToolTypes
		}
	}
	if req.Temperature != nil {
		chatReq["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chatReq["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		chatReq["max_completion_tokens"] = *req.MaxOutputTokens
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		chatReq["reasoning_effort"] = req.Reasoning.Effort
	}
	if len(req.Text) > 0 {
		if rf := translateTextFormat(req.Text); rf != nil {
			chatReq["response_format"] = json.RawMessage(rf)
		}
	}
	if req.User != "" {
		chatReq["user"] = req.User
	}
	return chatReq
}
