package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/pipeline"
)

// --- helpers ---

// textPDF builds a small valid PDF whose single page contains text.
func textPDF(text string) []byte {
	var buf bytes.Buffer
	offsets := []int{}
	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	buf.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	stream := fmt.Sprintf("BT /F1 12 Tf 72 700 Td (%s) Tj ET", text)
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return buf.Bytes()
}

type capturedUpstream struct {
	mu   sync.Mutex
	body map[string]any
	raw  []byte
}

func (c *capturedUpstream) set(raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw = raw
	c.body = nil
	json.Unmarshal(raw, &c.body)
}

func simpleChatResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "model": "test-model",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content}}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13},
	})
}

// newCompatHandler builds a handler for a single configured model with an
// optional pipeline. upstream receives every backend request.
func newCompatHandler(t *testing.T, mc config.ModelConfig, withPipeline bool, upstream http.HandlerFunc) (*MessagesHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)
	mc.Name = "test-model"
	mc.Model = "test-model"
	mc.Backend = ts.URL + "/v1"
	mc.APIKey = "backend-secret"
	if mc.Timeout == 0 {
		mc.Timeout = 10
	}
	cfg := &config.Config{Models: []config.ModelConfig{mc}}
	cs := config.NewTestConfigStore(cfg)
	var pl *pipeline.Pipeline
	if withPipeline {
		pl = pipeline.NewPipeline(cs, http.DefaultClient)
	}
	return NewMessagesHandler(cs, nil, pl), ts
}

func postMessages(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func upstreamMessages(t *testing.T, c *capturedUpstream) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body == nil {
		t.Fatal("no upstream request captured")
	}
	var out []map[string]any
	for _, m := range c.body["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func partTypes(content any) []string {
	var types []string
	if parts, ok := content.([]any); ok {
		for _, p := range parts {
			types = append(types, p.(map[string]any)["type"].(string))
		}
	}
	return types
}

// --- PDF / documents ---

func TestCompat_DocumentOnVisionModelBecomesText(t *testing.T) {
	pdf := textPDF("Quarterly revenue was twelve million dollars for the reporting period")
	var cap capturedUpstream
	h, _ := newCompatHandler(t, config.ModelConfig{SupportsVision: true}, true, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		simpleChatResponse(w, "ok")
	})
	body := fmt.Sprintf(`{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":[
		{"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":%q}},
		{"type":"text","text":"summarise"}]}]}`, base64.StdEncoding.EncodeToString(pdf))
	w := postMessages(h, body)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(cap.raw, []byte("pdf_data")) {
		t.Fatalf("pdf_data leaked upstream: %s", cap.raw)
	}
	if !bytes.Contains(cap.raw, []byte("twelve million dollars")) {
		t.Fatalf("expected extracted PDF text upstream: %s", cap.raw)
	}
}

