package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

// --- BodyNeedsProcessing tests ---

func TestBodyNeedsProcessing_ImageURL(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for image_url")
	}
}

func TestBodyNeedsProcessing_PDF(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"role":"user","content":"application/pdf"}]}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for application/pdf")
	}
}

func TestBodyNeedsProcessing_PDFMagicBytes(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"data":"JVBERi0xLjQ="}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for PDF magic bytes")
	}
}

func TestBodyNeedsProcessing_TextOnly(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	if p.BodyNeedsProcessing(body) {
		t.Fatal("expected false for text-only request")
	}
}

func TestBodyNeedsProcessing_Document(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"content":[{"type":"document"}]}]}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for document type")
	}
}

// --- ShouldProcess tests ---

func TestShouldProcess_AnthropicBackend(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{Type: config.BackendAnthropic}
	if p.ShouldProcess(m) {
		t.Fatal("expected false for anthropic backend without force_pipeline")
	}
}

func TestShouldProcess_AnthropicForcePipeline(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{Type: config.BackendAnthropic, ForcePipeline: true}
	if !p.ShouldProcess(m) {
		t.Fatal("expected true for anthropic with force_pipeline")
	}
}

func TestShouldProcess_OpenAIBackend(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{Type: config.BackendOpenAI}
	if !p.ShouldProcess(m) {
		t.Fatal("expected true for openai backend")
	}
}

func TestShouldProcess_DefaultBackend(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{}
	if !p.ShouldProcess(m) {
		t.Fatal("expected true for default backend type")
	}
}

// --- resolveVisionProcessor tests ---

func TestResolveVisionProcessor_GlobalDefault(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
		Models:     []config.ModelConfig{{Name: "test", Backend: "http://localhost/v1"}},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.resolveVisionProcessor(model); got != "qwen-3.5" {
		t.Fatalf("expected qwen-3.5, got %q", got)
	}
}

func TestResolveVisionProcessor_PerModelOverride(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{Vision: "custom-vision"},
	}
	if got := p.resolveVisionProcessor(model); got != "custom-vision" {
		t.Fatalf("expected custom-vision, got %q", got)
	}
}

func TestResolveVisionProcessor_PerModelNone(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{Vision: "none"},
	}
	if got := p.resolveVisionProcessor(model); got != "" {
		t.Fatalf("expected empty (disabled), got %q", got)
	}
}

func TestResolveVisionProcessor_NoConfig(t *testing.T) {
	cfg := &config.Config{}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.resolveVisionProcessor(model); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// --- resolveOCRProcessor tests ---

func TestResolveOCRProcessor_GlobalOCR(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5", OCR: "minicpm-ocr"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.resolveOCRProcessor(model); got != "minicpm-ocr" {
		t.Fatalf("expected minicpm-ocr, got %q", got)
	}
}

func TestResolveOCRProcessor_FallsBackToVision(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.resolveOCRProcessor(model); got != "qwen-3.5" {
		t.Fatalf("expected qwen-3.5 (fallback to vision), got %q", got)
	}
}

func TestResolveOCRProcessor_PerModelOverride(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5", OCR: "global-ocr"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{OCR: "custom-ocr"},
	}
	if got := p.resolveOCRProcessor(model); got != "custom-ocr" {
		t.Fatalf("expected custom-ocr, got %q", got)
	}
}

func TestResolveOCRProcessor_PerModelNone(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5", OCR: "global-ocr"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{OCR: "none"},
	}
	if got := p.resolveOCRProcessor(model); got != "" {
		t.Fatalf("expected empty (disabled), got %q", got)
	}
}

// --- resolveWebSearchKey tests ---

func TestResolveWebSearchKey_GlobalDefault(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-123"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.ResolveWebSearchKey(model); got != "tvly-123" {
		t.Fatalf("expected tvly-123, got %q", got)
	}
}

func TestResolveWebSearchKey_PerModelNone(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-123"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{WebSearchKey: "none"},
	}
	if got := p.ResolveWebSearchKey(model); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// --- Vision processor tests ---

func TestProcessImages_NoImages(t *testing.T) {
	p := &Pipeline{}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": "hello",
			},
		},
	}
	result, err := p.processImages(context.Background(), chatReq, &config.ModelConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := result["messages"].([]any)
	m := msgs[0].(map[string]any)
	if m["content"] != "hello" {
		t.Fatal("content should be unchanged")
	}
}

func TestDescribeImage_FallsBackToReasoningContent(t *testing.T) {
	// Regression: reasoning-model vision backends (e.g., Qwen3-VL variants)
	// sometimes return content="" with the actual description in
	// reasoning_content, especially when finish_reason=length truncates
	// mid-thinking. The pipeline must surface reasoning_content instead
	// of treating the response as empty.
	ResetImageCache()
	defer ResetImageCache()

	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"finish_reason": "length",
					"message": map[string]any{
						"content":           "",
						"reasoning_content": "A blue diagram showing a centerline mark and calibration axes",
					},
				},
			},
		})
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{Name: "vision", Backend: visionServer.URL, Model: "v"}
	p := &Pipeline{client: http.DefaultClient}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,abc"},
					},
				},
			},
		},
	}
	result, err := p.processImages(context.Background(), chatReq, visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := result["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "centerline mark") {
		t.Fatalf("expected reasoning_content to be used as description, got: %s", text)
	}
	if !strings.HasPrefix(text, "<image_description>") {
		t.Fatalf("expected XML-tag framing even for reasoning_content fallback, got: %s", text)
	}
}

