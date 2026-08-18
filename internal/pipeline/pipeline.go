package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/config"
)

// InternalKeyStrippedTools is the map key used to pass stripped server tool types
// from the translation layer to the pipeline. Deleted before sending to backend.
const InternalKeyStrippedTools = "_stripped_server_tools"

// Pipeline orchestrates pre-send content processing for translated Chat Completions requests.
// It detects unsupported content (images, PDFs) and routes them to capable processor models.
type Pipeline struct {
	config *config.ConfigStore
	client *http.Client
}

// NewPipeline creates a pipeline that uses the given config and HTTP client for processor calls.
func NewPipeline(cs *config.ConfigStore, client *http.Client) *Pipeline {
	return &Pipeline{config: cs, client: client}
}

// processingSignatures are byte patterns that indicate a request body may contain
// content that needs pipeline processing. Used for cheap pre-scan before full JSON parse.
var processingSignatures = [][]byte{
	[]byte(`"image_url"`),       // OpenAI image format
	[]byte(`"type":"image"`),    // Anthropic image format (after translation)
	[]byte(`"application/pdf"`), // PDF media type
	[]byte(`JVBERi0`),           // PDF magic bytes in base64
	[]byte(`"type":"document"`), // Anthropic document format
	[]byte(`"pdf_data"`),        // Pipeline-internal PDF marker (after translation)
}

// BodyNeedsProcessing does a fast string scan to detect if the raw request body
// contains content that may need pipeline processing. This avoids full JSON parse
// for the common case of text-only requests.
func (p *Pipeline) BodyNeedsProcessing(body []byte) bool {
	for _, sig := range processingSignatures {
		if bytes.Contains(body, sig) {
			return true
		}
	}
	return false
}

// ShouldProcess returns whether any pipeline stage should run for the given
// model — a fast pre-check so handlers can skip the pipeline entirely when
// no interception stage is enabled.
func (p *Pipeline) ShouldProcess(model *config.ModelConfig) bool {
	return p.shouldRewriteVision(model) || p.shouldRewriteDocuments(model) || p.shouldRewriteSearch(model)
}

// shouldRewriteVision reports whether the proxy should intercept image_url
// content for this model (describe it via the vision cascade) rather than
// forwarding it as-is. Auto follows the "Vision capable" flag — a model that
// can't accept images itself gets the fallback; one that can is left alone.
// Upstream protocol plays no part: an Anthropic-shaped backend is not
// necessarily real Claude, and a non-vision-capable model needs the same
// help regardless of which protocol it speaks.
func (p *Pipeline) shouldRewriteVision(model *config.ModelConfig) bool {
	switch model.RewriteVision {
	case "on":
		return true
	case "off":
		return false
	default:
		return model.ForcePipeline || !model.SupportsVision
	}
}

// shouldRewriteDocuments reports whether the proxy should intercept pdf_data
// content for this model (native text extraction, the Documents-tab
// processor, OCR, or vision-PDF fallback) rather than forwarding it as-is.
// Auto shares the "Vision capable" signal with shouldRewriteVision: native PDF
// understanding is built on vision capability in practice, so a model that
// can't accept images natively is assumed unable to handle raw PDFs either.
func (p *Pipeline) shouldRewriteDocuments(model *config.ModelConfig) bool {
	switch model.RewriteDocuments {
	case "on":
		return true
	case "off":
		return false
	default:
		return model.ForcePipeline || !model.SupportsVision
	}
}

// shouldRewriteSearch reports whether the proxy should inject/convert the
// web_search tool for this model. Auto activates only when a search key
// actually resolves for this model — protocol plays no part, since a
// third-party Anthropic-shaped backend has no native search tool of its own
// any more than an OpenAI-shaped one does. Force off opts a specific model
// out even when a search key is configured; force on has no separate effect
// beyond auto today, since there is nothing to force without a key.
func (p *Pipeline) shouldRewriteSearch(model *config.ModelConfig) bool {
	switch model.RewriteWebSearch {
	case "on":
		return true
	case "off":
		return false
	default:
		return len(p.ResolveSearchEntries(model)) > 0
	}
}

// resolveVisionProcessors returns the priority-ordered vision cascade for the
// given target model. A per-model override (single model, or "none") beats the
// global cascade. Empty result = vision processing disabled.
func (p *Pipeline) resolveVisionProcessors(targetModel *config.ModelConfig) []string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil {
		if targetModel.Processors.Vision == "none" {
			return nil
		}
		if targetModel.Processors.Vision != "" {
			return []string{targetModel.Processors.Vision}
		}
	}
	// Fall back to the global cascade.
	return p.config.Get().Processors.EffectiveVisionModels()
}

// resolveVisionProcessor returns the head of the vision cascade ("" = disabled).
func (p *Pipeline) resolveVisionProcessor(targetModel *config.ModelConfig) string {
	if l := p.resolveVisionProcessors(targetModel); len(l) > 0 {
		return l[0]
	}
	return ""
}

// resolveOCRProcessor returns the model name to use for OCR processing
// (PDF page images). Falls back to the vision processor if no OCR model is set.
// Returns "" if both are disabled.
func (p *Pipeline) resolveOCRProcessor(targetModel *config.ModelConfig) string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil {
		if targetModel.Processors.OCR == "none" {
			return ""
		}
		if targetModel.Processors.OCR != "" {
			return targetModel.Processors.OCR
		}
	}
	// Fall back to global OCR config.
	if ocr := p.config.Get().Processors.OCR; ocr != "" {
		return ocr
	}
	// Fall back to vision processor.
	return p.resolveVisionProcessor(targetModel)
}