func TestCompat_DocumentWithRewriteOffBecomesFilePart(t *testing.T) {
	for _, withPipeline := range []bool{true, false} {
		var cap capturedUpstream
		h, _ := newCompatHandler(t, config.ModelConfig{SupportsVision: true, RewriteDocuments: "off"}, withPipeline,
			func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				cap.set(raw)
				simpleChatResponse(w, "ok")
			})
		body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":[
			{"type":"document","title":"a.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="}}]}]}`
		if w := postMessages(h, body); w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if bytes.Contains(cap.raw, []byte("pdf_data")) {
			t.Fatalf("pdf_data leaked (pipeline=%v): %s", withPipeline, cap.raw)
		}
		msgs := upstreamMessages(t, &cap)
		parts := msgs[0]["content"].([]any)
		file := parts[0].(map[string]any)
		if file["type"] != "file" {
			t.Fatalf("expected file part, got %v", file)
		}
		fd := file["file"].(map[string]any)
		if fd["file_data"] != "data:application/pdf;base64,JVBERi0xLjQ=" || fd["filename"] != "a.pdf" {
			t.Fatalf("bad file part: %v", fd)
		}
	}
}

func TestCompat_TextDocumentSources(t *testing.T) {
	msgs, _ := translateAnthropicMessages([]json.RawMessage{json.RawMessage(`{"role":"user","content":[
		{"type":"document","title":"Notes","context":"from the wiki","citations":{"enabled":true},"cache_control":{"type":"ephemeral"},
		 "source":{"type":"text","media_type":"text/plain","data":"alpha beta"}},
		{"type":"document","source":{"type":"content","content":[{"type":"text","text":"inner one"},{"type":"text","text":"inner two"}]}},
		{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}]}`)})
	parts := msgs[0]["content"].([]any)
	first := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(first, "[Document: Notes]") || !strings.Contains(first, "from the wiki") || !strings.HasSuffix(first, "alpha beta") {
		t.Fatalf("text document not inlined: %q", first)
	}
	if parts[1].(map[string]any)["text"] != "inner one" || parts[2].(map[string]any)["text"] != "inner two" {
		t.Fatalf("content document not inlined: %v", parts)
	}
	if parts[3].(map[string]any)["type"] != "document_url" {
		t.Fatalf("url document should become document_url: %v", parts[3])
	}
}

// --- tool turn order and tool_result media ---

func TestCompat_ToolResultsPrecedeUserContent(t *testing.T) {
	var cap capturedUpstream
	h, _ := newCompatHandler(t, config.ModelConfig{SupportsVision: true}, true, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		simpleChatResponse(w, "ok")
	})
	pdf := base64.StdEncoding.EncodeToString(textPDF("An embedded attachment that has enough text to be extracted by the proxy pipeline."))
	body := fmt.Sprintf(`{"model":"test-model","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"file read"},
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":%q}},
			{"type":"text","text":"now summarise"}]}]}`, pdf)
	if w := postMessages(h, body); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	msgs := upstreamMessages(t, &cap)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user" {
		t.Fatalf("expected tool message before user content, roles=%v", roles)
	}
	last := fmt.Sprint(msgs[3]["content"])
	if !strings.Contains(last, "now summarise") || !strings.Contains(last, "embedded attachment") {
		t.Fatalf("user turn lost content: %s", last)
	}
}

func TestCompat_ToolResultImageReachesUpstream(t *testing.T) {
	var cap capturedUpstream
	h, _ := newCompatHandler(t, config.ModelConfig{SupportsVision: true}, true, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		simpleChatResponse(w, "ok")
	})
	body := `{"model":"test-model","max_tokens":100,"messages":[
		{"role":"user","content":"look"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"Screenshot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":[
			{"type":"text","text":"captured"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}]}`
	if w := postMessages(h, body); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	msgs := upstreamMessages(t, &cap)
	if msgs[2]["role"] != "tool" || msgs[2]["content"] != "captured" {
		t.Fatalf("bad tool message: %v", msgs[2])
	}
	if msgs[3]["role"] != "user" {
		t.Fatalf("expected a user message carrying the image, got %v", msgs[3])
	}
	types := partTypes(msgs[3]["content"])
	if strings.Join(types, ",") != "text,image_url" {
		t.Fatalf("expected label + image_url, got %v", types)
	}
	if !strings.Contains(fmt.Sprint(msgs[3]["content"]), "toolu_9") {
		t.Fatalf("label should name the tool use: %v", msgs[3]["content"])
	}
}

func TestCompat_ToolResultErrorFlag(t *testing.T) {
	msgs, _ := translateAnthropicMessages([]json.RawMessage{json.RawMessage(
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":"boom"}]}`)})
	if msgs[0]["content"] != "[tool error] boom" {
		t.Fatalf("got %v", msgs[0]["content"])
	}
}

// --- thinking / effort / structured output ---

