package handler

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/pipeline"
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
	OutputConfig  json.RawMessage   `json:"output_config,omitempty"`
	OutputFormat  json.RawMessage   `json:"output_format,omitempty"`
	aliasFrom     string
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

		// Log raw message structure for debugging.
		contentSnippet := string(msg.Content)
		if len(contentSnippet) > 200 {
			contentSnippet = contentSnippet[:200] + "..."
		}
		slog.Debug("raw anthropic message", "role", msg.Role, "content_prefix", contentSnippet)

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
// Chat Completions messages.
//
// tool_result blocks become role:tool messages and are emitted FIRST: Chat
// Completions requires tool messages to directly follow the assistant message
// that issued the calls, so any user text in the same Anthropic turn must come
// after them or strict templates reject the conversation. Images and documents
// inside a tool_result cannot ride in a tool message, so they are lifted into
// the user message that follows (labelled with the originating tool_use id).
// Handles text, image, document (PDF), tool_result, and thinking blocks.
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
	var toolMedia []any
	var toolResults []map[string]any

	for _, block := range blocks {
		var blockType string
		json.Unmarshal(block["type"], &blockType)

		slog.Debug("translating user content block", "type", blockType)

		switch blockType {
		case "text":
			var text string
			json.Unmarshal(block["text"], &text)
			userParts = append(userParts, map[string]any{"type": "text", "text": text})

		case "image":
			userParts = append(userParts, translateImageBlock(block))

		case "document":
			slog.Debug("translating document block", "keys", blockKeys(block))
			userParts = append(userParts, translateDocumentParts(block)...)

		case "tool_result":
			var toolUseID string
			json.Unmarshal(block["tool_use_id"], &toolUseID)
			text, media := splitToolResultContent(block["content"])
			var isError bool
			json.Unmarshal(block["is_error"], &isError)
			if len(media) > 0 {
				if strings.TrimSpace(text) == "" {
					text = "[the tool returned attachments; they follow in the next message]"
				}
				toolMedia = append(toolMedia, map[string]any{
					"type": "text",
					"text": "[attachments from tool result " + toolUseID + "]",
				})
				toolMedia = append(toolMedia, media...)
			}
			if isError {
				text = "[tool error] " + text
			}
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      text,
			})

		case "web_search_tool_result":
			// Search result blocks from prior proxy-injected searches. Convert to
			// text context so the backend sees what was found.
			var results []struct {
				Type  string `json:"type"`
				Title string `json:"title"`
				URL   string `json:"url"`
			}
			if raw, ok := block["content"]; ok {
				json.Unmarshal(raw, &results)
			}
			if len(results) > 0 {
				var sb strings.Builder
				sb.WriteString("[Web search results:")
				for _, r := range results {
					if r.Title != "" || r.URL != "" {
						sb.WriteString("\n- " + r.Title + " (" + r.URL + ")")
					}
				}
				sb.WriteString("]")
				userParts = append(userParts, map[string]any{"type": "text", "text": sb.String()})
			}

		case "thinking", "redacted_thinking":
			slog.Debug("stripping thinking block from user message", "type", blockType)
			continue

		default:
			slog.Warn("skipping unknown user content block type", "type", blockType,
				"keys", blockKeys(block))
		}
	}

	// Tool messages first, then the user turn (tool attachments, then the
	// user's own parts).
	result := append([]map[string]any(nil), toolResults...)

	parts := append(toolMedia, userParts...)
	if len(parts) > 0 {
		// If only one text part, simplify to string content.
		if len(parts) == 1 {
			if tp, ok := parts[0].(map[string]any); ok && tp["type"] == "text" {
				result = append(result, map[string]any{"role": "user", "content": tp["text"]})
				return result
			}
		}
		result = append(result, map[string]any{"role": "user", "content": parts})
	}

	return result
}

