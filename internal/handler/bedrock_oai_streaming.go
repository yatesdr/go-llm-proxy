package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/awsstream"
)

// streamBedrockToChatSSE consumes an AWS event-stream from Bedrock
// ConverseStream and re-emits it as OpenAI Chat Completions SSE chunks
// ("data: {...}\n\n", terminated by "data: [DONE]\n\n").
//
// The caller must have already written the SSE response headers and 200 status.
//
// includeUsage controls whether a final usage chunk is emitted before [DONE]
// — clients pass `stream_options.include_usage=true` to opt in.
func streamBedrockToChatSSE(w http.ResponseWriter, body io.Reader, modelName string, includeUsage bool) (responseBytes int64, usage *converseUsage) {
	flusher, _ := w.(http.Flusher)

	chunkID := api.RandomID("chatcmpl-")

	emitChunk := func(choices []map[string]any, u *api.ChunkUsage) {
		obj := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"model":   modelName,
			"choices": choices,
		}
		if u != nil {
			obj["usage"] = u
		}
		data, _ := json.Marshal(obj)
		n, _ := fmt.Fprintf(w, "data: %s\n\n", data)
		responseBytes += int64(n)
		if flusher != nil {
			flusher.Flush()
		}
	}

	emitDone := func() {
		n, _ := fmt.Fprintf(w, "data: [DONE]\n\n")
		responseBytes += int64(n)
		if flusher != nil {
			flusher.Flush()
		}
	}

	emitError := func(msg string) {
		obj := map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "api_error",
			},
		}
		data, _ := json.Marshal(obj)
		n, _ := fmt.Fprintf(w, "data: %s\n\n", data)
		responseBytes += int64(n)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Per-Bedrock-block-index → (assigned OAI tool_call index, name).
	// OAI streams tool calls indexed from 0 across the *response*, not per
	// content block, so we re-number as we go.
	type toolBlockState struct {
		oaiIndex int
		name     string
	}
	toolBlocks := map[int]*toolBlockState{}
	nextToolIndex := 0

	roleEmitted := false
	finishReason := "stop"

	r := awsstream.NewReader(body)
	for {
		if responseBytes > maxBedrockStreamBytes {
			slog.Error("bedrock stream exceeded size limit",
				"model", modelName, "bytes", responseBytes)
			emitError("upstream stream exceeded size limit")
			break
		}
		msg, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			slog.Warn("bedrock stream decode error", "model", modelName, "error", err)
			emitError("upstream stream decode error")
			break
		}

		switch msg.MessageType() {
		case "exception", "error":
			errType := msg.HeaderString(":exception-type")
			if errType == "" {
				errType = "api_error"
			}
			emitError(fmt.Sprintf("bedrock %s", errType))
			continue
		case "event", "":
			// fall through
		default:
			continue
		}

		if !roleEmitted {
			emitChunk([]map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant"},
			}}, nil)
			roleEmitted = true
		}

		switch msg.EventType() {
		case "messageStart":
			// role chunk already emitted

		case "contentBlockStart":
			var p struct {
				Start struct {
					ToolUse *struct {
						ToolUseID string `json:"toolUseId"`
						Name      string `json:"name"`
					} `json:"toolUse"`
				} `json:"start"`
				ContentBlockIndex int `json:"contentBlockIndex"`
			}
			if json.Unmarshal(msg.Payload, &p) != nil || p.Start.ToolUse == nil {
				continue
			}
			tb := &toolBlockState{oaiIndex: nextToolIndex, name: p.Start.ToolUse.Name}
			nextToolIndex++
			toolBlocks[p.ContentBlockIndex] = tb
			emitChunk([]map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": tb.oaiIndex,
						"id":    p.Start.ToolUse.ToolUseID,
						"type":  "function",
						"function": map[string]any{
							"name":      tb.name,
							"arguments": "",
						},
					}},
				},
			}}, nil)

		case "contentBlockDelta":
			var p struct {
				Delta struct {
					Text    string `json:"text"`
					ToolUse *struct {
						Input string `json:"input"`
					} `json:"toolUse"`
					ReasoningContent *struct {
						Text string `json:"text"`
					} `json:"reasoningContent"`
				} `json:"delta"`
				ContentBlockIndex int `json:"contentBlockIndex"`
			}
			if json.Unmarshal(msg.Payload, &p) != nil {
				continue
			}
			switch {
			case p.Delta.ReasoningContent != nil && p.Delta.ReasoningContent.Text != "":
				// Emit as the non-standard `reasoning` field — many OAI-compat
				// backends use this; standards-strict clients ignore it.
				emitChunk([]map[string]any{{
					"index": 0,
					"delta": map[string]any{"reasoning": p.Delta.ReasoningContent.Text},
				}}, nil)
			case p.Delta.ToolUse != nil:
				tb, ok := toolBlocks[p.ContentBlockIndex]
				if !ok {
					continue
				}
				emitChunk([]map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": tb.oaiIndex,
							"function": map[string]any{
								"arguments": p.Delta.ToolUse.Input,
							},
						}},
					},
				}}, nil)
			case p.Delta.Text != "":
				emitChunk([]map[string]any{{
					"index": 0,
					"delta": map[string]any{"content": p.Delta.Text},
				}}, nil)
			}

		case "contentBlockStop":
			// OAI doesn't have a per-block stop signal; nothing to emit.

		case "messageStop":
			var p struct {
				StopReason string `json:"stopReason"`
			}
			if json.Unmarshal(msg.Payload, &p) == nil && p.StopReason != "" {
				finishReason = mapConverseStopReasonToChat(p.StopReason)
			}

		case "metadata":
			var p struct {
				Usage struct {
					InputTokens  int `json:"inputTokens"`
					OutputTokens int `json:"outputTokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(msg.Payload, &p) == nil {
				usage = &converseUsage{Input: p.Usage.InputTokens, Output: p.Usage.OutputTokens}
			}
		}
	}

	if !roleEmitted {
		emitError("no events received from upstream")
		emitDone()
		return responseBytes, usage
	}

	// Final chunk: finish_reason on an empty delta (matches OAI behavior).
	emitChunk([]map[string]any{{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": finishReason,
	}}, nil)

	// Optional usage chunk: empty choices, usage populated.
	if includeUsage && usage != nil {
		emitChunk(nil, &api.ChunkUsage{
			PromptTokens:     usage.Input,
			CompletionTokens: usage.Output,
			TotalTokens:      usage.Input + usage.Output,
		})
	}

	emitDone()
	return responseBytes, usage
}