func TestCompat_ThinkingDisabledAppliesConfiguredKwargs(t *testing.T) {
	var cap capturedUpstream
	mc := config.ModelConfig{ThinkingOff: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}}
	h, _ := newCompatHandler(t, mc, false, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		simpleChatResponse(w, "ok")
	})
	body := `{"model":"test-model","max_tokens":100,"thinking":{"type":"disabled"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`
	if w := postMessages(h, body); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	kw, _ := cap.body["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Fatalf("thinking-off kwargs missing: %v", cap.body)
	}
	if _, has := cap.body["reasoning_effort"]; has {
		t.Fatal("effort must not be sent when thinking is disabled")
	}

	// Enabled thinking: kwargs are not applied, effort maps through.
	body = `{"model":"test-model","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":2000},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`
	postMessages(h, body)
	if _, has := cap.body["chat_template_kwargs"]; has {
		t.Fatal("kwargs applied while thinking enabled")
	}
	if cap.body["reasoning_effort"] != "xhigh" {
		t.Fatalf("expected max -> xhigh, got %v", cap.body["reasoning_effort"])
	}
	if _, has := cap.body["thinking"]; has {
		t.Fatal("thinking must not be forwarded")
	}
}

func TestCompat_ThinkingOffDropsDefaultReasoningEffort(t *testing.T) {
	effort := "high"
	mc := config.ModelConfig{
		ThinkingOff: map[string]any{"thinking": false},
		Defaults:    &config.SamplingDefaults{ReasoningEffort: &effort},
	}
	chatReq := map[string]any{"max_completion_tokens": 64}
	applyModelOptions(chatReq, messagesRequest{Thinking: json.RawMessage(`{"type":"disabled"}`)}, &mc)
	if chatReq["thinking"] != false {
		t.Fatalf("missing thinking=false: %v", chatReq)
	}
	if _, has := chatReq["reasoning_effort"]; has {
		t.Fatalf("default reasoning_effort should be dropped: %v", chatReq)
	}
	// Config map must not be mutated by merges.
	if len(mc.ThinkingOff) != 1 {
		t.Fatal("config mutated")
	}
}

func TestCompat_SamplingDefaultsDoNotOverrideClient(t *testing.T) {
	temp, topP := 0.6, 0.95
	maxNew := 8000
	mc := config.ModelConfig{Defaults: &config.SamplingDefaults{Temperature: &temp, TopP: &topP, MaxNewTokens: &maxNew}}
	var cap capturedUpstream
	h, _ := newCompatHandler(t, mc, false, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		simpleChatResponse(w, "ok")
	})
	postMessages(h, `{"model":"test-model","max_tokens":500,"temperature":1,"messages":[{"role":"user","content":"hi"}]}`)
	if cap.body["temperature"] != float64(1) {
		t.Fatalf("client temperature overridden: %v", cap.body["temperature"])
	}
	if cap.body["top_p"] != 0.95 {
		t.Fatalf("default top_p not applied: %v", cap.body["top_p"])
	}
	if _, has := cap.body["max_tokens"]; has {
		t.Fatal("default max_new_tokens must not add max_tokens beside max_completion_tokens")
	}
	if cap.body["max_completion_tokens"] != float64(500) {
		t.Fatalf("max_completion_tokens: %v", cap.body["max_completion_tokens"])
	}
}

func TestCompat_OutputConfigJSONSchema(t *testing.T) {
	for _, field := range []string{
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}`,
		`"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}`,
	} {
		var req messagesRequest
		json.Unmarshal([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}],`+field+`}`), &req)
		chatReq, _ := buildChatRequestFromAnthropic(req, "m")
		rf, _ := chatReq["response_format"].(map[string]any)
		if rf["type"] != "json_schema" {
			t.Fatalf("%s: no response_format: %v", field, chatReq)
		}
		js := rf["json_schema"].(map[string]any)
		if js["name"] != "response" || js["strict"] != true || js["schema"] == nil {
			t.Fatalf("bad json_schema: %v", js)
		}
	}
}

func TestCompat_ThinkingHistoryReplayedAsReasoning(t *testing.T) {
	msgs, _ := translateAnthropicMessages([]json.RawMessage{json.RawMessage(
		`{"role":"assistant","content":[{"type":"thinking","thinking":"first thoughts","signature":"x"},{"type":"redacted_thinking","data":"zzz"},{"type":"text","text":"answer"}]}`)})
	m := msgs[0]
	if m["reasoning_content"] != "first thoughts" || m["reasoning"] != "first thoughts" || m["content"] != "answer" {
		t.Fatalf("unexpected assistant message: %v", m)
	}
}

func TestCompat_DisableParallelToolUse(t *testing.T) {
	var req messagesRequest
	json.Unmarshal([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"t","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`), &req)
	chatReq, _ := buildChatRequestFromAnthropic(req, "m")
	if chatReq["parallel_tool_calls"] != false || string(chatReq["tool_choice"].(json.RawMessage)) != `"auto"` {
		t.Fatalf("bad request: %v", chatReq)
	}
}

