package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

// --- Request: Anthropic Messages -> Bedrock Converse ---
//
// Bedrock Converse is structurally close to Anthropic Messages with renamed
// fields and a more rigid schema (e.g. content is always an array, tool
// results require a content-block list, image sources use base64 bytes under
// a "format" tag). This translator does the field-by-field mapping; it does
// not introspect or rewrite the underlying conversation semantics.
//
// Reference: https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html

// buildConverseRequestFromAnthropic translates an Anthropic Messages request
// into a Converse request body. The model ID does NOT appear in the body —
// it goes in the URL path — so this returns just the JSON-marshalable map.
func buildConverseRequestFromAnthropic(req messagesRequest) (map[string]any, error) {
	out := map[string]any{}

	// System: preserve per-block cache_control breakpoints by building a
	// Converse content array (text + cachePoint siblings) rather than
	// flattening to a single string.
	if sys := translateAnthropicSystemToConverse(req.System); len(sys) > 0 {
		out["system"] = sys
	}

	messages, err := translateAnthropicMessagesToConverse(req.Messages)
	if err != nil {
		return nil, err
	}
	out["messages"] = messages

	inference := map[string]any{}
	if req.MaxTokens > 0 {
		inference["maxTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		inference["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		inference["topP"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		inference["stopSequences"] = req.StopSequences
	}
	if len(inference) > 0 {
		out["inferenceConfig"] = inference
	}

	if len(req.Tools) > 0 {
		toolCfg := translateAnthropicToolsToConverse(req.Tools, req.ToolChoice)
		if toolCfg != nil {
			out["toolConfig"] = toolCfg
		}
	}

	// Anthropic-specific knobs (thinking, top_k) ride along in
	// additionalModelRequestFields, which Bedrock forwards to the underlying
	// model. Only meaningful when the Bedrock model is an Anthropic Claude.
	addl := map[string]any{}
	if req.TopK != nil {
		addl["top_k"] = *req.TopK
	}
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		addl["thinking"] = json.RawMessage(req.Thinking)
	}
	if len(addl) > 0 {
		out["additionalModelRequestFields"] = addl
	}

	return out, nil
}

func translateAnthropicMessagesToConverse(msgs []json.RawMessage) ([]map[string]any, error) {
	var result []map[string]any
	for _, raw := range msgs {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		blocks := translateAnthropicContentToConverse(msg.Content, msg.Role)
		if len(blocks) == 0 {
			continue
		}
		result = append(result, map[string]any{
			"role":    msg.Role,
			"content": blocks,
		})
	}
	return result, nil
}

// translateAnthropicContentToConverse converts Anthropic content (string or
// array of typed blocks) into the array of Converse content blocks.
func translateAnthropicContentToConverse(content json.RawMessage, role string) []map[string]any {
	// String content → single text block.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []map[string]any{{"text": s}}
	}

	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}

	var out []map[string]any
	for _, b := range blocks {
		var blockType string
		json.Unmarshal(b["type"], &blockType)

		var emitted map[string]any
		switch blockType {
		case "text":
			var text string
			json.Unmarshal(b["text"], &text)
			if text != "" {
				emitted = map[string]any{"text": text}
			}

		case "image":
			emitted = translateImageBlockToConverse(b)

		case "document":
			emitted = translateDocumentBlockToConverse(b)

		case "tool_use":
			var id, name string
			json.Unmarshal(b["id"], &id)
			json.Unmarshal(b["name"], &name)
			input := json.RawMessage(b["input"])
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			emitted = map[string]any{
				"toolUse": map[string]any{
					"toolUseId": id,
					"name":      name,
					"input":     input,
				},
			}

		case "tool_result":
			var id string
			var isError bool
			json.Unmarshal(b["tool_use_id"], &id)
			json.Unmarshal(b["is_error"], &isError)
			toolResultContent := translateToolResultContentToConverse(b["content"])
			status := "success"
			if isError {
				status = "error"
			}
			emitted = map[string]any{"toolResult": map[string]any{
				"toolUseId": id,
				"content":   toolResultContent,
				"status":    status,
			}}

		case "thinking", "redacted_thinking":
			// Drop client-supplied thinking from prior turns; Bedrock will
			// re-emit reasoning if the model produces it.
			slog.Debug("stripping thinking block before converse send", "role", role)

		default:
			slog.Debug("skipping unsupported anthropic block type for converse",
				"type", blockType, "role", role)
		}

		if emitted == nil {
			continue
		}
		out = append(out, emitted)
		if hasAnthropicCachePoint(b) {
			out = append(out, converseCachePoint())
		}
	}
	return out
}

