package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

// imageCache stores image URL hash → description so that images are only
// processed once. Subsequent requests containing the same image reuse the
// cached description, making follow-up turns fast.
var imageCache sync.Map // map[string]string

// ResetImageCache clears the image description cache. Exported for testing.
func ResetImageCache() {
	imageCache.Range(func(key, _ any) bool {
		imageCache.Delete(key)
		return true
	})
}

// maxImagesPerRequest caps the number of images the vision processor will handle
// in a single request. Beyond this, remaining images get a placeholder to prevent
// a single request from triggering unbounded outbound HTTP calls.
const maxImagesPerRequest = 10

// maxConcurrentVision limits how many concurrent vision model calls are made.
const maxConcurrentVision = 5

// Vision prompts — the describe prompt is for general images; the OCR prompt is
// for PDF page images where text extraction is more useful than visual description.
// The short OCR prompt is for dedicated OCR models (e.g., PaddleOCR-VL) that
// respond to task-specific prefixes.
const (
	visionPromptDescribe = "Describe this image accurately and objectively. Include all visible subjects, objects, text, and relevant details. Be specific about what you observe."
	visionPromptOCR      = "Extract all text from this page. Reproduce the text content verbatim, preserving structure (headings, paragraphs, lists, tables). Focus on text content, not visual layout."
	ocrModelPrompt       = "OCR:"
)

// processImages detects image content in the translated Chat Completions request,
// sends each image to the vision model for description, and replaces the image_url
// parts with text descriptions. Images are processed concurrently for speed, and
// PDF page images (detected via tool result heuristics) use the OCR model with a
// text-extraction prompt. ocrModel may be nil, in which case visionModel is used.
func (p *Pipeline) processImages(ctx context.Context, chatReq map[string]any,
	visionModel *config.ModelConfig, ocrModel *config.ModelConfig) (map[string]any, error) {

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

	// --- First pass: collect all images that need processing. ---
	//
	// Each image may produce up to two jobs: a vision (describe) job and an
	// OCR (text extraction) job. Cache keys use suffixes ":v" and ":o" to
	// store results independently.
	//
	// For tool-role images (PDF pages, view_image output): OCR only.
	// For user-role images: vision always + OCR if an OCR model is configured.
	type imageJob struct {
		url       string
		cacheKey  string // hash + ":v" or ":o"
		prompt    string
		maxTokens int
		model     *config.ModelConfig
	}
	var jobs []imageJob
	seenKeys := map[string]bool{}

	imageCount := 0
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content := normalizeContentParts(msgMap)
		if content == nil {
			continue
		}

		role, _ := msgMap["role"].(string)
		isToolRole := role == "tool"

		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok || partMap["type"] != "image_url" {
				continue
			}

			imageCount++
			if imageCount > maxImagesPerRequest {
				continue
			}

			url := extractImageURL(partMap)
			if url == "" {
				continue
			}

			hash := hashImageURL(url)

			if isToolRole {
				// Tool-role images (PDF pages, screenshots): OCR only.
				ocrKey := hash + ":o"
				if _, ok := imageCache.Load(ocrKey); ok {
					continue
				}
				if seenKeys[ocrKey] {
					continue
				}
				seenKeys[ocrKey] = true
				ocrMdl := visionModel
				ocrPrompt := visionPromptOCR
				if ocrModel != nil {
					ocrMdl = ocrModel
					ocrPrompt = ocrModelPrompt
				}
				jobs = append(jobs, imageJob{
					url: url, cacheKey: ocrKey,
					prompt: ocrPrompt, maxTokens: 2000, model: ocrMdl,
				})
			} else {
				// User-role images: vision description only.
				// OCR is skipped for user-attached photos — dedicated OCR models
				// hallucinate on natural images. Text in photos is captured
				// adequately by the vision model's description.
				vKey := hash + ":v"
				if _, ok := imageCache.Load(vKey); !ok && !seenKeys[vKey] {
					seenKeys[vKey] = true
					jobs = append(jobs, imageJob{
						url: url, cacheKey: vKey,
						prompt: visionPromptDescribe, maxTokens: 1000, model: visionModel,
					})
				}
			}
		}
	}

	// --- Process all uncached images concurrently. ---
	type jobResult struct {
		desc string
		err  error
	}
	results := make([]jobResult, len(jobs))

	if len(jobs) > 0 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxConcurrentVision)

		for i, job := range jobs {
			wg.Add(1)
			go func(idx int, j imageJob) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				desc, err := p.describeImage(ctx, j.model, j.url, j.prompt, j.maxTokens)
				results[idx] = jobResult{desc: desc, err: err}
			}(i, job)
		}
		wg.Wait()

		// Cache successful results.
		for i, r := range results {
			if r.err != nil {
				slog.Warn("failed to process image",
					"model", jobs[i].model.Name, "cache_key", jobs[i].cacheKey, "error", r.err)
			} else {
				imageCache.Store(jobs[i].cacheKey, r.desc)
			}
		}
	}

	// Build a lookup from cache key → result for jobs that just completed.
	jobDescriptions := map[string]string{}
	jobErrors := map[string]bool{}
	for i, r := range results {
		if r.err != nil {
			jobErrors[jobs[i].cacheKey] = true
		} else {
			jobDescriptions[jobs[i].cacheKey] = r.desc
		}
	}

	// --- Second pass: replace images with combined descriptions. ---
	imageCount = 0
	anyModified := false
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content := normalizeContentParts(msgMap)
		if content == nil {
			continue
		}

		role, _ := msgMap["role"].(string)
		isToolRole := role == "tool"

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

			imageURL := extractImageURL(partMap)
			if imageURL == "" {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[Image: unsupported format]",
				})
				msgModified = true
				continue
			}

			hash := hashImageURL(imageURL)
			replacement := buildImageReplacement(hash, isToolRole, &imageCache, jobDescriptions)

			newContent = append(newContent, map[string]any{
				"type": "text",
				"text": replacement,
			})
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

