package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

// maxImagesPerRequest caps the number of images the vision processor will handle
// in a single request. Beyond this, remaining images get a placeholder to prevent
// a single request from triggering unbounded outbound HTTP calls.
const maxImagesPerRequest = 10

// processImages detects image content in the translated Chat Completions request,
// sends each image to the vision model for description, and replaces the image_url
// parts with text descriptions.
func (p *Pipeline) processImages(ctx context.Context, chatReq map[string]any,
	visionModel *config.ModelConfig) (map[string]any, error) {

	// Normalize messages to []any — translation layers may produce []map[string]any.
	var messages []any
	switch m := chatReq["messages"].(type) {
	case []any:
		messages = m
	case []map[string]any:
		messages = make([]any, len(m))
		for i, msg := range m {
			messages[i] = msg
		}
		chatReq["messages"] = messages
	default:
		return chatReq, nil
	}

	imageCount := 0
	anyModified := false
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}

		msgModified := false
		newContent := make([]any, 0, len(content))
		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok {
				newContent = append(newContent, part)
				continue
			}
			if partMap["type"] != "image_url" {
				newContent = append(newContent, part)
				continue
			}

			imageCount++
			if imageCount > maxImagesPerRequest {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[Image omitted: too many images in request]",
				})
				msgModified = true
				continue
			}

			// Extract image URL for the vision model.
			imageURL := extractImageURL(partMap)
			if imageURL == "" {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[Image: unsupported format]",
				})
				msgModified = true
				continue
			}

			// Send to vision model for description.
			desc, err := p.describeImage(ctx, visionModel, imageURL)
			if err != nil {
				slog.Warn("failed to describe image via vision model",
					"vision_model", visionModel.Name, "error", err)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[Image could not be processed]",
				})
			} else {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("[Image description: %s]", desc),
				})
			}
			msgModified = true
		}

		if msgModified {
			msgMap["content"] = newContent
			messages[i] = msgMap
			anyModified = true
		}
	}

	if anyModified {
		chatReq["messages"] = messages
	}
	return chatReq, nil
}

// extractImageURL gets the URL string from an image_url content part.
func extractImageURL(part map[string]any) string {
	iu, ok := part["image_url"].(map[string]any)
	if !ok {
		return ""
	}
	url, _ := iu["url"].(string)
	return url
}

// describeImage sends an image to a vision-capable model and returns a text description.
// It uses the Pipeline's HTTP client (which has redirect protection).
func (p *Pipeline) describeImage(ctx context.Context, visionModel *config.ModelConfig, imageURL string) (string, error) {
	// Use a dedicated timeout instead of the caller's context. The caller's
	// context is tied to the client connection, which may be closed (e.g. Claude
	// Code retry) before the vision model finishes. A 60s timeout gives large
	// images enough time while still bounding the call.
	visionCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = ctx // original context intentionally unused

	start := time.Now()

	reqBody := map[string]any{
		"model": visionModel.Model,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Describe this image in detail for a coding assistant. Include all visible text, code, UI elements, and layout.",
					},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": imageURL,
						},
					},
				},
			},
		},
		"max_completion_tokens": 1000,
		// Disable reasoning/thinking for vision utility calls — we want all
		// tokens spent on the description, not internal chain-of-thought.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal vision request: %w", err)
	}

	url := visionModel.Backend + api.ChatCompletionsPath
	req, err := http.NewRequestWithContext(visionCtx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build vision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if visionModel.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+visionModel.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision model request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if err != nil {
		return "", fmt.Errorf("read vision response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vision model returned %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("parse vision response: %w", err)
	}
	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("vision model returned empty response")
	}

	desc := chatResp.Choices[0].Message.Content
	slog.Debug("image described by vision model",
		"vision_model", visionModel.Name,
		"duration", time.Since(start),
		"description_len", len(desc))

	return desc, nil
}

// RequestContainsImageURLs checks if a translated Chat Completions request
// contains any image_url content parts. Handles both []any and []map[string]any
// message slice types (depending on which handler built the request).
func RequestContainsImageURLs(chatReq map[string]any) bool {
	// Try []any first (used by pipeline and responses handler).
	if msgs, ok := chatReq["messages"].([]any); ok {
		for _, msg := range msgs {
			if hasImageURLParts(msg) {
				return true
			}
		}
	}
	// Try []map[string]any (used by messages_translate).
	if msgs, ok := chatReq["messages"].([]map[string]any); ok {
		for _, msg := range msgs {
			if hasImageURLParts(msg) {
				return true
			}
		}
	}
	return false
}

// hasImageURLParts checks if a single message (as any) contains image_url content parts.
func hasImageURLParts(msg any) bool {
	m, ok := msg.(map[string]any)
	if !ok {
		return false
	}
	parts, ok := m["content"].([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if ok && p["type"] == "image_url" {
			return true
		}
	}
	return false
}