func TestCompat_ClampMaxTokens(t *testing.T) {
	model := &config.ModelConfig{Name: "m", ContextWindow: 10000}
	body := []byte(strings.Repeat("abcd ", 4000)) // ~5000 tokens
	req := map[string]any{"max_completion_tokens": 8000}
	if !clampMaxTokens(req, body, model) {
		t.Fatal("expected clamp")
	}
	if got := req["max_completion_tokens"].(int); got >= 5000 || got < 3000 {
		t.Fatalf("unexpected clamp %d", got)
	}
	// Window unknown: untouched.
	req = map[string]any{"max_completion_tokens": 8000}
	if clampMaxTokens(req, body, &config.ModelConfig{}) {
		t.Fatal("clamped without a known window")
	}
	// Prompt alone overflows: leave it to the backend error path.
	big := []byte(strings.Repeat("abcd ", 12000))
	req = map[string]any{"max_completion_tokens": 8000}
	if clampMaxTokens(req, big, model) {
		t.Fatal("must not clamp when no room remains")
	}
}

func TestCompat_EstimateSkipsBase64Media(t *testing.T) {
	img := strings.Repeat("A", 4*1024*1024)
	body := []byte(`{"messages":[{"image":"data:image/png;base64,` + img + `"}]}`)
	est := estimateRequestTokens(body)
	if est > 5000 {
		t.Fatalf("media counted as text: %d", est)
	}
	if est < 1000 {
		t.Fatalf("media not costed: %d", est)
	}
}

// --- errors ---

func TestCompat_ContextLengthRewrite(t *testing.T) {
	re := regexp.MustCompile(`prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)`)
	cases := []struct{ body, n, m string }{
		{`{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (35000 in the messages, 5000 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}}`, "35000", "32768"},
		{`{"error":{"message":"You passed 35000 input tokens and requested 5000 output tokens. However, the model's context length is only 32768 tokens, resulting in a maximum input length of 27768 tokens."}}`, "35000", "32768"},
		{`{"error":{"message":"Requested token count exceeds the model's maximum context length of 32768 tokens. You requested a total of 40000 tokens: 35000 tokens from the input messages and 5000 tokens for the completion."}}`, "35000", "32768"},
		{`{"error":{"message":"Input length (40001) exceeds the maximum allowed length (32768)."}}`, "40001", "32768"},
		{`{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error","n_prompt_tokens":50000,"n_ctx":32768}}`, "50000", "32768"},
	}
	for i, c := range cases {
		status, typ, msg := buildClientError(400, []byte(c.body), false)
		m := re.FindStringSubmatch(msg)
		if status != 400 || typ != "invalid_request_error" || m == nil || m[1] != c.n || m[2] != c.m {
			t.Errorf("case %d: status=%d type=%s msg=%q", i, status, typ, msg)
		}
	}
	// Prompt fits, completion overflows: shrink-max_tokens form.
	_, _, msg := buildClientError(400, []byte(`{"error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (30000 in the messages, 10000 in the completion)."}}`), false)
	if msg != "input length and `max_tokens` exceed context limit: 30000 + 10000 > 32768" {
		t.Fatalf("got %q", msg)
	}
}

func TestCompat_ScrubUpstreamMessage(t *testing.T) {
	got := scrubUpstreamMessage("failed calling http://10.1.2.3:8000/v1/chat with Bearer abcdefghijkl and api_key=sk-secretvalue123 on 192.168.0.5:9000")
	for _, bad := range []string{"10.1.2.3", "abcdefghijkl", "secretvalue", "192.168.0.5", "http://"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked in %q", bad, got)
		}
	}
}

