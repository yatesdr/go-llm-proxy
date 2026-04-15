package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"go-llm-proxy/internal/api"
)

// --- OAI Chat Completions -> Bedrock Converse ---
//
// This translator lets clients that speak OpenAI Chat Completions reach a
// Bedrock-hosted model unchanged. Converse is the Anthropic-flavored shape;
// Chat Completions is OpenAI's. The non-trivial mappings:
//
//   * Tool messages: OAI emits tool results as standalone role:"tool"
//     messages with a tool_call_id linking back to the assistant's
//     tool_calls[].id. Converse expects the result inline as a toolResult
//     content block on the *next user message*. We collapse runs of
//     consecutive tool messages into a synthetic user turn.
//
//   * Assistant tool_calls: OAI separates content (text) and tool_calls
//     (structured) into sibling fields. Converse interleaves them as
//     content blocks within a single assistant message.
//
//   * Images: OAI uses {type:"image_url", image_url:{url:"data:..."}}.
//     Converse needs raw base64 bytes plus a format tag. We decode the
//     data URL and re-encode in Converse shape; non-data URLs are dropped
//     with a warning since Bedrock doesn't fetch external URLs.

// chatRequest is a minimal OAI Chat Completions request decoder. It captures
// the fields we need for translation; unknown fields are ignored. Sampling
// fields are pointers so we can distinguish "not set" from "set to zero".
type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	Tools               []json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	Stop                json.RawMessage   `json:"stop,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       json.RawMessage   `json:"stream_options,omitempty"`
	User                string            `json:"user,omitempty"`
}

// buildConverseRequestFromChat parses an OAI Chat Completions request body
// and returns a Converse-shaped request map plus the parsed request (for
// callers that need stream/etc.).
func buildConverseRequestFromChat(body []byte) (map[string]any, *chatRequest, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("decoding chat completions request: %w", err)
	}

	out := map[string]any{}

	systemText, converseMsgs, err := translateChatMessagesToConverse(req.Messages)
	if err != nil {
		return nil, nil, err
	}
	if systemText != "" {
		out["system"] = []map[string]any{{"text": systemText}}
	}
	out["messages"] = converseMsgs

	inference := map[string]any{}
	// max_completion_tokens (newer field) takes precedence over max_tokens.
	if req.MaxCompletionTokens != nil {
		inference["maxTokens"] = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		inference["maxTokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		inference["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		inference["topP"] = *req.TopP
	}
	if stop := translateChatStop(req.Stop); len(stop) > 0 {
		inference["stopSequences"] = stop
	}
	if len(inference) > 0 {
		out["inferenceConfig"] = inference
	}

	if len(req.Tools) > 0 {
		if cfg := translateChatToolsToConverse(req.Tools, req.ToolChoice); cfg != nil {
			out["toolConfig"] = cfg
		}
	}

	return out, &req, nil
}

// translateChatStop normalizes the OAI `stop` field, which is either a
// single string or a string array, into Converse's stopSequences array.
func translateChatStop(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	return nil
}

// translateChatMessagesToConverse converts OAI messages to a system prompt
// (concatenated from any role:system messages) plus a sequence of Converse
// user/assistant messages. Tool messages are merged into synthetic user
// turns carrying toolResult content blocks.
func translateChatMessagesToConverse(msgs []json.RawMessage) (string, []map[string]any, error) {
	var systemParts []string
	var out []map[string]any

	// pendingToolResults accumulates toolResult blocks from consecutive
	// role:"tool" messages so they can be flushed as a single user message.
	var pendingToolResults []map[string]any
	flushPendingToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		out = append(out, map[string]any{
			"role":    "user",
			"content": pendingToolResults,
		})
		pendingToolResults = nil
	}

	for _, raw := range msgs {
		var msg struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			Name       string            `json:"name,omitempty"`
			ToolCalls  []json.RawMessage `json:"tool_calls,omitempty"`
			ToolCallID string            `json:"tool_call_id,omitempty"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Role {
		case "system", "developer":
			// "developer" is the GPT-5-era replacement for system; treat the same.
			flushPendingToolResults()
			if s := chatContentToText(msg.Content); s != "" {
				systemParts = append(systemParts, s)
			}

		case "user":
			flushPendingToolResults()
			blocks := translateChatUserContent(msg.Content)
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "user", "content": blocks})
			}

		case "assistant":
			flushPendingToolResults()
			blocks := translateChatAssistantContent(msg.Content, msg.ToolCalls)
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": blocks})
			}

		case "tool":
			// Append to the pending batch; flushed when the next non-tool
			// message arrives or at the end.
			pendingToolResults = append(pendingToolResults, map[string]any{
				"toolResult": map[string]any{
					"toolUseId": msg.ToolCallID,
					"content":   chatToolResultContent(msg.Content),
				},
			})

		default:
			slog.Debug("skipping unsupported chat message role", "role", msg.Role)
		}
	}
	flushPendingToolResults()

	return strings.Join(systemParts, "\n"), out, nil
}