// translateAssistantMessage converts an Anthropic assistant message to a
// Chat Completions message. text blocks become content, tool_use blocks
// become tool_calls, and thinking blocks are replayed as reasoning_content
// (and reasoning) so reasoning-capable templates can keep the model's prior
// chain of thought in context. redacted_thinking carries no readable text and
// is dropped.
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
	var reasoningParts []string
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
			input := block["input"]
			if len(input) == 0 || string(input) == "null" {
				input = json.RawMessage("{}")
			}
			argsBytes, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsBytes),
				},
			})

		case "server_tool_use":
			// Server tool calls (web_search_20250305 etc.) emitted by the proxy
			// during search. Convert to text so the backend sees the search context.
			var name string
			json.Unmarshal(block["name"], &name)
			var inputRaw json.RawMessage
			if raw, ok := block["input"]; ok {
				inputRaw = raw
			}
			var query string
			if inputRaw != nil {
				var inp struct {
					Query string `json:"query"`
				}
				json.Unmarshal(inputRaw, &inp)
				query = inp.Query
			}
			if query != "" {
				textParts = append(textParts, "[Searched the web: "+query+"]")
			}

		case "thinking":
			var thinking string
			json.Unmarshal(block["thinking"], &thinking)
			if thinking != "" {
				reasoningParts = append(reasoningParts, thinking)
			}

		case "redacted_thinking":
			slog.Debug("dropping redacted_thinking block from assistant message")

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

	if len(reasoningParts) > 0 {
		reasoning := strings.Join(reasoningParts, "\n")
		msg["reasoning_content"] = reasoning
		msg["reasoning"] = reasoning
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

// blockKeys returns the JSON keys present in a content block (for debug logging).
func blockKeys(block map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(block))
	for k := range block {
		keys = append(keys, k)
	}
	return keys
}