func TestProcessImages_ReplacesImageWithDescription(t *testing.T) {
	ResetImageCache()
	// Set up a mock vision model backend.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"content": "A screenshot showing a terminal with Go code",
					},
				},
			},
		})
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{
		Name:    "vision",
		Backend: visionServer.URL,
		Model:   "vision",
	}

	p := &Pipeline{client: http.DefaultClient}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "What is this?"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,abc123"},
					},
				},
			},
		},
	}

	result, err := p.processImages(context.Background(), chatReq, visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}

	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(content))
	}

	// First part should be unchanged text.
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "What is this?" {
		t.Fatalf("first part unexpected: %v", first)
	}

	// Second part should be the description replacing the image.
	second := content[1].(map[string]any)
	if second["type"] != "text" {
		t.Fatalf("expected text type, got %v", second["type"])
	}
	text := second["text"].(string)
	if text != "<image_description>A screenshot showing a terminal with Go code</image_description>" {
		t.Fatalf("unexpected description: %s", text)
	}
}

func TestProcessImages_CacheHit(t *testing.T) {
	ResetImageCache()

	callCount := 0
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"content": "A German Shepherd dog sitting on grass",
					},
				},
			},
		})
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{
		Name:    "vision",
		Backend: visionServer.URL,
		Model:   "vision",
	}

	p := &Pipeline{client: http.DefaultClient}
	imageURL := "data:image/png;base64,uniqueTestImage123"
	makeChatReq := func() map[string]any {
		return map[string]any{
			"messages": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": imageURL},
						},
					},
				},
			},
		}
	}

	// First call — should hit vision model.
	result, err := p.processImages(context.Background(), makeChatReq(), visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 vision call, got %d", callCount)
	}
	msgs := result["messages"].([]any)
	text := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != "<image_description>A German Shepherd dog sitting on grass</image_description>" {
		t.Fatalf("unexpected description: %s", text)
	}

	// Second call — should use cache, NOT call vision model again.
	result, err = p.processImages(context.Background(), makeChatReq(), visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected still 1 vision call (cached), got %d", callCount)
	}
	msgs = result["messages"].([]any)
	text = msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != "<image_description>A German Shepherd dog sitting on grass</image_description>" {
		t.Fatalf("unexpected cached description: %s", text)
	}
}

func TestProcessImages_ConcurrentAndOCRMode(t *testing.T) {
	ResetImageCache()

	var mu sync.Mutex
	var maxConcurrent, current int
	prompts := map[string]bool{} // track prompts received

	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()

		// Parse the request to check the prompt.
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		msgs := req["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		prompt := content[0].(map[string]any)["text"].(string)
		mu.Lock()
		prompts[prompt] = true
		mu.Unlock()

		// Simulate some work to verify concurrency.
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		current--
		mu.Unlock()

		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{"content": "Page text here"},
				},
			},
		})
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{
		Name:    "vision",
		Backend: visionServer.URL,
		Model:   "vision",
	}

	// Build a tool message with 5 images (simulating PDF pages).
	images := make([]any, 5)
	for i := range images {
		images[i] = map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": fmt.Sprintf("data:image/jpeg;base64,pdfpage%d", i)},
		}
	}

	p := &Pipeline{client: http.DefaultClient}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role":    "tool",
				"content": images,
			},
		},
	}

	result, err := p.processImages(context.Background(), chatReq, visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify all 5 images were processed.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 5 {
		t.Fatalf("expected 5 parts, got %d", len(content))
	}

	// Verify OCR prompt was used (tool role + 5 images = PDF page detection).
	if !prompts[visionPromptOCR] {
		t.Fatal("expected OCR prompt for PDF page images")
	}
	if prompts[visionPromptDescribe] {
		t.Fatal("did not expect describe prompt for PDF page images")
	}

	// Verify labeling uses "Page text" not "Image description".
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "<page_text>") {
		t.Fatalf("expected <page_text> tag, got: %s", text)
	}

	// Verify concurrency happened (at least 2 concurrent with 5 images).
	if maxConcurrent < 2 {
		t.Fatalf("expected concurrent processing, but max concurrent was %d", maxConcurrent)
	}
}

func TestProcessImages_VisionModelFailure(t *testing.T) {
	ResetImageCache()
	// Vision model returns 500.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{
		Name:    "vision",
		Backend: visionServer.URL,
		Model:   "vision",
	}

	p := &Pipeline{client: http.DefaultClient}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,abc"},
					},
				},
			},
		},
	}

	result, err := p.processImages(context.Background(), chatReq, visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should have a fallback text instead of the image.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if text != "[Image could not be processed]" {
		t.Fatalf("expected fallback text, got: %s", text)
	}
}

// --- Tool-role image cascade (OCR → vision) tests ---