// chatContentToText extracts plain text from an OAI message content field,
// which can be either a string or an array of typed parts. Non-text parts
// (images, etc.) are ignored — used only for system messages.
func chatContentToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		var t string
		json.Unmarshal(p["type"], &t)
		if t == "text" || t == "input_text" {
			var text string
			if json.Unmarshal(p["text"], &text) == nil {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}

// translateChatUserContent converts an OAI user message content (string or
// array of parts) into Converse content blocks.
func translateChatUserContent(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []map[string]any{{"text": s}}
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []map[string]any
	for _, p := range parts {
		var t string
		json.Unmarshal(p["type"], &t)
		switch t {
		case "text", "input_text":
			var text string
			if json.Unmarshal(p["text"], &text) == nil && text != "" {
				out = append(out, map[string]any{"text": text})
			}
		case "image_url":
			if img := translateChatImageURLToConverse(p); img != nil {
				out = append(out, img)
			}
		case "input_image":
			// Newer OAI shape (Responses-style) sometimes mirrored in Chat;
			// support it for forward compat.
			if img := translateChatImageURLToConverse(p); img != nil {
				out = append(out, img)
			}
		default:
			slog.Debug("skipping unsupported chat user content part", "type", t)
		}
	}
	return out
}

// translateChatAssistantContent converts an OAI assistant message (which has
// both `content` and optional `tool_calls`) into a flat list of Converse
// content blocks: text first, then toolUse blocks for each tool call.
func translateChatAssistantContent(content json.RawMessage, toolCalls []json.RawMessage) []map[string]any {
	var out []map[string]any
	if text := chatContentToText(content); text != "" {
		out = append(out, map[string]any{"text": text})
	}
	for _, raw := range toolCalls {
		var tc struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &tc) != nil {
			continue
		}
		// arguments comes through as a JSON-encoded string; Converse wants
		// a real object. If it doesn't parse, fall back to an empty object.
		var input json.RawMessage = json.RawMessage("{}")
		if tc.Function.Arguments != "" {
			var probe any
			if json.Unmarshal([]byte(tc.Function.Arguments), &probe) == nil {
				input = json.RawMessage(tc.Function.Arguments)
			}
		}
		out = append(out, map[string]any{
			"toolUse": map[string]any{
				"toolUseId": tc.ID,
				"name":      tc.Function.Name,
				"input":     input,
			},
		})
	}
	return out
}

// chatToolResultContent normalizes an OAI tool message content (almost always
// a plain string) into the array-of-blocks form Converse requires.
func chatToolResultContent(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return []map[string]any{{"text": ""}}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"text": s}}
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return []map[string]any{{"text": string(raw)}}
	}
	var out []map[string]any
	for _, p := range parts {
		var t, text string
		json.Unmarshal(p["type"], &t)
		if t == "text" || t == "input_text" {
			json.Unmarshal(p["text"], &text)
			out = append(out, map[string]any{"text": text})
		}
	}
	if len(out) == 0 {
		return []map[string]any{{"text": ""}}
	}
	return out
}

// translateChatImageURLToConverse parses an OAI image_url part and returns a
// Converse image block. Only data: URLs are supported; remote URLs are
// dropped because Bedrock does not fetch external images.
func translateChatImageURLToConverse(part map[string]json.RawMessage) map[string]any {
	var iu struct {
		URL string `json:"url"`
	}
	// image_url field may be the URL string directly or a nested object.
	if raw, ok := part["image_url"]; ok {
		if json.Unmarshal(raw, &iu.URL) != nil {
			json.Unmarshal(raw, &iu)
		}
	}
	if iu.URL == "" {
		return nil
	}
	if !strings.HasPrefix(iu.URL, "data:") {
		slog.Warn("dropping non-data image URL for bedrock (no remote fetch)", "url_prefix", safePrefix(iu.URL, 40))
		return nil
	}
	mediaType, data, ok := parseDataURL(iu.URL)
	if !ok {
		return nil
	}
	format := mediaTypeToConverseFormat(mediaType)
	if format == "" {
		slog.Warn("unsupported image media type for converse", "media_type", mediaType)
		return nil
	}
	return map[string]any{
		"image": map[string]any{
			"format": format,
			"source": map[string]any{"bytes": data},
		},
	}
}

