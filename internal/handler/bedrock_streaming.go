package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/awsauth"
	"go-llm-proxy/internal/awsstream"
)

// maxBedrockStreamBytes bounds the total response bytes we will write to the
// client in one streaming request. Prevents a rogue or compromised upstream
// from streaming unbounded data through the proxy; matches the
// api.MaxResponseBodySize used by the non-streaming paths.
const maxBedrockStreamBytes = api.MaxResponseBodySize

// streamBedrockToAnthropicSSE consumes an AWS event-stream from Bedrock
// ConverseStream and re-emits it on w as an Anthropic Messages SSE stream.
//
// The function assumes the caller has already written the SSE response
// headers and the response status code. It returns the total bytes written
// to the client (for usage logging) and any input/output token counts
// extracted from the Bedrock metadata event.
//
// This bridge is the streaming counterpart to buildAnthropicResponseFromConverse.
// The two share no state; deltas are translated event-by-event based on the
// minimal block-index → block-type map maintained inline.
func streamBedrockToAnthropicSSE(w http.ResponseWriter, body io.Reader, modelName, requestID string) (responseBytes int64, usage *converseUsage) {
	flusher, _ := w.(http.Flusher)

	// Per-frame emit helper, identical pattern to messages_streaming.go.
	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		n, _ := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
		responseBytes += int64(n)
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgID := api.RandomID("msg_")

	// Bedrock contentBlockIndex → Anthropic block kind. We need this so a
	// contentBlockDelta can be routed to the right delta-type
	// (text_delta vs input_json_delta vs thinking_delta).
	type blockKind int
	const (
		blockText blockKind = iota
		blockToolUse
		blockThinking
	)
	blockTypes := map[int]blockKind{}
	openedBlocks := map[int]bool{}

	msgStartEmitted := false
	stopReason := "end_turn"

	emitMessageStart := func() {
		emit("message_start", map[string]any{
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         modelName,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 1},
			},
		})
		emit("ping", map[string]any{})
		msgStartEmitted = true
	}

	r := awsstream.NewReader(body)
	for {
		if responseBytes > maxBedrockStreamBytes {
			slog.Error("bedrock stream exceeded size limit",
				"model", modelName, "bytes", responseBytes, "request_id", requestID)
			emit("error", map[string]any{
				"error": map[string]any{
					"type":    "api_error",
					"message": "upstream stream exceeded size limit",
				},
			})
			break
		}
		msg, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			slog.Warn("bedrock stream decode error",
				"model", modelName, "error", err, "request_id", requestID)
			// Emit an SSE error event so clients see something rather than
			// a silent truncation, then break.
			emit("error", map[string]any{
				"error": map[string]any{
					"type":    "api_error",
					"message": "upstream stream decode error",
				},
			})
			break
		}

		// Only process event frames; surface exceptions as SSE errors.
		// The upstream :exception-type header (ThrottlingException, etc.) is
		// NEVER forwarded to the client — we map to a fixed client vocabulary
		// and log the real type server-side only.
		switch msg.MessageType() {
		case "exception", "error":
			upstreamType := msg.HeaderString(":exception-type")
			clientErrType, clientMsg := classifyStreamException(shapeAnthropic, upstreamType)
			emit("error", map[string]any{
				"error": map[string]any{
					"type":    clientErrType,
					"message": clientMsg,
				},
			})
			slog.Error("bedrock stream exception",
				"model", modelName, "category", clientErrType, "request_id", requestID)
			slog.Debug("bedrock stream exception detail",
				"model", modelName, "upstream_type", upstreamType,
				"scrubbed_payload", awsauth.ScrubAWSErrorBody(msg.Payload),
				"request_id", requestID)
			continue
		case "event", "":
			// fall through
		default:
			continue
		}

		if !msgStartEmitted {
			emitMessageStart()
		}

		switch msg.EventType() {
		case "messageStart":
			// Already emitted message_start above; nothing else to do.

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
			if json.Unmarshal(msg.Payload, &p) != nil {
				continue
			}
			if p.Start.ToolUse != nil {
				blockTypes[p.ContentBlockIndex] = blockToolUse
				openedBlocks[p.ContentBlockIndex] = true
				emit("content_block_start", map[string]any{
					"index": p.ContentBlockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    p.Start.ToolUse.ToolUseID,
						"name":  p.Start.ToolUse.Name,
						"input": map[string]any{},
					},
				})
			}
			// Bedrock omits start frames for text and reasoning blocks; the
			// first contentBlockDelta opens those lazily below.

		case "contentBlockDelta":
			var p struct {
				Delta struct {
					Text    string `json:"text"`
					ToolUse *struct {
						Input string `json:"input"`
					} `json:"toolUse"`
					ReasoningContent *struct {
						Text      string `json:"text"`
						Signature string `json:"signature"`
					} `json:"reasoningContent"`
				} `json:"delta"`
				ContentBlockIndex int `json:"contentBlockIndex"`
			}
			if json.Unmarshal(msg.Payload, &p) != nil {
				continue
			}
			idx := p.ContentBlockIndex

			switch {
			case p.Delta.ReasoningContent != nil:
				if !openedBlocks[idx] {
					blockTypes[idx] = blockThinking
					openedBlocks[idx] = true
					emit("content_block_start", map[string]any{
						"index": idx,
						"content_block": map[string]any{
							"type": "thinking", "thinking": "", "signature": "",
						},
					})
				}
				if p.Delta.ReasoningContent.Text != "" {
					emit("content_block_delta", map[string]any{
						"index": idx,
						"delta": map[string]any{
							"type": "thinking_delta", "thinking": p.Delta.ReasoningContent.Text,
						},
					})
				}
				if p.Delta.ReasoningContent.Signature != "" {
					emit("content_block_delta", map[string]any{
						"index": idx,
						"delta": map[string]any{
							"type": "signature_delta", "signature": p.Delta.ReasoningContent.Signature,
						},
					})
				}

			case p.Delta.ToolUse != nil:
				// Tool-use block was opened by contentBlockStart; emit input
				// JSON fragment as input_json_delta.
				emit("content_block_delta", map[string]any{
					"index": idx,
					"delta": map[string]any{
						"type": "input_json_delta", "partial_json": p.Delta.ToolUse.Input,
					},
				})

			case p.Delta.Text != "":
				if !openedBlocks[idx] {
					blockTypes[idx] = blockText
					openedBlocks[idx] = true
					emit("content_block_start", map[string]any{
						"index":         idx,
						"content_block": map[string]any{"type": "text", "text": ""},
					})
				}
				emit("content_block_delta", map[string]any{
					"index": idx,
					"delta": map[string]any{"type": "text_delta", "text": p.Delta.Text},
				})
			}

		case "contentBlockStop":
			var p struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
			}
			if json.Unmarshal(msg.Payload, &p) != nil {
				continue
			}
			if openedBlocks[p.ContentBlockIndex] {
				emit("content_block_stop", map[string]any{"index": p.ContentBlockIndex})
				delete(openedBlocks, p.ContentBlockIndex)
			}

		case "messageStop":
			var p struct {
				StopReason string `json:"stopReason"`
			}
			if json.Unmarshal(msg.Payload, &p) == nil && p.StopReason != "" {
				stopReason = mapConverseStopReason(p.StopReason)
			}
			// message_delta is emitted once after the loop exits, so the final
			// stop_reason AND usage (from metadata) are both available
			// regardless of which of messageStop/metadata arrives first.

		case "metadata":
			var p struct {
				Usage struct {
					InputTokens           int `json:"inputTokens"`
					OutputTokens          int `json:"outputTokens"`
					CacheReadInputTokens  int `json:"cacheReadInputTokens"`
					CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(msg.Payload, &p) == nil {
				usage = &converseUsage{
					Input:           p.Usage.InputTokens,
					Output:          p.Usage.OutputTokens,
					CacheReadInput:  p.Usage.CacheReadInputTokens,
					CacheWriteInput: p.Usage.CacheWriteInputTokens,
				}
			}
		}
	}

	// Close any blocks still open (defensive — Bedrock should always send
	// contentBlockStop, but a truncated stream might not).
	for idx := range openedBlocks {
		emit("content_block_stop", map[string]any{"index": idx})
	}

	if !msgStartEmitted {
		// Stream ended before any events — emit a minimal error so the client
		// doesn't hang waiting for events. Headers were already written by
		// the caller.
		emit("error", map[string]any{
			"error": map[string]any{
				"type":    "api_error",
				"message": "no events received from upstream",
			},
		})
		return responseBytes, usage
	}

	in, out, cacheRead, cacheWrite := 0, 0, 0, 0
	if usage != nil {
		in, out = usage.Input, usage.Output
		cacheRead, cacheWrite = usage.CacheReadInput, usage.CacheWriteInput
	}
	emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":                in,
			"output_tokens":               out,
			"cache_creation_input_tokens": cacheWrite,
			"cache_read_input_tokens":     cacheRead,
		},
	})
	emit("message_stop", map[string]any{})

	return responseBytes, usage
}