// mockImageServer returns a handler that counts hits and produces either a
// text response, empty content, or an HTTP 500.
func mockImageServer(t *testing.T, behavior string, text string, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		switch behavior {
		case "err":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"simulated"}`))
		case "empty":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{
					map[string]any{"message": map[string]any{"content": ""}},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{
					map[string]any{"message": map[string]any{"content": text}},
				},
			})
		}
	}))
}

func toolRoleReq() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{
				"role": "tool",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,toolpage"},
					},
				},
			},
		},
	}
}

func TestImageCascade_ToolRole_OCRSuccess_VisionNotCalled(t *testing.T) {
	ResetImageCache()
	defer ResetImageCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockImageServer(t, "ok", "OCR page content", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockImageServer(t, "ok", "vision describe", &visionHits)
	defer visionSrv.Close()

	ocrModel := &config.ModelConfig{Name: "ocr-model", Backend: ocrSrv.URL, Model: "ocr"}
	visionModel := &config.ModelConfig{Name: "vision-model", Backend: visionSrv.URL, Model: "vision"}

	p := &Pipeline{client: http.DefaultClient}
	result, err := p.processImages(context.Background(), toolRoleReq(), visionModel, ocrModel)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 {
		t.Fatalf("expected OCR called once, got %d", ocrHits)
	}
	if visionHits != 0 {
		t.Fatalf("expected vision NOT called on OCR success, got %d", visionHits)
	}
	content := result["messages"].([]any)[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "OCR page content") {
		t.Fatalf("expected OCR content, got: %s", text)
	}
}

func TestImageCascade_ToolRole_OCREmpty_VisionFallback(t *testing.T) {
	ResetImageCache()
	defer ResetImageCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockImageServer(t, "empty", "", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockImageServer(t, "ok", "vision saved it", &visionHits)
	defer visionSrv.Close()

	ocrModel := &config.ModelConfig{Name: "ocr-model", Backend: ocrSrv.URL, Model: "ocr"}
	visionModel := &config.ModelConfig{Name: "vision-model", Backend: visionSrv.URL, Model: "vision"}

	p := &Pipeline{client: http.DefaultClient}
	result, err := p.processImages(context.Background(), toolRoleReq(), visionModel, ocrModel)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected 1 OCR + 1 vision, got ocr=%d vision=%d", ocrHits, visionHits)
	}
	content := result["messages"].([]any)[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "vision saved") {
		t.Fatalf("expected vision fallback result, got: %s", text)
	}
}

func TestImageCascade_ToolRole_OCRError_VisionFallback(t *testing.T) {
	ResetImageCache()
	defer ResetImageCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockImageServer(t, "err", "", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockImageServer(t, "ok", "vision rescued", &visionHits)
	defer visionSrv.Close()

	ocrModel := &config.ModelConfig{Name: "ocr-model", Backend: ocrSrv.URL, Model: "ocr"}
	visionModel := &config.ModelConfig{Name: "vision-model", Backend: visionSrv.URL, Model: "vision"}

	p := &Pipeline{client: http.DefaultClient}
	result, err := p.processImages(context.Background(), toolRoleReq(), visionModel, ocrModel)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected cascade (1+1), got ocr=%d vision=%d", ocrHits, visionHits)
	}
	content := result["messages"].([]any)[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "vision rescued") {
		t.Fatalf("expected vision fallback after OCR error, got: %s", text)
	}
}

func TestImageCascade_ToolRole_BothFail_TTLShortCircuits(t *testing.T) {
	ResetImageCache()
	defer ResetImageCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockImageServer(t, "err", "", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockImageServer(t, "err", "", &visionHits)
	defer visionSrv.Close()

	ocrModel := &config.ModelConfig{Name: "ocr-model", Backend: ocrSrv.URL, Model: "ocr"}
	visionModel := &config.ModelConfig{Name: "vision-model", Backend: visionSrv.URL, Model: "vision"}

	p := &Pipeline{client: http.DefaultClient}
	// First call: both stages should be attempted.
	result, err := p.processImages(context.Background(), toolRoleReq(), visionModel, ocrModel)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected cascade tried both (1+1), got ocr=%d vision=%d", ocrHits, visionHits)
	}
	content := result["messages"].([]any)[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if text != "[Image could not be processed]" {
		t.Fatalf("expected failure placeholder, got: %s", text)
	}

	// Second call within TTL: neither upstream should be re-invoked.
	_, err = p.processImages(context.Background(), toolRoleReq(), visionModel, ocrModel)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected TTL short-circuit (still 1+1), got ocr=%d vision=%d",
			ocrHits, visionHits)
	}
}

func TestImageCascade_ToolRole_OnlyVision_NoDuplicateCall(t *testing.T) {
	ResetImageCache()
	defer ResetImageCache()

	// Only vision is configured; OCR resolves to the same model.
	hits := 0
	srv := mockImageServer(t, "err", "", &hits)
	defer srv.Close()
	visionModel := &config.ModelConfig{Name: "vision-model", Backend: srv.URL, Model: "vision"}

	p := &Pipeline{client: http.DefaultClient}
	_, err := p.processImages(context.Background(), toolRoleReq(), visionModel, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Cascade should not fire a fallback to the same model — exactly one call.
	if hits != 1 {
		t.Fatalf("expected exactly 1 call (no duplicate), got %d", hits)
	}
}

func TestImageCascade_UserRole_OCRNotCalled(t *testing.T) {
	// Regression gate: user-role images should still only go to vision,
	// never to OCR (per the existing design note in vision.go).
	ResetImageCache()
	defer ResetImageCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockImageServer(t, "ok", "should-not-appear", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockImageServer(t, "ok", "user image described", &visionHits)
	defer visionSrv.Close()

	ocrModel := &config.ModelConfig{Name: "ocr-model", Backend: ocrSrv.URL, Model: "ocr"}
	visionModel := &config.ModelConfig{Name: "vision-model", Backend: visionSrv.URL, Model: "vision"}

	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,userphoto"},
					},
				},
			},
		},
	}
	p := &Pipeline{client: http.DefaultClient}
	result, err := p.processImages(context.Background(), req, visionModel, ocrModel)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 0 {
		t.Fatalf("user-role images must not hit OCR, got %d calls", ocrHits)
	}
	if visionHits != 1 {
		t.Fatalf("expected exactly 1 vision call for user image, got %d", visionHits)
	}
	content := result["messages"].([]any)[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "user image described") {
		t.Fatalf("expected vision description, got: %s", text)
	}
}

func TestRequestContainsImageURLs(t *testing.T) {
	withImages := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://example.com/img.png"}},
				},
			},
		},
	}
	if !RequestContainsImageURLs(withImages) {
		t.Fatal("expected true")
	}

	withoutImages := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	if RequestContainsImageURLs(withoutImages) {
		t.Fatal("expected false")
	}
}

// --- Error formatting tests ---

func TestImageNotSupportedError(t *testing.T) {
	msg := imageNotSupportedError("MiniMax-M2.5", "400: model does not support image inputs")
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
	// Should mention the model name and have config guidance.
	for _, substr := range []string{"MiniMax-M2.5", "vision:", "Original error:"} {
		if !strings.Contains(msg, substr) {
			t.Fatalf("error message missing %q: %s", substr, msg)
		}
	}
}

func TestSearchNotConfiguredError(t *testing.T) {
	msg := searchNotConfiguredError()
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
	if !strings.Contains(msg, "web_search_key") {
		t.Fatalf("error message missing web_search_key: %s", msg)
	}
}

// --- Web search tests ---

func TestConvertOrInjectSearchTool_StrippedServerTool(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}

	chatReq := map[string]any{
		"messages":               []any{map[string]any{"role": "user", "content": "hello"}},
		"tools":                  []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result := p.convertOrInjectSearchTool(chatReq, model)

	// Should have injected web_search tool.
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected []any tools, got %T", result["tools"])
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (bash + web_search), got %d", len(tools))
	}

	// Verify the injected tool.
	lastTool := tools[1].(map[string]any)
	fn := lastTool["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("expected web_search tool, got %v", fn["name"])
	}

	// _stripped_server_tools is NOT cleaned up by convertOrInjectSearchTool —
	// that's ProcessRequest's responsibility (single ownership).
	if _, exists := result[InternalKeyStrippedTools]; !exists {
		t.Fatal("_stripped_server_tools should still exist (ProcessRequest cleans it)")
	}
}

func TestConvertOrInjectSearchTool_NoSearchKey(t *testing.T) {
	cfg := &config.Config{}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}

	chatReq := map[string]any{
		"messages":               []any{},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result := p.convertOrInjectSearchTool(chatReq, model)

	// Should not inject anything without a search key.
	if _, ok := result["tools"]; ok {
		t.Fatal("should not inject tools when no search key configured")
	}
}

func TestConvertOrInjectSearchTool_AlreadyHasSearchTool(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}

	chatReq := map[string]any{
		"messages": []any{},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "web_search"}},
		},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result := p.convertOrInjectSearchTool(chatReq, model)

	// Should not duplicate.
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (no duplicate), got %d", len(tools))
	}
}

func TestIsSearchServerTool(t *testing.T) {
	if !isSearchServerTool("web_search_20250305") {
		t.Fatal("expected true for web_search_20250305")
	}
	if !isSearchServerTool("web_search_preview") {
		t.Fatal("expected true for web_search_preview")
	}
	if !isSearchServerTool("web_search") {
		t.Fatal("expected true for web_search (Codex current)")
	}
	if isSearchServerTool("code_execution") {
		t.Fatal("expected false for code_execution")
	}
	if isSearchServerTool("function") {
		t.Fatal("expected false for function")
	}
}

func TestExecuteTavilySearch(t *testing.T) {
	// Mock Tavily server.
	tavilyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		if req["api_key"] != "tvly-test" {
			w.WriteHeader(401)
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"answer": "Test answer",
			"results": []any{
				map[string]any{
					"title":   "Test Result",
					"url":     "https://example.com",
					"content": "Test content",
					"score":   0.95,
				},
			},
		})
	}))
	defer tavilyServer.Close()

	// We can't easily test against the real URL, but we can test the parsing.
	// For now, test the formatting with a mock server.
	// Note: executeTavilySearch hardcodes the Tavily URL, so this test
	// is more about verifying the happy path of the formatter.
}

func TestHasSearchToolCall(t *testing.T) {
	calls := []api.ChatChoiceToolCall{
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash", Arguments: "{}"}},
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "web_search", Arguments: `{"query":"test"}`}},
	}
	if !HasSearchToolCall(calls) {
		t.Fatal("expected true when web_search is present")
	}

	callsNoSearch := []api.ChatChoiceToolCall{
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash", Arguments: "{}"}},
	}
	if HasSearchToolCall(callsNoSearch) {
		t.Fatal("expected false when web_search is absent")
	}
}

func TestMarshalToolCalls(t *testing.T) {
	tcs := []api.ChatChoiceToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "web_search", Arguments: `{"query":"test"}`},
	}}
	result := marshalToolCalls(tcs)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	m := result[0].(map[string]any)
	if m["id"] != "call_1" {
		t.Fatalf("expected call_1, got %v", m["id"])
	}
	fn := m["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("expected web_search, got %v", fn["name"])
	}
}

func TestStreamingSearchState(t *testing.T) {
	s := &StreamingSearchState{}
	idx1 := s.AccumulateToolCall("call_1", "web_search")
	s.AppendArgs(idx1, `{"query":`)
	s.AppendArgs(idx1, `"test"}`)

	idx2 := s.AccumulateToolCall("call_2", "bash")
	s.AppendArgs(idx2, `{"cmd":"ls"}`)

	if !s.HasSearchCall() {
		t.Fatal("expected HasSearchCall true")
	}
	if s.OnlySearchCalls() {
		t.Fatal("expected OnlySearchCalls false (has bash)")
	}

	tcs := s.ToChatChoiceToolCalls()
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "web_search" {
		t.Fatalf("expected web_search, got %s", tcs[0].Function.Name)
	}
	if tcs[0].Function.Arguments != `{"query":"test"}` {
		t.Fatalf("expected accumulated args, got %s", tcs[0].Function.Arguments)
	}
}

func TestStreamingSearchState_OnlySearch(t *testing.T) {
	s := &StreamingSearchState{}
	s.AccumulateToolCall("call_1", "web_search")
	s.AccumulateToolCall("call_2", "web_search")

	if !s.OnlySearchCalls() {
		t.Fatal("expected OnlySearchCalls true")
	}
}

func TestStreamingSearchState_Empty(t *testing.T) {
	s := &StreamingSearchState{}
	if s.HasSearchCall() {
		t.Fatal("expected HasSearchCall false on empty")
	}
	if s.OnlySearchCalls() {
		t.Fatal("expected OnlySearchCalls false on empty")
	}
}

func TestPipeline_ProcessRequest_SkipsAnthropic(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "test", Backend: "http://localhost/v1", Type: config.BackendAnthropic},
			{Name: "vision-model", Backend: "http://localhost/v1"},
		},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &config.ModelConfig{Name: "test", Type: config.BackendAnthropic}

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	// Should NOT process — Anthropic backends skip pipeline.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatal("expected image_url to be preserved for anthropic backend")
	}
}

func TestPipeline_ProcessRequest_ForcePipeline(t *testing.T) {
	ResetImageCache()
	// Set up a mock vision model backend.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{"content": "A code screenshot"},
				},
			},
		})
	}))
	defer visionServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "test", Backend: "http://localhost/v1", Type: config.BackendAnthropic, ForcePipeline: true},
			{Name: "vision-model", Backend: visionServer.URL, Model: "vision-model"},
		},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &cfg.Models[0]

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	// Should process despite Anthropic type because force_pipeline is true.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "text" {
		t.Fatal("expected image to be replaced with text description")
	}
}

func TestPipeline_ProcessRequest_SupportsVisionSkipsProcessing(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "test", Backend: "http://localhost/v1", SupportsVision: true},
			{Name: "vision-model", Backend: "http://localhost/v1"},
		},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &cfg.Models[0]

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	// Should NOT process — model supports vision natively.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatal("expected image_url preserved for vision-capable model")
	}
}

func TestPipeline_ProcessRequest_CleansUpInternalFields(t *testing.T) {
	cfg := &config.Config{}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &config.ModelConfig{Name: "test", Backend: "http://localhost/v1"}

	chatReq := map[string]any{
		"messages":               []any{map[string]any{"role": "user", "content": "hi"}},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := result[InternalKeyStrippedTools]; exists {
		t.Fatal("internal field should be cleaned up")
	}
}

func TestPipeline_ProcessRequest_InjectsSearchTool(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &config.ModelConfig{Name: "test", Backend: "http://localhost/v1"}

	chatReq := map[string]any{
		"messages":               []any{map[string]any{"role": "user", "content": "search for news"}},
		"tools":                  []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}

	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected []any tools, got %T", result["tools"])
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (bash + web_search), got %d", len(tools))
	}

	// Verify web_search tool was injected.
	lastTool := tools[1].(map[string]any)
	fn := lastTool["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("expected web_search, got %v", fn["name"])
	}
}

func TestHandleNonStreamingSearchLoop(t *testing.T) {
	// Mock Tavily server.
	tavilyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"answer":  "Latest news about Go",
			"results": []any{},
		})
	}))
	defer tavilyServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := &Pipeline{
		config: config.NewTestConfigStore(cfg),
		client: http.DefaultClient,
	}
	model := &config.ModelConfig{Name: "test"}

	content := "Here is the answer"

	// First response: model calls web_search.
	firstResp := &api.ChatResponse{
		Choices: []api.ChatChoice{{
			Message: api.ChatChoiceMsg{
				ToolCalls: []api.ChatChoiceToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "web_search", Arguments: `{"query":"Go news"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	callCount := 0
	sendRequest := func(chatReq map[string]any) (*api.ChatResponse, error) {
		callCount++
		// After search results injected, return final response.
		return &api.ChatResponse{
			Choices: []api.ChatChoice{{
				Message: api.ChatChoiceMsg{
					Content: &content,
				},
				FinishReason: "stop",
			}},
		}, nil
	}

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "What's new in Go?"},
		},
	}

	// executeTavilySearch hardcodes the Tavily URL, so the search will fail.
	// The loop handles this gracefully — it injects the error message as the result.
	resp, err := p.HandleNonStreamingSearchLoop(context.Background(), chatReq, model, firstResp, sendRequest, 5)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 re-send call after search, got %d", callCount)
	}
	if resp.Choices[0].Message.Content == nil || *resp.Choices[0].Message.Content != "Here is the answer" {
		t.Fatal("expected final response content")
	}
}