// normalizeContentParts converts a message's content field to []any, handling
// both []any (from messages_translate) and []map[string]any (from responses_translate).
// Returns nil if content is not an array type. If the content was []map[string]any,
// it is also updated in the message map for downstream consistency.
func normalizeContentParts(msgMap map[string]any) []any {
	switch c := msgMap["content"].(type) {
	case []any:
		return c
	case []map[string]any:
		parts := make([]any, len(c))
		for i, p := range c {
			parts[i] = p
		}
		msgMap["content"] = parts
		return parts
	default:
		return nil
	}
}

// buildImageReplacement constructs the replacement text for a single image.
//
// For tool-role images (PDF pages, screenshots): OCR text only.
// For user-role images: vision description only.
func buildImageReplacement(hash string, isToolRole bool, cache *sync.Map, jobDescs map[string]string) string {
	// Helper to look up a result from cache or fresh job results.
	lookup := func(cacheKey string) (string, bool) {
		if cached, ok := cache.Load(cacheKey); ok {
			return cached.(string), true
		}
		if desc, ok := jobDescs[cacheKey]; ok {
			return desc, true
		}
		return "", false
	}

	if isToolRole {
		// Tool-role: OCR only.
		if ocrText, ok := lookup(hash + ":o"); ok {
			return fmt.Sprintf("[Page text: %s]", ocrText)
		}
		return "[Image could not be processed]"
	}

	// User-role: vision description only.
	if visionDesc, ok := lookup(hash + ":v"); ok {
		return fmt.Sprintf("[Image description: %s]", visionDesc)
	}
	return "[Image could not be processed]"
}

// countImageURLParts counts image_url parts in a content array.
func countImageURLParts(content []any) int {
	n := 0
	for _, part := range content {
		p, ok := part.(map[string]any)
		if ok && p["type"] == "image_url" {
			n++
		}
	}
	return n
}

// hashImageURL returns a hex-encoded SHA-256 hash of the image URL (or data URL).
// This is used as the cache key for image descriptions.
func hashImageURL(imageURL string) string {
	h := sha256.Sum256([]byte(imageURL))
	return fmt.Sprintf("%x", h)
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
// The prompt and maxTokens control the style of description (general vs OCR).
func (p *Pipeline) describeImage(ctx context.Context, visionModel *config.ModelConfig,
	imageURL, prompt string, maxTokens int) (string, error) {

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
						"text": prompt,
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
		"max_completion_tokens": maxTokens,
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
	parts := normalizeContentParts(m)
	if parts == nil {
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
