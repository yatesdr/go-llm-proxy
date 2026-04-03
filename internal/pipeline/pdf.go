package pipeline

import (
	"context"

	"go-llm-proxy/internal/config"
)

// processPDFs detects PDF content in the translated Chat Completions request,
// extracts text, and replaces the PDF with text content blocks.
// Currently a stub — PDF processing will be implemented with a Go PDF library.
func (p *Pipeline) processPDFs(ctx context.Context, chatReq map[string]any,
	visionModel *config.ModelConfig) (map[string]any, error) {
	// TODO: Phase 4 — implement PDF text extraction + vision fallback
	return chatReq, nil
}
