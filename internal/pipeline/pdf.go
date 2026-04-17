package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-llm-proxy/internal/config"

	"github.com/ledongthuc/pdf"
)

// pdfFailureTTL is how long a total-failure result is cached. Short enough
// that transient upstream issues don't permanently poison the cache, long
// enough that a spammy retry loop from a misbehaving client won't hammer
// the OCR and vision models on every turn.
const pdfFailureTTL = 5 * time.Minute

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
				result := buildPDFResult(filename, "text", text)
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

			// Stage 2: OCR via rasterization (fast path).
			// Rasterize the PDF to PNG pages and send each page to the
			// dedicated OCR model. This is the correct path for OCR
			// backends like paddleOCR-VL that accept images but not raw
			// PDF data URLs. Requires pdftoppm or Ghostscript on the host.
			//
			// Skip when OCR and vision resolve to the same model — the
			// rasterization path is for dedicated OCR models, not general
			// vision models that can accept raw PDFs directly.
			ocrIsDedicated := ocrModel != nil && (visionModel == nil || ocrModel.Name != visionModel.Name)
			if ocrIsDedicated {
				if result, ok := p.tryPDFSourceViaOCR(ctx, ocrModel, pdfBytes, filename); ok {
				pdfCache.Store(pdfCacheKey, result)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": result,
				})
				msgModified = true
				continue
				}
			}

			// Stage 3: Vision fallback with raw PDF.
			// Vision models like Qwen3-VL accept data:application/pdf
			// URLs directly. This covers: no rasterizer installed, no OCR
			// model configured, or OCR returned nothing useful.
			if result, ok := p.tryPDFSource(ctx, visionModel, visionPromptOCR, pdfBytes, filename, "vision"); ok {
				pdfCache.Store(pdfCacheKey, result)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": result,
				})
				msgModified = true
				continue
			}

			// All stages failed — cache with a TTL so transient upstream
			// issues don't permanently block this PDF, but repeated same-turn
			// retries don't re-hit the backends either.
			failResult := "[PDF content could not be extracted]"
			pdfCache.StoreWithTTL(pdfCacheKey, failResult, pdfFailureTTL)
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

// DecodePDFDataURL detects URLs of the form `data:application/pdf;base64,...`
// and returns the base64 payload (without the prefix). Returns false if the
// URL is not a PDF data URL or is malformed. Exported for use by API-layer
// translators that need to recognize PDFs masquerading as images.
func DecodePDFDataURL(url string) (string, bool) {
	const prefix = "data:application/pdf"
	if !strings.HasPrefix(url, prefix) {
		return "", false
	}
	// Find the comma that separates the header from the payload.
	idx := strings.IndexByte(url, ',')
	if idx < 0 {
		return "", false
	}
	// Expect base64 encoding in the header — it's the only encoding the
	// pipeline supports. If a client sends a URL-encoded or plaintext PDF
	// data URL, skip (the shape is never valid for PDFs anyway).
	header := url[:idx]
	if !strings.Contains(header, ";base64") {
		return "", false
	}
	payload := url[idx+1:]
	if payload == "" {
		return "", false
	}
	return payload, true
}

