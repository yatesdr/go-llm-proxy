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
	[]byte(`"image_url"`),        // OpenAI image format
	[]byte(`"type":"image"`),     // Anthropic image format (after translation)
	[]byte(`"application/pdf"`),  // PDF media type
	[]byte(`JVBERi0`),            // PDF magic bytes in base64
	[]byte(`"type":"document"`),  // Anthropic document format
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

// ShouldProcess returns whether the pipeline should run for the given model.
// Native Anthropic backends skip the pipeline unless force_pipeline is set.
func (p *Pipeline) ShouldProcess(model *config.ModelConfig) bool {
	if model.Type == config.BackendAnthropic && !model.ForcePipeline {
		return false
	}
	return true
}

// resolveVisionProcessor returns the model name to use for vision processing
// for the given target model. Returns "" if vision processing is disabled.
func (p *Pipeline) resolveVisionProcessor(targetModel *config.ModelConfig) string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil {
		if targetModel.Processors.Vision == "none" {
			return ""
		}
		if targetModel.Processors.Vision != "" {
			return targetModel.Processors.Vision
		}
	}
	// Fall back to global config.
	return p.config.Get().Processors.Vision
}

// ResolveWebSearchKey returns the Tavily API key for the given target model.
// Returns "" if web search is disabled for this model.
func (p *Pipeline) ResolveWebSearchKey(targetModel *config.ModelConfig) string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil && targetModel.Processors.WebSearchKey != "" {
		if targetModel.Processors.WebSearchKey == "none" {
			return ""
		}
		return targetModel.Processors.WebSearchKey
	}
	// Fall back to global config.
	return p.config.Get().Processors.WebSearchKey
}

// ProcessRequest runs pre-send processors on a translated Chat Completions request.
// It modifies the request in place and returns it.
func (p *Pipeline) ProcessRequest(ctx context.Context, chatReq map[string]any,
	targetModel *config.ModelConfig) (map[string]any, error) {

	if !p.ShouldProcess(targetModel) {
		return chatReq, nil
	}

	cfg := p.config.Get()

	// Resolve the vision model once (used by both image and PDF processing).
	visionModelName := p.resolveVisionProcessor(targetModel)
	var visionModel *config.ModelConfig
	if visionModelName != "" {
		visionModel = config.FindModel(cfg, visionModelName)
	}

	// Vision: route images to processor if target can't handle them natively.
	if visionModel != nil && (!targetModel.SupportsVision || targetModel.ForcePipeline) {
		var err error
		chatReq, err = p.processImages(ctx, chatReq, visionModel)
		if err != nil {
			slog.Warn("vision processing error", "error", err)
		}
	}

	// PDF: text extraction with vision fallback for scanned pages.
	if visionModel != nil {
		var err error
		chatReq, err = p.processPDFs(ctx, chatReq, visionModel)
		if err != nil {
			slog.Warn("PDF processing error", "error", err)
		}
	}

	// Web search: convert stripped server tools to function tools, or inject.
	chatReq = p.convertOrInjectSearchTool(chatReq, targetModel)

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