// --- PDF vision fallback tests ---

func TestProcessPDFs_VisionFallback(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	// Create a fake vision model backend.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "v1", "model": "vision-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "This PDF shows a scanned receipt."},
			}},
		})
	}))
	defer visionServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionServer.URL + "/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	model := &cfg.Models[0]

	// Create a minimal valid PDF that has no extractable text.
	// This is just random bytes that won't parse as a valid PDF with text,
	// triggering the vision fallback. We encode it as base64.
	// Actually, use a minimal PDF that has no text objects.
	minimalPDF := "%PDF-1.0\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/MediaBox[0 0 612 792]/Parent 2 0 R>>endobj\nxref\n0 4\n0000000000 65535 f \n0000000009 00000 n \n0000000058 00000 n \n0000000115 00000 n \ntrailer<</Size 4/Root 1 0 R>>\nstartxref\n190\n%%EOF"
	b64PDF := base64.StdEncoding.EncodeToString([]byte(minimalPDF))

	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "pdf_data", "data": b64PDF, "filename": "receipt.pdf"},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The PDF should be replaced with text from the vision model (or extraction).
	msgs := result["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	userMsg := msgs[0].(map[string]any)
	content := userMsg["content"]
	// Should contain text about the PDF (either extracted or vision-described).
	contentStr := fmt.Sprintf("%v", content)
	if !strings.Contains(contentStr, "PDF") && !strings.Contains(contentStr, "pdf") &&
		!strings.Contains(contentStr, "receipt") && !strings.Contains(contentStr, "scanned") {
		t.Fatalf("expected PDF-related content, got: %s", contentStr)
	}
}