// hasAnthropicCachePoint reports whether the Anthropic block carries a
// cache_control directive. Bedrock Converse represents cache breakpoints
// as a sibling {"cachePoint": ...} block appended AFTER the block being
// cached, so callers emit the content block first, then this cachePoint.
func hasAnthropicCachePoint(block map[string]json.RawMessage) bool {
	cc, ok := block["cache_control"]
	if !ok {
		return false
	}
	s := strings.TrimSpace(string(cc))
	return s != "" && s != "null"
}

// converseCachePoint returns the Bedrock cachePoint sibling block. Bedrock
// supports a single cache type today ("default"); the type field is always
// emitted because Converse requires it.
func converseCachePoint() map[string]any {
	return map[string]any{"cachePoint": map[string]any{"type": "default"}}
}

// translateAnthropicSystemToConverse builds a Converse system content array
// from the Anthropic system field. The Converse shape is identical to a
// message content array ([{text}, {cachePoint}, {text}, ...]), so cache
// breakpoints on system blocks are preserved here — unlike the Chat
// Completions path which flattens system to a single string.
func translateAnthropicSystemToConverse(system json.RawMessage) []map[string]any {
	if len(system) == 0 || string(system) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(system, &s) == nil {
		if s == "" {
			return nil
		}
		return []map[string]any{{"text": s}}
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(system, &blocks) != nil {
		return nil
	}
	var out []map[string]any
	for _, b := range blocks {
		var blockType string
		json.Unmarshal(b["type"], &blockType)
		if blockType != "" && blockType != "text" {
			continue
		}
		var text string
		json.Unmarshal(b["text"], &text)
		if text == "" {
			continue
		}
		out = append(out, map[string]any{"text": text})
		if hasAnthropicCachePoint(b) {
			out = append(out, converseCachePoint())
		}
	}
	return out
}

// translateImageBlockToConverse converts an Anthropic image block to
// Converse format. Converse expects { image: { format, source: { bytes }}}
// where format is "png"|"jpeg"|"gif"|"webp" and bytes is base64-encoded.
func translateImageBlockToConverse(block map[string]json.RawMessage) map[string]any {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if json.Unmarshal(block["source"], &source) != nil {
		return nil
	}
	if source.Type != "base64" {
		// URL sources (which Anthropic permits) are not supported by Converse.
		// The pipeline normally rewrites these, but log if one slips through.
		slog.Warn("skipping non-base64 image for converse", "source_type", source.Type)
		return nil
	}
	format := mediaTypeToConverseFormat(source.MediaType)
	if format == "" {
		slog.Warn("unsupported image media type for converse", "media_type", source.MediaType)
		return nil
	}
	return map[string]any{
		"image": map[string]any{
			"format": format,
			"source": map[string]any{"bytes": source.Data},
		},
	}
}

func mediaTypeToConverseFormat(mt string) string {
	switch strings.ToLower(mt) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return ""
}

// translateDocumentBlockToConverse converts an Anthropic document block to
// Converse format. Converse requires document.name (a unique identifier the
// model can reference); we synthesize one if absent.
func translateDocumentBlockToConverse(block map[string]json.RawMessage) map[string]any {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if json.Unmarshal(block["source"], &source) != nil {
		return nil
	}
	if source.Type != "base64" || source.MediaType != "application/pdf" {
		slog.Warn("unsupported document for converse",
			"source_type", source.Type, "media_type", source.MediaType)
		return nil
	}
	var name string
	json.Unmarshal(block["title"], &name)
	if name == "" {
		json.Unmarshal(block["name"], &name)
	}
	if name == "" {
		name = api.RandomID("doc-")
	}
	return map[string]any{
		"document": map[string]any{
			"name":   name,
			"format": "pdf",
			"source": map[string]any{"bytes": source.Data},
		},
	}
}

// translateToolResultContentToConverse normalizes Anthropic tool_result
// content (string | array-of-blocks) into Converse's required array form:
// [{text: "..."} | {json: ...} | {image: ...}].
func translateToolResultContentToConverse(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return []map[string]any{{"text": ""}}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"text": s}}
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		// Last-ditch: stringify whatever we got.
		return []map[string]any{{"text": string(raw)}}
	}
	var out []map[string]any
	for _, b := range blocks {
		var blockType string
		json.Unmarshal(b["type"], &blockType)
		switch blockType {
		case "text":
			var text string
			json.Unmarshal(b["text"], &text)
			out = append(out, map[string]any{"text": text})
		case "image":
			if img := translateImageBlockToConverse(b); img != nil {
				out = append(out, img)
			}
		}
	}
	if len(out) == 0 {
		return []map[string]any{{"text": ""}}
	}
	return out
}