func TestCompat_ErrorStatusMapping(t *testing.T) {
	cases := []struct {
		upstream, status int
		typ              string
	}{
		{400, 400, "invalid_request_error"}, {401, 502, "api_error"}, {403, 502, "api_error"},
		{404, 404, "not_found_error"}, {413, 413, "request_too_large"}, {429, 429, "rate_limit_error"},
		{500, 500, "api_error"}, {503, 503, "overloaded_error"}, {529, 529, "overloaded_error"},
	}
	for _, c := range cases {
		var gotBody []byte
		h, _ := newCompatHandler(t, config.ModelConfig{}, false, func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Retry-After", "7")
			w.Header().Set("Retry-After-Ms", "7000")
			w.Header().Set("X-Should-Retry", "true")
			w.WriteHeader(c.upstream)
			io.WriteString(w, `{"error":{"message":"upstream said no at http://10.0.0.1:8000"}}`)
		})
		_ = gotBody
		for _, stream := range []string{"false", "true"} {
			w := postMessages(h, `{"model":"test-model","max_tokens":10,"stream":`+stream+`,"messages":[{"role":"user","content":"x"}]}`)
			if w.Code != c.status {
				t.Fatalf("upstream %d stream=%s: got status %d", c.upstream, stream, w.Code)
			}
			var d struct {
				Error struct{ Type, Message string } `json:"error"`
			}
			json.Unmarshal(w.Body.Bytes(), &d)
			if d.Error.Type != c.typ {
				t.Fatalf("upstream %d: type %q want %q", c.upstream, d.Error.Type, c.typ)
			}
			if strings.Contains(d.Error.Message, "10.0.0.1") {
				t.Fatalf("message not scrubbed: %q", d.Error.Message)
			}
			if w.Header().Get("Retry-After") != "7" || w.Header().Get("Retry-After-Ms") != "7000" || w.Header().Get("X-Should-Retry") != "true" {
				t.Fatalf("retry headers not forwarded: %v", w.Header())
			}
		}
	}
}

func TestCompat_NativeErrorForwarding(t *testing.T) {
	cfg := &config.Config{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "0")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer ts.Close()
	cfg.Models = []config.ModelConfig{{Name: "test-model", Backend: ts.URL, Model: "test-model", Type: config.BackendAnthropic, Timeout: 10}}
	h := NewMessagesHandler(config.NewTestConfigStore(cfg), nil, nil)
	w := postMessages(h, `{"model":"test-model","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`)
	var d struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 429 || d.Error.Type != "rate_limit_error" || d.Error.Message != "slow down" {
		t.Fatalf("native error not forwarded: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "30" || w.Header().Get("Anthropic-Ratelimit-Requests-Remaining") != "0" {
		t.Fatalf("headers lost: %v", w.Header())
	}
}

// --- streaming robustness ---

func sseUpstream(chunks []string, done bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		if done {
			fmt.Fprint(w, "data: [DONE]\n\n")
			f.Flush()
		}
	}
}