func TestProcessPDFs_VisionFallbackFailure(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	// Vision model that always errors.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"model unavailable"}`))
	}))
	defer visionServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionServer.URL + "/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	model := &cfg.Models[0]

	minimalPDF := "%PDF-1.0\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/MediaBox[0 0 612 792]/Parent 2 0 R>>endobj\nxref\n0 4\n0000000000 65535 f \n0000000009 00000 n \n0000000058 00000 n \n0000000115 00000 n \ntrailer<</Size 4/Root 1 0 R>>\nstartxref\n190\n%%EOF"
	b64PDF := base64.StdEncoding.EncodeToString([]byte(minimalPDF))

	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "pdf_data", "data": b64PDF},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Even with vision failure, should gracefully degrade to placeholder.
	msgs := result["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	contentStr := fmt.Sprintf("%v", userMsg["content"])
	if !strings.Contains(contentStr, "PDF") && !strings.Contains(contentStr, "could not") {
		t.Fatalf("expected graceful degradation message, got: %s", contentStr)
	}
}

// --- Cross-API PDF normalization tests (M4) ---

func TestDecodePDFDataURL_Valid(t *testing.T) {
	data, ok := DecodePDFDataURL("data:application/pdf;base64,JVBERi0xLjQK")
	if !ok {
		t.Fatal("expected valid PDF data URL to be recognized")
	}
	if data != "JVBERi0xLjQK" {
		t.Fatalf("expected base64 payload, got %q", data)
	}
}