// parseDataURL splits a "data:<media-type>;base64,<data>" URL into its parts.
// Returns ("", "", false) if the URL is malformed or not base64-encoded.
// We re-encode the bytes to canonical base64 to normalize any whitespace
// the client may have included.
func parseDataURL(s string) (mediaType, base64Data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", "", false
	}
	rest := s[len(prefix):]
	commaIdx := strings.IndexByte(rest, ',')
	if commaIdx < 0 {
		return "", "", false
	}
	header, payload := rest[:commaIdx], rest[commaIdx+1:]
	if !strings.Contains(header, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(header, ";base64")
	// Validate by decoding then re-encoding canonically.
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Try URL-safe just in case.
		raw, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return "", "", false
		}
	}
	return mediaType, base64.StdEncoding.EncodeToString(raw), true
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// translateChatToolsToConverse converts OAI tool definitions (function-only)
// to Converse toolConfig.
func translateChatToolsToConverse(tools []json.RawMessage, toolChoice json.RawMessage) map[string]any {
	var converseTools []map[string]any
	for _, raw := range tools {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		if tool.Type != "" && tool.Type != "function" {
			// OAI only specs "function"; skip future/unknown types.
			continue
		}
		spec := map[string]any{"name": tool.Function.Name}
		if tool.Function.Description != "" {
			spec["description"] = tool.Function.Description
		}
		if len(tool.Function.Parameters) > 0 {
			spec["inputSchema"] = map[string]any{"json": json.RawMessage(tool.Function.Parameters)}
		}
		converseTools = append(converseTools, map[string]any{"toolSpec": spec})
	}
	if len(converseTools) == 0 {
		return nil
	}
	cfg := map[string]any{"tools": converseTools}
	if tc := translateChatToolChoiceToConverse(toolChoice); tc != nil {
		cfg["toolChoice"] = tc
	}
	return cfg
}

// translateChatToolChoiceToConverse maps OAI tool_choice (string or object)
// to Converse format. "none" returns nil (caller should drop tools entirely).
func translateChatToolChoiceToConverse(tc json.RawMessage) map[string]any {
	if len(tc) == 0 || string(tc) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(tc, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"auto": map[string]any{}}
		case "required":
			return map[string]any{"any": map[string]any{}}
		case "none":
			// Converse has no equivalent; the caller can suppress tools
			// instead. For now return nil so we just don't set a toolChoice.
			return nil
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(tc, &obj) != nil {
		return nil
	}
	if obj.Type == "function" && obj.Function.Name != "" {
		return map[string]any{"tool": map[string]any{"name": obj.Function.Name}}
	}
	return nil
}

// --- Bedrock Converse -> OAI Chat Completions response ---

// buildChatResponseFromConverse converts a non-streaming Converse response
// into an OAI Chat Completions response using the same field shapes the
// rest of the proxy already emits.
func buildChatResponseFromConverse(body []byte, modelName string) (*api.ChatResponse, *converseUsage, error) {
	var resp converseResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("decoding converse response: %w", err)
	}

	var textParts []string
	var toolCalls []api.ChatChoiceToolCall
	var reasoning string

	for _, b := range resp.Output.Message.Content {
		switch {
		case len(b["text"]) > 0:
			var t string
			json.Unmarshal(b["text"], &t)
			textParts = append(textParts, t)

		case len(b["toolUse"]) > 0:
			var tu struct {
				ToolUseID string          `json:"toolUseId"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
			}
			if json.Unmarshal(b["toolUse"], &tu) != nil {
				continue
			}
			args := "{}"
			if len(tu.Input) > 0 {
				args = string(tu.Input)
			}
			tc := api.ChatChoiceToolCall{ID: tu.ToolUseID, Type: "function"}
			tc.Function.Name = tu.Name
			tc.Function.Arguments = args
			toolCalls = append(toolCalls, tc)

		case len(b["reasoningContent"]) > 0:
			var rc struct {
				ReasoningText struct {
					Text string `json:"text"`
				} `json:"reasoningText"`
			}
			if json.Unmarshal(b["reasoningContent"], &rc) == nil && rc.ReasoningText.Text != "" {
				if reasoning == "" {
					reasoning = rc.ReasoningText.Text
				} else {
					reasoning = reasoning + rc.ReasoningText.Text
				}
			}
		}
	}

	contentText := strings.Join(textParts, "")
	msg := api.ChatChoiceMsg{
		Role:      "assistant",
		Content:   &contentText,
		ToolCalls: toolCalls,
	}
	if reasoning != "" {
		msg.Reasoning = &reasoning
	}

	out := &api.ChatResponse{
		ID:      api.RandomID("chatcmpl-"),
		Model:   modelName,
		Created: 0,
		Choices: []api.ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: mapConverseStopReasonToChat(resp.StopReason),
		}},
		Usage: &api.ChunkUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	return out, &converseUsage{Input: resp.Usage.InputTokens, Output: resp.Usage.OutputTokens}, nil
}

// mapConverseStopReasonToChat maps Bedrock Converse stopReason values to OAI
// finish_reason. Inverse of mapFinishToStopReason in messages_translate.go.
func mapConverseStopReasonToChat(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "guardrail_intervened", "content_filtered":
		return "content_filter"
	default:
		return "stop"
	}
}