// NormalizePDFDataURLs walks a Chat Completions message list and rewrites
// any image_url parts whose URL is a PDF data URL into pipeline-internal
// pdf_data parts. Idempotent and inexpensive when no PDFs are present
// (early-exits on the first non-matching part). Intended to be called just
// before processPDFs so all three entry APIs (Anthropic Messages,
// Chat Completions, OpenAI Responses) converge on the same internal shape
// regardless of how the client originally submitted the PDF.
func NormalizePDFDataURLs(chatReq map[string]any) {
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
		return
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
			if !ok || partMap["type"] != "image_url" {
				newContent = append(newContent, part)
				continue
			}
			url := extractImageURL(partMap)
			data, isPDF := DecodePDFDataURL(url)
			if !isPDF {
				newContent = append(newContent, part)
				continue
			}
			// Replace image_url with pdf_data. Preserve filename hint if
			// the client supplied one via the (non-standard but seen-in-wild)
			// "filename" sibling field.
			converted := map[string]any{
				"type": "pdf_data",
				"data": data,
			}
			if fn, ok := partMap["filename"].(string); ok && fn != "" {
				converted["filename"] = fn
			}
			newContent = append(newContent, converted)
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

// tryPDFSource attempts to extract text from a scanned PDF using a single
// processor model (either OCR or vision). Returns the formatted result and
// true on success (non-empty trimmed text), empty and false otherwise. A
// nil model is a clean miss — no error is logged and no call is made.
//
// The source label is embedded in the result so operators can see which
// cascade stage answered the request.
func (p *Pipeline) tryPDFSource(ctx context.Context, model *config.ModelConfig, prompt string,
	pdfBytes []byte, filename string, source string) (string, bool) {
	if model == nil {
		return "", false
	}
	desc, err := p.describePDFViaVision(ctx, model, pdfBytes, filename, prompt)
	if err != nil {
		slog.Warn("PDF pipeline stage failed",
			"stage", source, "model", model.Name, "filename", filename, "error", err)
		return "", false
	}
	if len(strings.TrimSpace(desc)) == 0 {
		slog.Warn("PDF pipeline stage returned empty result",
			"stage", source, "model", model.Name, "filename", filename)
		return "", false
	}
	slog.Info("PDF described via pipeline stage",
		"stage", source, "model", model.Name, "filename", filename, "desc_len", len(desc))
	return buildPDFResult(filename, source, desc), true
}

// buildPDFResult formats extracted/described PDF text with an XML-like
// wrapper so the target model can distinguish pipeline-injected content
// from user-authored text. The source attribute identifies which stage
// produced the text: "text" (native extraction), "ocr" (OCR model), or
// "vision" (vision fallback).
func buildPDFResult(filename, source, content string) string {
	if filename != "" {
		return fmt.Sprintf("<pdf_content filename=%q source=%q>\n%s\n</pdf_content>",
			filename, source, content)
	}
	return fmt.Sprintf("<pdf_content source=%q>\n%s\n</pdf_content>", source, content)
}

// tryPDFSourceViaOCR rasterizes a PDF to PNG pages and runs each page
// through a dedicated OCR model (e.g., paddleOCR-VL). This is the fast
// path for scanned PDFs — OCR models are typically smaller and faster than
// general vision models, and purpose-built for text extraction.
//
// Returns the concatenated per-page text wrapped in a <pdf_content> block
// and true on success; empty and false if rasterization fails, no rasterizer
// is installed, or all pages produce empty OCR results.
//
// Requires either `pdftoppm` (from poppler-utils) or `gs` (Ghostscript) on
// the system. If neither is available, returns false so the caller can fall
// through to the vision model with the raw PDF bytes.
func (p *Pipeline) tryPDFSourceViaOCR(ctx context.Context, ocrModel *config.ModelConfig,
	pdfBytes []byte, filename string) (string, bool) {
	if ocrModel == nil {
		return "", false
	}

	pages, err := rasterizePDFPages(pdfBytes, maxPDFPages)
	if err != nil {
		slog.Info("PDF rasterization unavailable or failed, skipping OCR stage",
			"filename", filename, "error", err)
		return "", false
	}
	if len(pages) == 0 {
		slog.Warn("PDF rasterization produced no pages", "filename", filename)
		return "", false
	}

	slog.Info("PDF rasterized for OCR",
		"filename", filename, "pages", len(pages))

	var texts []string
	for i, page := range pages {
		dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(page)
		desc, err := p.describeImage(ctx, ocrModel, dataURL, ocrModelPrompt, 2000)
		if err != nil {
			slog.Warn("OCR failed on rasterized page",
				"filename", filename, "page", i+1, "error", err)
			continue
		}
		if t := strings.TrimSpace(desc); t != "" {
			texts = append(texts, t)
		}
	}

	if len(texts) == 0 {
		slog.Warn("OCR returned no text for any rasterized page",
			"filename", filename, "pages_attempted", len(pages))
		return "", false
	}

	combined := strings.Join(texts, "\n\n")
	slog.Info("PDF OCR via rasterization succeeded",
		"filename", filename, "pages_with_text", len(texts),
		"total_chars", len(combined))
	return buildPDFResult(filename, "ocr", combined), true
}

// rasterizePDFPages converts a PDF to a series of PNG images, one per page,
// using either pdftoppm (preferred, from poppler-utils) or Ghostscript.
// Returns up to maxPages PNGs. Errors if no rasterizer is available on the
// system — the caller should fall through to a different extraction method.
//
// Rasterization runs in a temporary directory that is cleaned up before
// return. The subprocess is time-limited to 30 seconds to prevent runaway
// renders on huge PDFs.
func rasterizePDFPages(pdfBytes []byte, maxPages int) ([][]byte, error) {
	// Find a rasterizer.
	pdftoppm, _ := exec.LookPath("pdftoppm")
	gs, _ := exec.LookPath("gs")
	if gs == "" {
		gs, _ = exec.LookPath("ghostscript")
	}
	if pdftoppm == "" && gs == "" {
		return nil, fmt.Errorf("no PDF rasterizer found (install poppler-utils or ghostscript)")
	}

	tmpDir, err := os.MkdirTemp("", "pdf-raster-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pdfPath := filepath.Join(tmpDir, "input.pdf")
	if err := os.WriteFile(pdfPath, pdfBytes, 0600); err != nil {
		return nil, fmt.Errorf("write temp PDF: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	outPrefix := filepath.Join(tmpDir, "page")
	if pdftoppm != "" {
		cmd := exec.CommandContext(ctx, pdftoppm, "-png", "-r", "150",
			"-l", strconv.Itoa(maxPages), pdfPath, outPrefix)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("pdftoppm: %w (output: %s)", err, string(out))
		}
	} else {
		outPattern := outPrefix + "-%03d.png"
		cmd := exec.CommandContext(ctx, gs, "-sDEVICE=png16m", "-r150",
			"-dBATCH", "-dNOPAUSE", "-dQUIET",
			fmt.Sprintf("-dFirstPage=%d", 1),
			fmt.Sprintf("-dLastPage=%d", maxPages),
			"-o", outPattern, pdfPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ghostscript: %w (output: %s)", err, string(out))
		}
	}

	// Collect output PNGs in sorted order.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("read temp dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var pages [][]byte
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
		if err != nil {
			slog.Warn("failed to read rasterized page", "file", e.Name(), "error", err)
			continue
		}
		pages = append(pages, data)
		if len(pages) >= maxPages {
			break
		}
	}
	return pages, nil
}

// describePDFViaVision sends a PDF as a base64 data URL to a vision model
// that supports direct PDF input (e.g., Qwen3-VL). Used as the final
// fallback when OCR via rasterization is unavailable or fails.
func (p *Pipeline) describePDFViaVision(ctx context.Context, model *config.ModelConfig, pdfBytes []byte, filename string, prompt string) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(pdfBytes)
	dataURL := "data:application/pdf;base64," + b64

	desc, err := p.describeImage(ctx, model, dataURL, prompt, 2000)
	if err != nil {
		return "", fmt.Errorf("vision PDF fallback: %w", err)
	}

	return desc, nil
}
