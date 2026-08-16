package pipeline

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

func TestTryPDFViaDocumentProcessorUsesMarkdownContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"file":"`) {
			t.Fatalf("missing base64 file field: %s", body)
		}
		if !strings.Contains(string(body), `"fileType":0`) {
			t.Fatalf("PDF fileType was not declared: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"errorCode":0,"result":{"layoutParsingResults":[{"markdown":{"text":"| A | B |\n|---|---|\n| 1 | 2 |"}}]}}`)
	}))
	defer server.Close()

	doc := &config.PaddleOCRConfig{Endpoint: "/layout-parsing", Timeout: 10, Backends: []config.BackendConfig{{URL: server.URL}}}
	cs := config.NewTestConfigStore(&config.Config{Documents: config.DocumentsConfig{PaddleOCR: doc}})
	p := NewPipeline(cs, httputil.NewHTTPClient())
	result, ok := p.tryPDFViaDocumentProcessor(t.Context(), doc, []byte("%PDF-test"), "test.pdf")
	if !ok || !strings.Contains(result, "| 1 | 2 |") || !strings.Contains(result, `source="layout"`) {
		t.Fatalf("unexpected result ok=%v: %s", ok, result)
	}
}