func chunk(delta string, finish string) string {
	fr := "null"
	if finish != "" {
		fr = fmt.Sprintf("%q", finish)
	}
	return fmt.Sprintf(`{"id":"c","model":"test-model","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, fr)
}

func sseEventNames(events []sseEvent) []string {
	var n []string
	for _, e := range events {
		n = append(n, e.event)
	}
	return n
}

func TestCompat_TruncatedStreamEmitsError(t *testing.T) {
	cases := map[string]struct {
		chunks []string
		done   bool
	}{
		"no finish, no done": {[]string{chunk(`{"content":"par"}`, "")}, false},
		"no finish, done":    {[]string{chunk(`{"content":"par"}`, "")}, true},
		"empty stream":       {nil, false},
	}
	for name, c := range cases {
		h, _ := newCompatHandler(t, config.ModelConfig{}, false, sseUpstream(c.chunks, c.done))
		w := postMessages(h, `{"model":"test-model","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
		names := sseEventNames(parseSSEEvents(w.Body.String()))
		last := names[len(names)-1]
		if last != "error" {
			t.Errorf("%s: last event %q, events=%v", name, last, names)
		}
		for _, n := range names {
			if n == "message_stop" {
				t.Errorf("%s: message_stop must not be emitted", name)
			}
		}
		if !strings.Contains(w.Body.String(), "overloaded_error") {
			t.Errorf("%s: expected overloaded_error", name)
		}
	}
}

func TestCompat_CompleteStreamWithoutDoneIsAccepted(t *testing.T) {
	h, _ := newCompatHandler(t, config.ModelConfig{}, false,
		sseUpstream([]string{chunk(`{"content":"hi"}`, ""), chunk(`{}`, "stop")}, false))
	w := postMessages(h, `{"model":"test-model","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	names := sseEventNames(parseSSEEvents(w.Body.String()))
	if names[len(names)-1] != "message_stop" {
		t.Fatalf("events=%v", names)
	}
}

func TestCompat_EarlyMessageStartAndPings(t *testing.T) {
	old := messagesPingInterval
	messagesPingInterval = 20 * time.Millisecond
	defer func() { messagesPingInterval = old }()

	h, _ := newCompatHandler(t, config.ModelConfig{}, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // long prefill
		fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", chunk(`{"content":"late"}`, ""), chunk(`{}`, "stop"))
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"test-model","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	names := sseEventNames(parseSSEEvents(string(body)))
	if names[0] != "message_start" {
		t.Fatalf("message_start must be first: %v", names)
	}
	pingsBeforeContent := 0
	for _, n := range names {
		if n == "content_block_start" {
			break
		}
		if n == "ping" {
			pingsBeforeContent++
		}
	}
	if pingsBeforeContent < 5 {
		t.Fatalf("expected pings during the wait, got %d: %v", pingsBeforeContent, names)
	}
}

func TestCompat_StreamBlockIndicesUniqueAndBalanced(t *testing.T) {
	// text, then reasoning (after text), then two tool calls, one repeating its id on each delta.
	chunks := []string{
		chunk(`{"content":"hello"}`, ""),
		chunk(`{"reasoning_content":"hmm"}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"A","arguments":""}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"call_a","function":{"arguments":"{\"x\":"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"call_a","function":{"arguments":"1}"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"B","arguments":"{}"}}]}`, ""),
		chunk(`{}`, "stop"), // some backends say stop even with tool calls
	}
	h, _ := newCompatHandler(t, config.ModelConfig{}, false, sseUpstream(chunks, true))
	w := postMessages(h, `{"model":"test-model","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	started := map[int]string{}
	stopped := map[int]bool{}
	var stopReason string
	for _, e := range parseSSEEvents(w.Body.String()) {
		var d map[string]any
		json.Unmarshal([]byte(e.data), &d)
		switch e.event {
		case "content_block_start":
			idx := int(d["index"].(float64))
			if _, dup := started[idx]; dup {
				t.Fatalf("duplicate block index %d", idx)
			}
			started[idx] = d["content_block"].(map[string]any)["type"].(string)
		case "content_block_stop":
			idx := int(d["index"].(float64))
			if _, ok := started[idx]; !ok || stopped[idx] {
				t.Fatalf("unbalanced stop for %d", idx)
			}
			stopped[idx] = true
		case "message_delta":
			stopReason = d["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	if len(started) != 4 || len(stopped) != 4 {
		t.Fatalf("expected 4 balanced blocks, started=%v stopped=%v", started, stopped)
	}
	if started[0] != "text" || started[1] != "thinking" || started[2] != "tool_use" || started[3] != "tool_use" {
		t.Fatalf("unexpected block order: %v", started)
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason=%q, want tool_use", stopReason)
	}
}

func TestCompat_UsageCacheAndStopSequence(t *testing.T) {
	h, _ := newCompatHandler(t, config.ModelConfig{}, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "model": "test-model",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "stop_reason": "</block>",
				"message": map[string]any{"role": "assistant", "content": "<block>no"}}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 4, "total_tokens": 104,
				"prompt_tokens_details": map[string]any{"cached_tokens": 60}},
		})
	})
	w := postMessages(h, `{"model":"test-model","max_tokens":64,"stop_sequences":["</block>"],"messages":[{"role":"user","content":"x"}]}`)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(40) || u["cache_read_input_tokens"] != float64(60) {
		t.Fatalf("cache accounting: %v", u)
	}
	if resp["stop_reason"] != "stop_sequence" || resp["stop_sequence"] != "</block>" {
		t.Fatalf("stop: %v %v", resp["stop_reason"], resp["stop_sequence"])
	}
}