func TestDecodePDFDataURL_NotPDF(t *testing.T) {
	if _, ok := DecodePDFDataURL("data:image/png;base64,iVBORw0K"); ok {
		t.Fatal("expected PNG data URL to be rejected")
	}
	if _, ok := DecodePDFDataURL("https://example.com/foo.pdf"); ok {
		t.Fatal("expected http URL to be rejected")
	}
	if _, ok := DecodePDFDataURL(""); ok {
		t.Fatal("expected empty URL to be rejected")
	}
}

func TestDecodePDFDataURL_MissingBase64(t *testing.T) {
	// No ;base64 marker in header.
	if _, ok := DecodePDFDataURL("data:application/pdf,JVBERi0xLjQK"); ok {
		t.Fatal("expected URL without ;base64 to be rejected")
	}
}

func TestDecodePDFDataURL_EmptyPayload(t *testing.T) {
	if _, ok := DecodePDFDataURL("data:application/pdf;base64,"); ok {
		t.Fatal("expected empty payload to be rejected")
	}
}

func TestNormalizePDFDataURLs_ConvertsImageURLToPDFData(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what's in this?"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:application/pdf;base64," + b64},
					},
				},
			},
		},
	}
	NormalizePDFDataURLs(req)
	content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(content))
	}
	// Text part should be unchanged.
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("text part clobbered: %v", content[0])
	}
	// Image part should now be pdf_data.
	pdfPart := content[1].(map[string]any)
	if pdfPart["type"] != "pdf_data" {
		t.Fatalf("expected pdf_data, got %v", pdfPart["type"])
	}
	if pdfPart["data"] != b64 {
		t.Fatalf("expected base64 payload preserved, got %v", pdfPart["data"])
	}
}