// translateDocumentParts converts an Anthropic document content block into
// Chat Completions content parts.
//
//   - base64 PDF: a pipeline-internal pdf_data part (processPDFs extracts text
//     or falls back to OCR/vision, and no part of it reaches the backend).
//   - text source: the text itself, headed by the title/context when present.
//   - content source: the inner text/image blocks, inlined.
//   - url source: a pipeline-internal document_url part; the pipeline fetches
//     it through the SSRF-safe client (size capped) and processes the result.
//
// citations and cache_control are ignored.
func translateDocumentParts(block map[string]json.RawMessage) []any {
	var source struct {
		Type      string          `json:"type"`
		MediaType string          `json:"media_type"`
		Data      string          `json:"data"`
		URL       string          `json:"url"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(block["source"], &source); err != nil {
		slog.Warn("document block: failed to parse source", "error", err,
			"raw_source", string(block["source"]))
		return []any{map[string]any{"type": "text", "text": "[document: could not parse source]"}}
	}

	slog.Debug("document block source", "source_type", source.Type,
		"media_type", source.MediaType, "data_len", len(source.Data))

	var title, docContext string
	json.Unmarshal(block["title"], &title)
	json.Unmarshal(block["context"], &docContext)
	if title == "" {
		// Some Claude Code versions use "name".
		json.Unmarshal(block["name"], &title)
	}
	header := documentHeader(title, docContext)

	switch source.Type {
	case "base64":
		if source.MediaType == "application/pdf" || source.MediaType == "" {
			result := map[string]any{
				"type": "pdf_data",
				"data": source.Data,
			}
			if title != "" {
				result["filename"] = title
			}
			return []any{result}
		}
		if strings.HasPrefix(source.MediaType, "text/") {
			if raw, err := base64.StdEncoding.DecodeString(source.Data); err == nil {
				return []any{map[string]any{"type": "text", "text": header + string(raw)}}
			}
		}

	case "text":
		return []any{map[string]any{"type": "text", "text": header + source.Data}}

	case "content":
		var inner []map[string]json.RawMessage
		var innerStr string
		if json.Unmarshal(source.Content, &innerStr) == nil {
			return []any{map[string]any{"type": "text", "text": header + innerStr}}
		}
		if json.Unmarshal(source.Content, &inner) == nil {
			parts := []any{}
			if header != "" {
				parts = append(parts, map[string]any{"type": "text", "text": strings.TrimRight(header, "\n")})
			}
			for _, ib := range inner {
				var t string
				json.Unmarshal(ib["type"], &t)
				switch t {
				case "text":
					var text string
					json.Unmarshal(ib["text"], &text)
					parts = append(parts, map[string]any{"type": "text", "text": text})
				case "image":
					parts = append(parts, translateImageBlock(ib))
				}
			}
			return parts
		}

	case "url":
		if source.URL != "" {
			result := map[string]any{"type": "document_url", "url": source.URL}
			if title != "" {
				result["filename"] = title
			}
			return []any{result}
		}
	}

	slog.Warn("document block: unsupported format",
		"source_type", source.Type, "media_type", source.MediaType)
	return []any{map[string]any{"type": "text", "text": "[unsupported document format: " + source.MediaType + "]"}}
}

// documentHeader renders the optional title/context of a document block as a
// text prefix (empty when neither is present).
func documentHeader(title, docContext string) string {
	var sb strings.Builder
	if title != "" {
		sb.WriteString("[Document: " + title + "]\n")
	}
	if docContext != "" {
		sb.WriteString(docContext + "\n")
	}
	if sb.Len() > 0 {
		sb.WriteString("\n")
	}
	return sb.String()
}

// translateDocumentBlock returns the first part of translateDocumentParts. It
// is kept for single-part callers (and tests); multi-part sources (content
// documents) are only fully represented by translateDocumentParts.
func translateDocumentBlock(block map[string]json.RawMessage) map[string]any {
	parts := translateDocumentParts(block)
	if len(parts) > 0 {
		if m, ok := parts[0].(map[string]any); ok {
			return m
		}
	}
	return map[string]any{"type": "text", "text": "[empty document]"}
}

// extractToolResultContent extracts the text of a tool_result content field,
// which can be a string or an array of content blocks.
func extractToolResultContent(raw json.RawMessage) string {
	text, _ := splitToolResultContent(raw)
	return text
}

// splitToolResultContent separates a tool_result content field into its text
// and any media parts (image_url / pdf_data / ...) that a role:tool message
// cannot carry.
func splitToolResultContent(raw json.RawMessage) (string, []any) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw), nil
	}
	var texts []string
	var media []any
	for _, b := range blocks {
		var t string
		json.Unmarshal(b["type"], &t)
		switch t {
		case "text":
			var text string
			json.Unmarshal(b["text"], &text)
			if text != "" {
				texts = append(texts, text)
			}
		case "image":
			media = append(media, translateImageBlock(b))
		case "document":
			media = append(media, translateDocumentParts(b)...)
		case "search_result":
			var title string
			json.Unmarshal(b["title"], &title)
			var inner []struct {
				Text string `json:"text"`
			}
			json.Unmarshal(b["content"], &inner)
			var sb strings.Builder
			if title != "" {
				sb.WriteString(title + "\n")
			}
			for _, c := range inner {
				sb.WriteString(c.Text + "\n")
			}
			if sb.Len() > 0 {
				texts = append(texts, strings.TrimRight(sb.String(), "\n"))
			}
		}
	}
	return strings.Join(texts, "\n"), media
}

// --- Tool translation ---

// translateAnthropicToolsToChat converts Anthropic tool definitions to Chat
// Completions format: {name, input_schema} -> {type: "function", function: {name, parameters}}.
// Server tools (web_search_20250305 etc.) are stripped and returned in the second value.
func translateAnthropicToolsToChat(tools []json.RawMessage) ([]map[string]any, []string) {
	var result []map[string]any
	var strippedServerTools []string
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
			strippedServerTools = append(strippedServerTools, tool.Type)
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
	return result, strippedServerTools
}

// translateAnthropicToolChoice converts Anthropic tool_choice to OpenAI format.
func translateAnthropicToolChoice(tc json.RawMessage, hasTools bool) json.RawMessage {
	if len(tc) == 0 || string(tc) == "null" || !hasTools {
		return nil
	}
	var choice struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
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

	// System prompt -> system message.
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
		tools, strippedServerTools := translateAnthropicToolsToChat(req.Tools)
		if len(tools) > 0 {
			chatReq["tools"] = tools
			if tc := translateAnthropicToolChoice(req.ToolChoice, true); tc != nil {
				chatReq["tool_choice"] = json.RawMessage(tc)
			}
			if toolChoiceDisablesParallel(req.ToolChoice) {
				chatReq["parallel_tool_calls"] = false
			}
		}
		if len(strippedServerTools) > 0 {
			chatReq[pipeline.InternalKeyStrippedTools] = strippedServerTools
		}
	}

	// max_tokens -> max_completion_tokens (required in Anthropic, map to OpenAI).
	if req.MaxTokens > 0 {
		chatReq["max_completion_tokens"] = req.MaxTokens
	}

	// stop_sequences -> stop.
	if len(req.StopSequences) > 0 {
		chatReq["stop"] = req.StopSequences
	}

	// Temperature (Anthropic max 1.0, OpenAI max 2.0 -- pass through).
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

	// thinking: Chat Completions has no equivalent. thinking.type "disabled"
	// is honored per model via thinking_off (applyThinkingOff); an enabled
	// budget_tokens has no portable mapping and is dropped. output_config.effort
	// becomes reasoning_effort, except when thinking is disabled.
	thinking := parseThinking(req.Thinking)
	if thinking.Type != "" || thinking.BudgetTokens > 0 {
		slog.Debug("translated thinking config", "type", thinking.Type, "budget_tokens", thinking.BudgetTokens)
	}
	oc := parseOutputConfig(req.OutputConfig, req.OutputFormat)
	if oc.Effort != "" && thinking.Type != "disabled" {
		chatReq["reasoning_effort"] = oc.Effort
	}
	if rf := oc.responseFormat(); rf != nil {
		chatReq["response_format"] = rf
	}

	// metadata.user_id -> user.
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

// --- thinking / output_config ---

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

func parseThinking(raw json.RawMessage) thinkingConfig {
	var t thinkingConfig
	if len(raw) > 0 && string(raw) != "null" {
		json.Unmarshal(raw, &t)
	}
	return t
}

type outputConfig struct {
	Effort string
	Format json.RawMessage
}

// parseOutputConfig reads output_config.{effort,format} and the older
// top-level output_format. Efforts the Chat Completions world doesn't know
// ("max") are mapped to the nearest spelling ("xhigh").
func parseOutputConfig(rawConfig, rawFormat json.RawMessage) outputConfig {
	var oc outputConfig
	var cfg struct {
		Effort string          `json:"effort"`
		Format json.RawMessage `json:"format"`
	}
	if len(rawConfig) > 0 && string(rawConfig) != "null" {
		json.Unmarshal(rawConfig, &cfg)
	}
	oc.Effort = strings.ToLower(strings.TrimSpace(cfg.Effort))
	if oc.Effort == "max" {
		oc.Effort = "xhigh"
	}
	oc.Format = cfg.Format
	if (len(oc.Format) == 0 || string(oc.Format) == "null") && len(rawFormat) > 0 {
		oc.Format = rawFormat
	}
	return oc
}

// responseFormat converts an Anthropic json_schema output format into the
// Chat Completions response_format object.
func (oc outputConfig) responseFormat() map[string]any {
	if len(oc.Format) == 0 || string(oc.Format) == "null" {
		return nil
	}
	var f struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(oc.Format, &f) != nil || f.Type != "json_schema" || len(f.Schema) == 0 {
		return nil
	}
	name := f.Name
	if name == "" {
		name = "response"
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   name,
			"schema": f.Schema,
			"strict": true,
		},
	}
}

func toolChoiceDisablesParallel(tc json.RawMessage) bool {
	if len(tc) == 0 || string(tc) == "null" {
		return false
	}
	var c struct {
		Disable bool `json:"disable_parallel_tool_use"`
	}
	return json.Unmarshal(tc, &c) == nil && c.Disable
}

// anthropicUsage converts Chat Completions usage into Anthropic usage.
// Cached prompt tokens are reported as cache_read_input_tokens and removed from
// input_tokens, matching Anthropic's accounting (input_tokens excludes cache
// reads).
func anthropicUsage(u *api.ChunkUsage) map[string]any {
	if u == nil {
		return nil
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	input := u.PromptTokens - cached
	if input < 0 {
		input = 0
	}
	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               u.CompletionTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     cached,
	}
}

// resolveStopReason maps a Chat Completions finish to the Anthropic stop_reason
// and stop_sequence. A finish of "stop" whose backend stop_reason names one of
// the request's stop sequences is reported as "stop_sequence"; any turn that
// emitted tool calls ends with "tool_use" even if the backend said "stop".
func resolveStopReason(finish string, backendStop any, stopSequences []string, sawToolCalls bool) (string, any) {
	if sawToolCalls && (finish == "stop" || finish == "tool_calls" || finish == "") {
		return "tool_use", nil
	}
	if s, ok := backendStop.(string); ok && s != "" && finish == "stop" {
		for _, seq := range stopSequences {
			if seq == s {
				return "stop_sequence", s
			}
		}
	}
	return mapFinishToStopReason(finish), nil
}
