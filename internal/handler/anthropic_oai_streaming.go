package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
)

// streamAnthropicToChatSSE consumes Anthropic Messages SSE and emits OpenAI
// Chat Completions SSE. It deliberately ignores unknown Anthropic event types
// so additions to the upstream protocol remain forward-compatible.
func streamAnthropicToChatSSE(
	w io.Writer,
	body io.Reader,
	modelName string,
	includeUsage bool,
	flush func(),
) (responseBytes int64, usage *api.ChunkUsage) {
	if flush == nil {
		flush = func() {}
	}

	chunkID := api.RandomID("chatcmpl-")
	created := time.Now().Unix()
	usage = &api.ChunkUsage{}
	roleEmitted := false
	finishReason := "stop"
	finished := false
	streamFailed := false
	nextToolIndex := 0
	toolIndexes := make(map[int]int)

	emitChunk := func(choices []map[string]any, chunkUsage *api.ChunkUsage) {
		chunk := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": choices,
		}
		if chunkUsage != nil {
			chunk["usage"] = chunkUsage
		}
		data, _ := json.Marshal(chunk)
		n, _ := fmt.Fprintf(w, "data: %s\n\n", data)
		responseBytes += int64(n)
		flush()
	}

	emitRole := func() {
		if roleEmitted {
			return
		}
		emitChunk([]map[string]any{{
			"index": 0,
			"delta": map[string]any{"role": "assistant"},
		}}, nil)
		roleEmitted = true
	}

	emitError := func() {
		payload := map[string]any{
			"error": map[string]any{
				"message": "upstream stream error",
				"type":    "api_error",
			},
		}
		data, _ := json.Marshal(payload)
		n, _ := fmt.Fprintf(w, "data: %s\n\n", data)
		responseBytes += int64(n)
		flush()
		streamFailed = true
	}

	emitFinish := func() {
		if finished || streamFailed {
			return
		}
		emitRole()
		emitChunk([]map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}}, nil)
		if includeUsage {
			emitChunk([]map[string]any{}, usage)
		}
		finished = true
	}

	emitDone := func() {
		n, _ := fmt.Fprint(w, "data: [DONE]\n\n")
		responseBytes += int64(n)
		flush()
	}

	var upstreamBytes int64
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
streamLoop:
	for scanner.Scan() {
		line := scanner.Text()
		upstreamBytes += int64(len(line)) + 1
		if upstreamBytes > api.MaxResponseBodySize {
			slog.Error("anthropic stream exceeded response size limit", "model", modelName)
			emitError()
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				ID    string             `json:"id"`
				Model string             `json:"model"`
				Usage anthropicChatUsage `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type         string `json:"type"`
				Text         string `json:"text"`
				Thinking     string `json:"thinking"`
				PartialJSON  string `json:"partial_json"`
				StopReason   string `json:"stop_reason"`
				StopSequence string `json:"stop_sequence"`
			} `json:"delta"`
			Usage anthropicChatUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			slog.Debug("skipping malformed anthropic SSE event", "error", err)
			continue
		}

		switch event.Type {
		case "message_start":
			if strings.HasPrefix(event.Message.ID, "chatcmpl-") {
				chunkID = event.Message.ID
			}
			applyAnthropicUsage(usage, event.Message.Usage)
			emitRole()

		case "content_block_start":
			emitRole()
			if event.ContentBlock.Type != "tool_use" {
				continue
			}
			toolIndex := nextToolIndex
			nextToolIndex++
			toolIndexes[event.Index] = toolIndex
			emitChunk([]map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": toolIndex,
						"id":    event.ContentBlock.ID,
						"type":  "function",
						"function": map[string]any{
							"name": event.ContentBlock.Name, "arguments": "",
						},
					}},
				},
			}}, nil)

		case "content_block_delta":
			emitRole()
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					emitChunk([]map[string]any{{
						"index": 0, "delta": map[string]any{"content": event.Delta.Text},
					}}, nil)
				}
			case "thinking_delta":
				if event.Delta.Thinking != "" {
					emitChunk([]map[string]any{{
						"index": 0, "delta": map[string]any{"reasoning": event.Delta.Thinking},
					}}, nil)
				}
			case "input_json_delta":
				toolIndex, ok := toolIndexes[event.Index]
				if ok {
					emitChunk([]map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": []map[string]any{{
								"index":    toolIndex,
								"function": map[string]any{"arguments": event.Delta.PartialJSON},
							}},
						},
					}}, nil)
				}
			}

		case "message_delta":
			if event.Delta.StopReason != "" {
				finishReason = mapAnthropicStopReasonToChat(event.Delta.StopReason)
			}
			applyAnthropicUsage(usage, event.Usage)

		case "message_stop":
			emitFinish()

		case "error":
			emitError()
			break streamLoop

		case "ping", "content_block_stop":
			// No OpenAI equivalent.

		default:
			slog.Debug("ignoring unknown anthropic SSE event", "type", event.Type)
		}
	}
	if err := scanner.Err(); err != nil && !streamFailed {
		slog.Warn("anthropic stream read failed", "model", modelName, "error", err)
		emitError()
	}
	if !streamFailed {
		emitFinish()
	}
	emitDone()
	return responseBytes, usage
}

func applyAnthropicUsage(dst *api.ChunkUsage, src anthropicChatUsage) {
	if src.InputTokens != 0 || src.CacheCreationInputTokens != 0 || src.CacheReadInputTokens != 0 {
		dst.PromptTokens = src.InputTokens + src.CacheCreationInputTokens + src.CacheReadInputTokens
		if src.CacheReadInputTokens > 0 {
			dst.PromptTokensDetails = &api.PromptTokensDetails{CachedTokens: src.CacheReadInputTokens}
		}
	}
	if src.OutputTokens != 0 {
		dst.CompletionTokens = src.OutputTokens
	}
	dst.TotalTokens = dst.PromptTokens + dst.CompletionTokens
}