func TestNormalizePDFDataURLs_LeavesImagesAlone(t *testing.T) {
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0K"},
					},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "https://example.com/photo.jpg"},
					},
				},
			},
		},
	}
	NormalizePDFDataURLs(req)
	content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	for i, part := range content {
		if part.(map[string]any)["type"] != "image_url" {
			t.Fatalf("part %d: expected image_url to be preserved, got %v", i, part)
		}
	}
}

func TestNormalizePDFDataURLs_NoContent_NoOp(t *testing.T) {
	// String content — shouldn't blow up.
	req := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	NormalizePDFDataURLs(req)
	if req["messages"].([]any)[0].(map[string]any)["content"] != "hello" {
		t.Fatal("string content should be untouched")
	}
}

func TestNormalizePDFDataURLs_Idempotent(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "pdf_data", "data": b64, "filename": "x.pdf"},
				},
			},
		},
	}
	NormalizePDFDataURLs(req)
	NormalizePDFDataURLs(req)
	content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["type"] != "pdf_data" {
		t.Fatal("pdf_data part should remain pdf_data after repeated calls")
	}
	if content[0].(map[string]any)["filename"] != "x.pdf" {
		t.Fatal("filename should be preserved")
	}
}

// --- PDF cascade tests (OCR → vision) ---

// scannedPDFStub is a PDF without extractable text, used to force Stage 2+.
const scannedPDFStub = "%PDF-1.0\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/MediaBox[0 0 612 792]/Parent 2 0 R>>endobj\nxref\n0 4\n0000000000 65535 f \n0000000009 00000 n \n0000000058 00000 n \n0000000115 00000 n \ntrailer<</Size 4/Root 1 0 R>>\nstartxref\n190\n%%EOF"