// --- aliases through the handler ---

func TestCompat_GlobAliasesRouteClaudeModelIDs(t *testing.T) {
	var cap capturedUpstream
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		simpleChatResponse(w, "ok")
	}))
	defer ts.Close()
	cfg := &config.Config{
		Models:  []config.ModelConfig{{Name: "test-model", Backend: ts.URL + "/v1", Model: "backend-id", Timeout: 10}},
		Aliases: map[string]string{"claude-*": "test-model", "claude-haiku-*": "test-model"},
	}
	h := NewMessagesHandler(config.NewTestConfigStore(cfg), nil, nil)
	for _, id := range []string{"claude-haiku-4-5-20251001", "claude-opus-5[1m]", "claude-sonnet-4-5"} {
		w := postMessages(h, `{"model":"`+id+`","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`)
		if w.Code != 200 {
			t.Fatalf("%s: status %d: %s", id, w.Code, w.Body.String())
		}
		if cap.body["model"] != "backend-id" {
			t.Fatalf("%s: upstream model %v", id, cap.body["model"])
		}
	}
}

// --- permission classifier (auto mode) end to end ---

func TestCompat_AutoModeClassifierRequest(t *testing.T) {
	var cap capturedUpstream
	mc := config.ModelConfig{
		SupportsVision: true,
		ThinkingOff:    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
	}
	h, _ := newCompatHandler(t, mc, true, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.set(raw)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "model": "test-model",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "stop_reason": "</block>",
				"message": map[string]any{"role": "assistant", "content": "<block>yes"}}},
			"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 3, "total_tokens": 903},
		})
	})
	body := `{"model":"test-model","max_tokens":64,"temperature":1,"thinking":{"type":"disabled"},
		"stop_sequences":["</block>"],
		"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],
		"messages":[{"role":"user","content":[{"type":"text","text":"<transcript>\nUser: delete build dir\nAssistant: Bash rm -rf build\n</transcript>\nShould this be blocked?","cache_control":{"type":"ephemeral"}}]}]}`
	w := postMessages(h, body)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	up := cap.body
	if up["max_completion_tokens"] != float64(64) || up["temperature"] != float64(1) {
		t.Fatalf("sampling not forwarded: %v", up)
	}
	if stops, _ := up["stop"].([]any); len(stops) != 1 || stops[0] != "</block>" {
		t.Fatalf("stop sequence lost: %v", up["stop"])
	}
	if kw, _ := up["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
		t.Fatalf("thinking-off kwargs missing: %v", up)
	}
	if up["stream"] != false {
		t.Fatalf("classifier is non-streaming: %v", up["stream"])
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	content := resp["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "<block>yes" {
		t.Fatalf("classifier text mangled: %v", resp["content"])
	}
	if resp["stop_reason"] != "stop_sequence" {
		t.Fatalf("stop_reason=%v", resp["stop_reason"])
	}
}

// --- count_tokens ---

func TestCompat_CountTokensNativeFallsBackOn404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"not found"}`)
	}))
	defer ts.Close()
	cfg := &config.Config{Models: []config.ModelConfig{{Name: "test-model", Backend: ts.URL, Model: "test-model", Type: config.BackendAnthropic, Timeout: 10}}}
	h := NewCountTokensHandler(config.NewTestConfigStore(cfg), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello hello hello hello"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var d map[string]any
	json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d["input_tokens"] == nil {
		t.Fatalf("expected estimate fallback, got %d %s", w.Code, w.Body.String())
	}
}