// ResolveSearchEntries returns the priority-ordered web-search cascade for the
// given target model. A per-model override (single key, or "none") beats the
// global cascade. Empty result = web search disabled.
func (p *Pipeline) ResolveSearchEntries(targetModel *config.ModelConfig) []config.WebSearchKeyEntry {
	if targetModel.Processors != nil && targetModel.Processors.WebSearchKey != "" {
		if targetModel.Processors.WebSearchKey == "none" {
			return nil
		}
		k := targetModel.Processors.WebSearchKey
		return []config.WebSearchKeyEntry{{Provider: config.InferSearchProvider(k), Key: k}}
	}
	return p.config.Get().Processors.EffectiveSearchKeys()
}

// ResolveWebSearchKey returns the head key of the search cascade ("" = disabled).
// Kept for enablement checks; execution should use the cascade.
func (p *Pipeline) ResolveWebSearchKey(targetModel *config.ModelConfig) string {
	if e := p.ResolveSearchEntries(targetModel); len(e) > 0 {
		return e[0].Key
	}
	return ""
}

// ProcessRequest runs pre-send processors on a translated Chat Completions request.
// It modifies the request in place and returns it.
func (p *Pipeline) ProcessRequest(ctx context.Context, chatReq map[string]any,
	targetModel *config.ModelConfig) (map[string]any, error) {

	rewriteVisionEnabled := p.shouldRewriteVision(targetModel)
	rewriteDocumentsEnabled := p.shouldRewriteDocuments(targetModel)
	rewriteSearchEnabled := p.shouldRewriteSearch(targetModel)
	if !rewriteVisionEnabled && !rewriteDocumentsEnabled && !rewriteSearchEnabled {
		return chatReq, nil
	}

	cfg := p.config.Get()

	// Resolve the vision cascade once (used by both image and PDF processing).
	// Entries that are the target itself are dropped (pointless round-trip).
	var visionModels []*config.ModelConfig
	for _, name := range p.resolveVisionProcessors(targetModel) {
		if m := config.FindModel(cfg, name); m != nil && m.Name != targetModel.Name {
			visionModels = append(visionModels, m)
		}
	}
	var visionModel *config.ModelConfig
	if len(visionModels) > 0 {
		visionModel = visionModels[0]
	}

	// Resolve the OCR model (used for PDF page images). Falls back to vision model.
	ocrModelName := p.resolveOCRProcessor(targetModel)
	var ocrModel *config.ModelConfig
	if ocrModelName != "" {
		ocrModel = config.FindModel(cfg, ocrModelName)
	}

	// Normalize PDF data URLs disguised as image_url into pdf_data parts.
	// Runs before both image and PDF processors so that Chat Completions and
	// Responses API clients (which have no structured PDF input) converge on
	// the same internal shape as Anthropic's document blocks.
	if rewriteDocumentsEnabled {
		NormalizePDFDataURLs(chatReq)
	}

	// Vision: route images to processor if target can't handle them natively,
	// or if rewrite_vision is explicitly "on" (force describing even though
	// the model could accept the image directly). Gated independently of PDF
	// handling — image_url content only.
	if rewriteVisionEnabled && visionModel != nil && (!targetModel.SupportsVision || targetModel.RewriteVision == "on") {
		var err error
		chatReq, err = p.processImages(ctx, chatReq, visionModels, ocrModel)
		if err != nil {
			slog.Warn("vision processing error", "error", err)
		}
	}

	// PDF: text extraction (always attempted) with OCR/vision fallback for scanned pages.
	// Prefers ocrModel for scanned PDFs; falls back to visionModel if no OCR model configured.
	// Gated independently of image handling — pdf_data content only.
	if rewriteDocumentsEnabled {
		var err error
		chatReq, err = p.processPDFs(ctx, chatReq, visionModels, ocrModel)
		if err != nil {
			slog.Warn("PDF processing error", "error", err)
		}
	}

	// Web search: convert stripped server tools to function tools, or inject.
	if rewriteSearchEnabled {
		chatReq = p.convertOrInjectSearchTool(chatReq, targetModel)
	}

	// Clean up internal metadata that shouldn't be sent to the backend.
	delete(chatReq, InternalKeyStrippedTools)

	return chatReq, nil
}

// pipelineError builds a formatted error message for pipeline failures.
func pipelineError(feature, model, docSection, originalErr string) string {
	return fmt.Sprintf(
		"The backend model (%s) does not support %s, and the proxy could not process it.\n\n"+
			"To enable %s support, configure the proxy:\n\n"+
			"    processors:\n"+
			"      %s\n\n"+
			"Original error:\n    %s",
		model, feature, feature, docSection, originalErr,
	)
}

// imageNotSupportedError returns a friendly error when images are sent to a text-only
// model and no vision processor is configured.
func imageNotSupportedError(modelName string, originalErr string) string {
	return pipelineError("image inputs", modelName,
		"vision: your-vision-model    # any vision-capable model", originalErr)
}

// searchNotConfiguredError returns a friendly error when web search is requested
// but no Tavily key is configured.
func searchNotConfiguredError() string {
	return "Web search was requested but no search API key is configured on the proxy.\n\n" +
		"To enable web search, add a Tavily API key to your proxy config:\n\n" +
		"    processors:\n" +
		"      web_search_key: tvly-your-key"
}