// translateAnthropicToolsToConverse converts Anthropic tools + tool_choice to
// Converse toolConfig. Server tools (web_search etc.) are dropped — those
// are handled by the proxy pipeline, not the backend.
func translateAnthropicToolsToConverse(tools []json.RawMessage, toolChoice json.RawMessage) map[string]any {
	var converseTools []map[string]any
	// Anthropic allows cache_control on individual tools; a cachePoint in
	// Converse's tools array caches every tool up to (and including) the
	// preceding toolSpec. We emit the cachePoint immediately after the
	// annotated tool, matching the Anthropic semantics.
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
		if tool.Type != "" && tool.Type != "custom" {
			continue
		}
		spec := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			spec["description"] = tool.Description
		}
		if len(tool.InputSchema) > 0 {
			spec["inputSchema"] = map[string]any{"json": json.RawMessage(tool.InputSchema)}
		}
		converseTools = append(converseTools, map[string]any{"toolSpec": spec})

		// Re-decode to detect cache_control without mutating the core decode
		// above (keeps the happy path clean).
		var cacheProbe struct {
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if json.Unmarshal(raw, &cacheProbe) == nil {
			s := strings.TrimSpace(string(cacheProbe.CacheControl))
			if s != "" && s != "null" {
				converseTools = append(converseTools, converseCachePoint())
			}
		}
	}
	if len(converseTools) == 0 {
		return nil
	}
	cfg := map[string]any{"tools": converseTools}
	if tc := translateAnthropicToolChoiceToConverse(toolChoice); tc != nil {
		cfg["toolChoice"] = tc
	}
	return cfg
}

func translateAnthropicToolChoiceToConverse(tc json.RawMessage) map[string]any {
	if len(tc) == 0 || string(tc) == "null" {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(tc, &choice) != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return map[string]any{"auto": map[string]any{}}
	case "any":
		return map[string]any{"any": map[string]any{}}
	case "tool":
		if choice.Name == "" {
			return nil
		}
		return map[string]any{"tool": map[string]any{"name": choice.Name}}
	}
	return nil
}

// applyConverseSamplingDefaults fills in model.Defaults values into the
// Converse request's inferenceConfig (and additionalModelRequestFields for
// top_k) for any field the client did not specify. The shape mirrors
// ApplySamplingDefaults but writes Converse-spec field names.
func applyConverseSamplingDefaults(req map[string]any, model *config.ModelConfig) {
	if model.Defaults == nil {
		return
	}
	d := model.Defaults

	inf, _ := req["inferenceConfig"].(map[string]any)
	if inf == nil {
		inf = map[string]any{}
	}
	if d.Temperature != nil {
		if _, ok := inf["temperature"]; !ok {
			inf["temperature"] = *d.Temperature
		}
	}
	if d.TopP != nil {
		if _, ok := inf["topP"]; !ok {
			inf["topP"] = *d.TopP
		}
	}
	if d.MaxNewTokens != nil {
		if _, ok := inf["maxTokens"]; !ok {
			inf["maxTokens"] = *d.MaxNewTokens
		}
	}
	if len(d.Stop) > 0 {
		if _, ok := inf["stopSequences"]; !ok {
			inf["stopSequences"] = d.Stop
		}
	}
	if len(inf) > 0 {
		req["inferenceConfig"] = inf
	}

	// top_k has no Converse inferenceConfig slot; ride along in
	// additionalModelRequestFields where Anthropic-on-Bedrock will pick it up.
	if d.TopK != nil {
		addl, _ := req["additionalModelRequestFields"].(map[string]any)
		if addl == nil {
			addl = map[string]any{}
		}
		if _, ok := addl["top_k"]; !ok {
			addl["top_k"] = *d.TopK
			req["additionalModelRequestFields"] = addl
		}
	}
}

// applyGuardrails attaches a guardrailConfig block to the Converse request
// when the model is configured with a guardrail. A missing GuardrailID is a
// no-op so this can be called unconditionally from the dispatch handlers.
//
// Per-request overrides are intentionally not supported: guardrails are an
// operator-level safety control, and exposing them to callers would let a
// caller weaken the policy for their own requests.
func applyGuardrails(req map[string]any, model *config.ModelConfig) {
	if model == nil || model.GuardrailID == "" {
		return
	}
	cfg := map[string]any{"guardrailIdentifier": model.GuardrailID}
	if model.GuardrailVersion != "" {
		cfg["guardrailVersion"] = model.GuardrailVersion
	}
	if model.GuardrailTrace != "" {
		cfg["trace"] = model.GuardrailTrace
	}
	req["guardrailConfig"] = cfg
}