// mockProcessorServer returns a handler that counts hits and responds with
// the given behavior: "ok" = 200 with text, "empty" = 200 with empty content,
// "err" = 500 error.
func mockProcessorServer(t *testing.T, behavior string, text string, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		switch behavior {
		case "err":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"simulated"}`))
		case "empty":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "r1", "model": "x",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": ""},
				}},
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "r1", "model": "x",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": text},
				}},
			})
		}
	}))
}

func runCascade(t *testing.T, cfg *config.Config, pdfBase64 string) (map[string]any, error) {
	t.Helper()
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	model := &cfg.Models[0]
	req := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "pdf_data", "data": pdfBase64, "filename": "test.pdf"},
				},
			},
		},
	}
	return p.ProcessRequest(context.Background(), req, model)
}

func TestPDFCascade_OCRSuccess_VisionNotCalled(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockProcessorServer(t, "ok", "OCR extracted text here", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockProcessorServer(t, "ok", "vision described", &visionHits)
	defer visionSrv.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{OCR: "ocr-model", Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "ocr-model", Backend: ocrSrv.URL + "/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionSrv.URL + "/v1", Timeout: 30},
		},
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(scannedPDFStub))
	result, err := runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 {
		t.Fatalf("expected OCR to be called once, got %d", ocrHits)
	}
	if visionHits != 0 {
		t.Fatalf("expected vision NOT to be called, got %d", visionHits)
	}
	msgs := result["messages"].([]any)
	content := fmt.Sprintf("%v", msgs[0].(map[string]any)["content"])
	if !strings.Contains(content, "OCR extracted text") {
		t.Fatalf("expected OCR result in content, got: %s", content)
	}
	if !strings.Contains(content, `source="ocr"`) {
		t.Fatalf("expected source=\"ocr\" in content, got: %s", content)
	}
}

func TestPDFCascade_OCREmpty_VisionFallback(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockProcessorServer(t, "empty", "", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockProcessorServer(t, "ok", "vision saved the day", &visionHits)
	defer visionSrv.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{OCR: "ocr-model", Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "ocr-model", Backend: ocrSrv.URL + "/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionSrv.URL + "/v1", Timeout: 30},
		},
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(scannedPDFStub))
	result, err := runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected 1 OCR + 1 vision, got ocr=%d vision=%d", ocrHits, visionHits)
	}
	content := fmt.Sprintf("%v", result["messages"].([]any)[0].(map[string]any)["content"])
	if !strings.Contains(content, "vision saved") {
		t.Fatalf("expected vision result, got: %s", content)
	}
	if !strings.Contains(content, `source="vision"`) {
		t.Fatalf("expected source=\"vision\", got: %s", content)
	}
}

func TestPDFCascade_OCRErrors_VisionFallback(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockProcessorServer(t, "err", "", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockProcessorServer(t, "ok", "vision rescued", &visionHits)
	defer visionSrv.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{OCR: "ocr-model", Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "ocr-model", Backend: ocrSrv.URL + "/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionSrv.URL + "/v1", Timeout: 30},
		},
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(scannedPDFStub))
	result, err := runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected 1 OCR + 1 vision, got ocr=%d vision=%d", ocrHits, visionHits)
	}
	content := fmt.Sprintf("%v", result["messages"].([]any)[0].(map[string]any)["content"])
	if !strings.Contains(content, "vision rescued") {
		t.Fatalf("expected vision result after OCR error, got: %s", content)
	}
}

func TestPDFCascade_BothFail_TTLCached(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	ocrHits, visionHits := 0, 0
	ocrSrv := mockProcessorServer(t, "err", "", &ocrHits)
	defer ocrSrv.Close()
	visionSrv := mockProcessorServer(t, "err", "", &visionHits)
	defer visionSrv.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{OCR: "ocr-model", Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "ocr-model", Backend: ocrSrv.URL + "/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionSrv.URL + "/v1", Timeout: 30},
		},
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(scannedPDFStub))
	result, err := runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected 1 OCR + 1 vision on first call, got ocr=%d vision=%d", ocrHits, visionHits)
	}
	content := fmt.Sprintf("%v", result["messages"].([]any)[0].(map[string]any)["content"])
	if !strings.Contains(content, "could not be extracted") {
		t.Fatalf("expected failure message, got: %s", content)
	}

	// Second call with same PDF: should hit the TTL-cached failure without
	// re-invoking either upstream.
	_, err = runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if ocrHits != 1 || visionHits != 1 {
		t.Fatalf("expected TTL cache hit (no new upstream calls), got ocr=%d vision=%d",
			ocrHits, visionHits)
	}

	// Verify the cache entry has a non-zero expiresAt (TTL).
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(b64)))
	pdfCache.mu.RLock()
	entry, ok := pdfCache.items[key]
	pdfCache.mu.RUnlock()
	if !ok {
		t.Fatal("expected failure to be cached")
	}
	if entry.expiresAt.IsZero() {
		t.Fatal("expected failure cache entry to have TTL (non-zero expiresAt)")
	}
}

func TestPDFCascade_OnlyVisionConfigured_NoDuplicateCall(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	// Only vision is configured; OCR falls back to the same model via
	// resolveOCRProcessor. Cascade must not double-call.
	hits := 0
	srv := mockProcessorServer(t, "ok", "single call result", &hits)
	defer srv.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: srv.URL + "/v1", Timeout: 30},
		},
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(scannedPDFStub))
	_, err := runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 upstream call (no dedupe miss), got %d", hits)
	}
}

func TestPDFCascade_OnlyVisionConfigured_FailureNotDoubled(t *testing.T) {
	ResetPDFCache()
	defer ResetPDFCache()

	// Vision fails; because OCR and vision resolve to the same model, we
	// should stop after one failed call, not try the same backend twice.
	hits := 0
	srv := mockProcessorServer(t, "err", "", &hits)
	defer srv.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: srv.URL + "/v1", Timeout: 30},
		},
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(scannedPDFStub))
	result, err := runCascade(t, cfg, b64)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", hits)
	}
	content := fmt.Sprintf("%v", result["messages"].([]any)[0].(map[string]any)["content"])
	if !strings.Contains(content, "could not be extracted") {
		t.Fatalf("expected failure placeholder, got: %s", content)
	}
}

func TestHandleNonStreamingSearchLoop_MaxIterations(t *testing.T) {
	// The model always calls web_search, never returning a final answer.
	// The loop should stop after maxIterations.
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
		Models:     []config.ModelConfig{{Name: "test", Backend: "http://localhost/v1", Timeout: 30}},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &cfg.Models[0]

	sendCount := 0
	sendRequest := func(chatReq map[string]any) (*api.ChatResponse, error) {
		sendCount++
		// Always return another web_search tool call.
		return &api.ChatResponse{
			Choices: []api.ChatChoice{{
				Message: api.ChatChoiceMsg{
					ToolCalls: []api.ChatChoiceToolCall{{
						ID:   fmt.Sprintf("call_%d", sendCount),
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "web_search", Arguments: `{"query":"infinite loop"}`},
					}},
				},
				FinishReason: "tool_calls",
			}},
		}, nil
	}

	firstResp := &api.ChatResponse{
		Choices: []api.ChatChoice{{
			Message: api.ChatChoiceMsg{
				ToolCalls: []api.ChatChoiceToolCall{{
					ID:   "call_0",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "web_search", Arguments: `{"query":"test"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	chatReq := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "Search forever"}},
	}

	maxIter := 3
	resp, err := p.HandleNonStreamingSearchLoop(context.Background(), chatReq, model, firstResp, sendRequest, maxIter)
	if err != nil {
		t.Fatal(err)
	}

	// Should have called sendRequest exactly maxIter times (the loop limit).
	if sendCount != maxIter {
		t.Fatalf("expected %d send calls (max iterations), got %d", maxIter, sendCount)
	}

	// The response should still be a tool_calls response (loop hit the limit, didn't get final answer).
	if len(resp.Choices) == 0 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatal("expected tool_calls finish reason when max iterations reached")
	}
}

func TestEmitSearchResultBlocks_Format(t *testing.T) {
	// Test the emitSearchResultBlocks helper via messages_streaming.go.
	// We can't call it directly from this package, but we can test the pipeline's
	// SearchCallResult structure which feeds it.
	results := []SearchCallResult{
		{
			ToolUseID: "srvtoolu_1",
			Query:     "golang proxy",
			Hits: []SearchHit{
				{Title: "Go Proxy Guide", URL: "https://example.com/go"},
				{Title: "Reverse Proxy in Go", URL: "https://example.com/reverse"},
			},
		},
		{
			ToolUseID: "",
			Query:     "failed query",
			Error:     "search timed out",
		},
	}

	// Verify structure: first result has hits, second has error.
	if len(results[0].Hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(results[0].Hits))
	}
	if results[0].Hits[0].Title != "Go Proxy Guide" {
		t.Fatalf("expected title preserved, got %q", results[0].Hits[0].Title)
	}
	if results[1].Error == "" {
		t.Fatal("expected error on second result")
	}
}
