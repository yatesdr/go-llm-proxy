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

func TestResolveDocumentURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.4 fake"))
		case "/a.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("plain words"))
		case "/a.bin":
			w.Header().Set("Content-Type", "application/zip")
			w.Write([]byte("PK"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	// The default client refuses loopback (SSRF guard).
	blocked := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "document_url", "url": srv.URL + "/a.txt"}}}}}
	resolveDocumentURLs(t.Context(), blocked)
	part := blocked["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "text" || !strings.Contains(part["text"].(string), "could not be fetched") {
		t.Fatalf("loopback fetch should be refused: %v", part)
	}

	// The pre-flight rejects literal private IPs; reach the server by name.
	base := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	old := documentHTTPClient
	documentHTTPClient = http.DefaultClient
	defer func() { documentHTTPClient = old }()

	req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "document_url", "url": base + "/a.pdf", "filename": "a.pdf"},
		map[string]any{"type": "document_url", "url": base + "/a.txt"},
		map[string]any{"type": "document_url", "url": base + "/a.bin"},
		map[string]any{"type": "document_url", "url": base + "/missing"},
	}}}}
	resolveDocumentURLs(t.Context(), req)
	parts := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if p := parts[0].(map[string]any); p["type"] != "pdf_data" || p["filename"] != "a.pdf" || p["data"] == "" {
		t.Fatalf("pdf: %v", p)
	}
	if p := parts[1].(map[string]any); p["type"] != "text" || !strings.HasSuffix(p["text"].(string), "plain words") {
		t.Fatalf("text: %v", p)
	}
	if p := parts[2].(map[string]any); !strings.Contains(p["text"].(string), "unsupported document type") {
		t.Fatalf("bin: %v", p)
	}
	if p := parts[3].(map[string]any); !strings.Contains(p["text"].(string), "HTTP 404") {
		t.Fatalf("404: %v", p)
	}
}