// --- Response: Bedrock Converse -> Anthropic Messages ---

// converseResponse is a minimal decoder for the non-streaming Converse
// response. We don't fully model the Bedrock schema — only the fields the
// translator needs.
type converseResponse struct {
	Output struct {
		Message struct {
			Role    string                       `json:"role"`
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens           int `json:"inputTokens"`
		OutputTokens          int `json:"outputTokens"`
		TotalTokens           int `json:"totalTokens"`
		CacheReadInputTokens  int `json:"cacheReadInputTokens"`
		CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	} `json:"usage"`
	Metrics struct {
		LatencyMs int64 `json:"latencyMs"`
	} `json:"metrics"`
}

// buildAnthropicResponseFromConverse converts a non-streaming Converse
// response into the same response shape MessagesHandler.handleNonStreaming
// emits today (an Anthropic Messages response document).
//
// modelName is the friendly name registered in the proxy (what the client
// asked for), not the Bedrock model ID.
func buildAnthropicResponseFromConverse(body []byte, modelName string) (map[string]any, *converseUsage, error) {
	var resp converseResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("decoding converse response: %w", err)
	}

	content := translateConverseContentToAnthropic(resp.Output.Message.Content)
	if content == nil {
		content = []any{}
	}

	usage := map[string]any{
		"input_tokens":                resp.Usage.InputTokens,
		"output_tokens":               resp.Usage.OutputTokens,
		"cache_creation_input_tokens": resp.Usage.CacheWriteInputTokens,
		"cache_read_input_tokens":     resp.Usage.CacheReadInputTokens,
	}

	out := map[string]any{
		"id":            api.RandomID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         modelName,
		"stop_reason":   mapConverseStopReason(resp.StopReason),
		"stop_sequence": nil,
		"usage":         usage,
	}
	return out, &converseUsage{
		Input:           resp.Usage.InputTokens,
		Output:          resp.Usage.OutputTokens,
		CacheReadInput:  resp.Usage.CacheReadInputTokens,
		CacheWriteInput: resp.Usage.CacheWriteInputTokens,
	}, nil
}

// converseUsage carries per-request token counts extracted from either a
// non-streaming Converse response or the streaming metadata event. Cache
// fields are zero when Bedrock did not report them (most models today);
// they appear on responses from cache-capable models (Claude family).
type converseUsage struct {
	Input           int
	Output          int
	CacheReadInput  int
	CacheWriteInput int
}

func translateConverseContentToAnthropic(blocks []map[string]json.RawMessage) []any {
	var out []any
	for _, b := range blocks {
		switch {
		case len(b["text"]) > 0:
			var text string
			json.Unmarshal(b["text"], &text)
			out = append(out, map[string]any{"type": "text", "text": text})

		case len(b["toolUse"]) > 0:
			var tu struct {
				ToolUseID string          `json:"toolUseId"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
			}
			if json.Unmarshal(b["toolUse"], &tu) != nil {
				continue
			}
			input := tu.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, map[string]any{
				"type":  "tool_use",
				"id":    tu.ToolUseID,
				"name":  tu.Name,
				"input": input,
			})

		case len(b["reasoningContent"]) > 0:
			var rc struct {
				ReasoningText struct {
					Text      string `json:"text"`
					Signature string `json:"signature"`
				} `json:"reasoningText"`
			}
			if json.Unmarshal(b["reasoningContent"], &rc) != nil {
				continue
			}
			if rc.ReasoningText.Text == "" {
				continue
			}
			// Insert thinking block before any text/tool_use blocks already
			// emitted, matching Anthropic's convention. The signature field
			// is optional on the Anthropic wire format: if Bedrock didn't
			// provide one, we omit it rather than fabricate a value that
			// would fail any downstream signature validation.
			thinking := map[string]any{
				"type":     "thinking",
				"thinking": rc.ReasoningText.Text,
			}
			if rc.ReasoningText.Signature != "" {
				thinking["signature"] = rc.ReasoningText.Signature
			}
			out = append([]any{thinking}, out...)
		}
	}
	return out
}

// mapConverseStopReason maps Bedrock Converse stopReason strings to Anthropic
// stop_reason values. Most are identical; "guardrail_intervened" and
// "content_filtered" map to "end_turn" since Anthropic has no equivalent.
func mapConverseStopReason(r string) string {
	switch r {
	case "end_turn", "tool_use", "max_tokens", "stop_sequence":
		return r
	case "guardrail_intervened", "content_filtered":
		return "end_turn"
	default:
		return "end_turn"
	}
}
