package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"go-llm-proxy/internal/config"

	"github.com/ledongthuc/pdf"
)

// pdfCache stores PDF data hash → extracted/described text so that PDFs are only
// processed once. Subsequent requests containing the same PDF reuse the cached
// result, avoiding repeated text extraction and vision model calls. Bounded to
// prevent unbounded memory growth in long-running processes.
var pdfCache = newBoundedCache()

// ResetPDFCache clears the PDF description cache. Exported for testing.
func ResetPDFCache() {
	pdfCache.Reset()
}

// minTextLength is the minimum number of characters for text extraction to be
// considered successful. Below this, the PDF is likely scanned/image-only and
// we fall back to the vision model.
const minTextLength = 50

// maxPDFTextLength caps extracted text to avoid overwhelming the model's context.
const maxPDFTextLength = 100_000

// maxPDFPages caps the number of pages processed via vision fallback to avoid
// unbounded outbound HTTP calls for large scanned PDFs.
const maxPDFPages = 20

// processPDFs detects PDF content in the translated Chat Completions request,
// extracts text, and replaces the PDF with text content blocks. Falls back to
// the OCR model (or vision model) for scanned/image-heavy PDFs.
func (p *Pipeline) processPDFs(ctx context.Context, chatReq map[string]any,
	visionModel *config.ModelConfig, ocrModel *config.ModelConfig) (map[string]any, error) {

	// Normalize messages to []any (same pattern as processImages).
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

		msgModified := false
		newContent := make([]any, 0, len(content))
		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok {
				newContent = append(newContent, part)
				continue
			}
			if partMap["type"] != "pdf_data" {
				newContent = append(newContent, part)
				continue
			}

			slog.Info("processing PDF content block")

			// Extract PDF data.
			b64Data, _ := partMap["data"].(string)
			filename, _ := partMap["filename"].(string)
			if b64Data == "" {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[PDF: no data provided]",
				})
				msgModified = true
				continue
			}

			// Check the cache first — avoid re-processing the same PDF
			// on every conversational turn.
			pdfCacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(b64Data)))
			if cached, ok := pdfCache.Load(pdfCacheKey); ok {
				slog.Debug("PDF cache hit", "filename", filename)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": cached,
				})
				msgModified = true
				continue
			}

			// Try standard base64 first, then URL-safe, then with whitespace stripped.
			pdfBytes, err := decodePDFBase64(b64Data)
			if err != nil {
				slog.Warn("failed to decode PDF base64", "error", err, "data_len", len(b64Data))
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[PDF content could not be decoded]",
				})
				msgModified = true
				continue
			}
			slog.Info("PDF decoded", "filename", filename, "pdf_bytes", len(pdfBytes))

			// Stage 1: text extraction (pure Go, fast).
			text, err := extractPDFText(pdfBytes)
			if err != nil {
				slog.Info("PDF text extraction failed, trying vision fallback",
					"filename", filename, "error", err, "pdf_bytes", len(pdfBytes))
			} else {
				slog.Info("PDF text extraction result",
					"filename", filename, "text_len", len(strings.TrimSpace(text)))
			}
			if err == nil && len(strings.TrimSpace(text)) >= minTextLength {
				if len(text) > maxPDFTextLength {
					text = text[:maxPDFTextLength] + "\n\n[PDF text truncated at 100K characters]"
				}
				label := "PDF content"
				if filename != "" {
					label = fmt.Sprintf("PDF: %s", filename)
				}
				result := fmt.Sprintf("[%s]\n\n%s", label, text)
				pdfCache.Store(pdfCacheKey, result)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": result,
				})
				slog.Debug("PDF text extracted",
					"filename", filename,
					"text_len", len(text))
				msgModified = true
				continue
			}

			// Stage 2: OCR/vision fallback for scanned/image PDFs.
			// Prefer dedicated OCR model if configured; fall back to vision model.
			fallbackModel := ocrModel
			fallbackPrompt := ocrModelPrompt
			if fallbackModel == nil {
				fallbackModel = visionModel
				fallbackPrompt = visionPromptOCR
			}
			if fallbackModel != nil {
				desc, vErr := p.describePDFViaVision(ctx, fallbackModel, pdfBytes, filename, fallbackPrompt)
				if vErr == nil && len(strings.TrimSpace(desc)) > 0 {
					label := "PDF content (scanned)"
					if filename != "" {
						label = fmt.Sprintf("PDF: %s (scanned)", filename)
					}
					result := fmt.Sprintf("[%s]\n\n%s", label, desc)
					pdfCache.Store(pdfCacheKey, result)
					newContent = append(newContent, map[string]any{
						"type": "text",
						"text": result,
					})
					slog.Debug("PDF described via OCR/vision model",
						"model", fallbackModel.Name,
						"filename", filename,
						"desc_len", len(desc))
					msgModified = true
					continue
				}
				slog.Warn("OCR/vision fallback failed for PDF",
					"filename", filename, "error", vErr)
			}

			// Both stages failed — cache the failure so we don't re-attempt
			// the same failing PDF on every conversational turn.
			failResult := "[PDF content could not be extracted]"
			pdfCache.Store(pdfCacheKey, failResult)
			newContent = append(newContent, map[string]any{
				"type": "text",
				"text": failResult,
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

// decodePDFBase64 decodes base64-encoded PDF data, trying multiple encodings.
func decodePDFBase64(data string) ([]byte, error) {
	// Standard base64.
	if b, err := base64.StdEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	// URL-safe base64.
	if b, err := base64.URLEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	// Raw (no padding) variants.
	if b, err := base64.RawStdEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	// Strip whitespace and retry.
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, data)
	if b, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(cleaned)
}

// extractPDFText uses ledongthuc/pdf to extract plain text from PDF bytes.
// Wrapped in recover to handle panics from malformed PDFs.
func extractPDFText(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PDF library panic: %v", r)
		}
	}()

	reader := bytes.NewReader(data)
	pdfReader, err := pdf.NewReader(reader, int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open PDF: %w", err)
	}

	plainText, err := pdfReader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extract text: %w", err)
	}

	raw, err := io.ReadAll(io.LimitReader(plainText, maxPDFTextLength+1000))
	if err != nil {
		return "", fmt.Errorf("read text: %w", err)
	}

	return string(raw), nil
}

// describePDFViaVision sends the first N pages of a PDF as a base64 document
// to the OCR or vision model for text extraction. This handles scanned PDFs
// where text extraction produces little/no content.
func (p *Pipeline) describePDFViaVision(ctx context.Context, model *config.ModelConfig, pdfBytes []byte, filename string, prompt string) (string, error) {
	// Send the full PDF as a base64 data URL to the model.
	// Many vision models (including Qwen) can handle PDF pages as images
	// when sent as data URLs.
	b64 := base64.StdEncoding.EncodeToString(pdfBytes)
	dataURL := "data:application/pdf;base64," + b64

	desc, err := p.describeImage(ctx, model, dataURL, prompt, 2000)
	if err != nil {
		return "", fmt.Errorf("vision PDF fallback: %w", err)
	}

	return desc, nil
}
