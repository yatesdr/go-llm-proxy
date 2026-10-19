package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/lb"
)

// Tests for the Codex-facing Responses path. The fixtures under
// testdata/codex are real Codex 0.163 requests with long texts shortened and
// identifiers replaced.

func loadCodexFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fixtureBody(t *testing.T, name string, mutate func(map[string]any)) string {
	t.Helper()
	m := loadCodexFixture(t, name)
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// chatStub is a Chat Completions upstream that records request bodies and
// answers with a canned SSE stream (or any handler).
type chatStub struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *chatStub) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no chat request")
	}
	return c.bodies[len(c.bodies)-1]
}

func (c *chatStub) handler(chunks []string, done bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		c.mu.Lock()
		c.bodies = append(c.bodies, b)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ch := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", ch)
		}
		if done {
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}
}

func newCodexHandler(t *testing.T, upstream http.HandlerFunc, mutate func(*config.ModelConfig)) (*ResponsesHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	m := config.ModelConfig{
		Name: "test-model", Backend: ts.URL + "/v1", Model: "backend-model",
		Timeout: 10, ResponsesMode: "translate",
	}
	if mutate != nil {
		mutate(&m)
	}
	cfg := &config.Config{
		Models:  []config.ModelConfig{m},
		Aliases: map[string]string{"codex-auto-review": "test-model"},
	}
	return NewResponsesHandler(config.NewTestConfigStore(cfg), nil, nil), ts
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func chunk(delta string, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return fmt.Sprintf(`{"id":"c1","model":"backend-model","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, fr)
}

func eventsOf(w *httptest.ResponseRecorder, name string) []map[string]any {
	var out []map[string]any
	for _, e := range parseSSEEvents(w.Body.String()) {
		if e.event == name {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			out = append(out, d)
		}
	}
	return out
}

func hasEvent(w *httptest.ResponseRecorder, name string) bool { return len(eventsOf(w, name)) > 0 }

func chatMessages(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range body["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

// --- P0 1: developer / system handling ---

func TestCodexMainTurn_LeadingDeveloperMessagesMergeIntoOneSystemMessage(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"role":"assistant","content":"ok"}`, ""), chunk(`{}`, "stop"),
	}, true), nil)
	defer ts.Close()

	fx := loadCodexFixture(t, "main_turn")
	w := post(h, fixtureBody(t, "main_turn", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	// Expected bytes: both developer messages' text, joined by one blank line.
	input := fx["input"].([]any)
	var texts []string
	for _, i := range []int{0, 1} {
		for _, part := range input[i].(map[string]any)["content"].([]any) {
			texts = append(texts, part.(map[string]any)["text"].(string))
		}
	}
	want := strings.Join(texts, "\n\n")

	msgs := chatMessages(stub.last(t))
	if len(msgs) != 3 {
		t.Fatalf("expected system + 2 user messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != want {
		t.Fatalf("merged system message mismatch:\n got %q\nwant %q", msgs[0]["content"], want)
	}
	if msgs[1]["role"] != "user" || msgs[2]["role"] != "user" {
		t.Fatalf("expected user messages after the system message, got %v", msgs)
	}
	for _, m := range msgs[1:] {
		if m["role"] == "system" || m["role"] == "developer" {
			t.Fatalf("stray system/developer message: %v", m)
		}
	}
}

func TestTranslateInput_MergeInstructionsAndLeadingRunExactBytes(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"additional_tools","role":"developer","tools":[]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"B"},{"type":"input_text","text":"C"}]},
		{"type":"message","role":"system","content":"D"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]`)
	msgs, err := translateInput(input, "A")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(msgs)
	want := `[{"content":"A\n\nB\n\nC\n\nD","role":"system"},{"content":[{"text":"hi","type":"text"}],"role":"user"}]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	b2, _ := json.Marshal(mustTranslate(t, input, "A", translateOptions{}))
	if string(b2) != string(b) {
		t.Fatal("translation is not byte-stable")
	}
}

func mustTranslate(t *testing.T, input json.RawMessage, instr string, o translateOptions) []map[string]any {
	t.Helper()
	msgs, err := translateInputWith(input, instr, o)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestTranslateInput_LaterDeveloperRoleOption(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"developer","content":"base"},
		{"role":"user","content":"q"},
		{"role":"assistant","content":"a"},
		{"role":"developer","content":"later"}
	]`)
	cases := map[string][2]string{
		"":          {"user", "<developer>\nlater\n</developer>"},
		"user":      {"user", "<developer>\nlater\n</developer>"},
		"system":    {"system", "later"},
		"developer": {"developer", "later"},
	}
	for role, want := range cases {
		msgs := mustTranslate(t, input, "", translateOptions{DeveloperRole: role})
		if len(msgs) != 4 || msgs[0]["role"] != "system" || msgs[0]["content"] != "base" {
			t.Fatalf("%q: bad leading message: %v", role, msgs)
		}
		if msgs[3]["role"] != want[0] || msgs[3]["content"] != want[1] {
			t.Errorf("developer_role %q: got %v, want role=%s content=%q", role, msgs[3], want[0], want[1])
		}
	}
}

func TestResponsesHandler_DeveloperRoleFromModelConfig(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{chunk(`{"content":"x"}`, "stop")}, true),
		func(m *config.ModelConfig) { m.DeveloperRole = "system" })
	defer ts.Close()
	post(h, `{"model":"test-model","stream":true,"input":[
		{"role":"user","content":"q"},{"role":"developer","content":"later"}]}`)
	msgs := chatMessages(stub.last(t))
	if msgs[1]["role"] != "system" || msgs[1]["content"] != "later" {
		t.Fatalf("developer_role: system not honored: %v", msgs)
	}
}

// --- P0 2: truncated streams ---

func TestResponsesHandler_TruncatedStreamIsFailed(t *testing.T) {
	cases := map[string][]string{
		"no finish_reason": {chunk(`{"role":"assistant","content":"partial"}`, "")},
		"mid tool call":    {chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}`, "")},
	}
	for name, chunks := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &chatStub{}
			h, ts := newCodexHandler(t, stub.handler(chunks, false), nil)
			defer ts.Close()
			w := post(h, `{"model":"test-model","input":"hi","stream":true,"tools":[{"type":"function","name":"f","parameters":{}}]}`)
			if hasEvent(w, "response.completed") {
				t.Fatalf("truncated stream reported as completed:\n%s", w.Body.String())
			}
			failed := eventsOf(w, "response.failed")
			if len(failed) != 1 {
				t.Fatalf("expected one response.failed, got:\n%s", w.Body.String())
			}
			resp := failed[0]["response"].(map[string]any)
			errObj := resp["error"].(map[string]any)
			if errObj["code"] != "stream_truncated" || resp["status"] != "failed" {
				t.Fatalf("unexpected failure payload: %v", resp)
			}
			// A cut-off tool call must not be presented as a finished item.
			if hasEvent(w, "response.function_call_arguments.done") {
				t.Fatal("partial tool call was finalized")
			}
		})
	}
}

func TestResponsesHandler_StreamErrorObjectIsFailed(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"content":"hi"}`, ""),
		`{"error":{"message":"engine died at http://203.0.113.2:8000","code":500}}`,
	}, false), nil)
	defer ts.Close()
	w := post(h, `{"model":"test-model","input":"hi","stream":true}`)
	failed := eventsOf(w, "response.failed")
	if len(failed) != 1 || hasEvent(w, "response.completed") {
		t.Fatalf("expected only response.failed:\n%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "203.0.113.2") {
		t.Fatal("upstream address leaked")
	}
}

func TestResponsesHandler_MissingDoneAfterFinishReasonStillCompletes(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{chunk(`{"content":"hi"}`, ""), chunk(`{}`, "stop")}, false), nil)
	defer ts.Close()
	w := post(h, `{"model":"test-model","input":"hi","stream":true}`)
	if !hasEvent(w, "response.completed") || hasEvent(w, "response.failed") {
		t.Fatalf("expected completed:\n%s", w.Body.String())
	}
}

func TestResponsesHandler_StreamingLargeLine(t *testing.T) {
	big := strings.Repeat("x", 2*1024*1024)
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"content":"`+big+`"}`, ""), chunk(`{}`, "stop"),
	}, true), nil)
	defer ts.Close()
	w := post(h, `{"model":"test-model","input":"hi","stream":true}`)
	if !hasEvent(w, "response.completed") {
		t.Fatalf("a 2 MiB SSE line must be accepted; got failure: %.300s", w.Body.String())
	}
}

// --- P0 3: auto-review, namespaces, custom tools ---

func TestCodexAutoReview_AdditionalToolsBecomeChatToolsAndOutputFormat(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"role":"assistant","content":"{\"outcome\":\"allow\"}"}`, ""), chunk(`{}`, "stop"),
	}, true), nil)
	defer ts.Close()

	w := post(h, fixtureBody(t, "auto_review", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := stub.last(t)
	if body["model"] != "backend-model" {
		t.Errorf("model not rewritten: %v", body["model"])
	}

	// additional_tools -> tools: custom `exec` as a one-string-parameter function,
	// namespaced `wait` flattened with the default `functions` namespace dropped.
	tools := body["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, tl := range tools {
		fn := tl.(map[string]any)["function"].(map[string]any)
		byName[fn["name"].(string)] = fn
	}
	exec, ok := byName["exec"]
	if !ok || byName["wait"] == nil {
		t.Fatalf("expected exec and wait tools, got %v", byName)
	}
	params := exec["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["input"]; !ok || params["required"].([]any)[0] != "input" {
		t.Errorf("custom tool parameters wrong: %v", params)
	}
	if !strings.Contains(exec["description"].(string), "grammar") {
		t.Errorf("grammar missing from custom tool description")
	}

	// No developer/additional_tools message leaks; one leading system message.
	msgs := chatMessages(body)
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message must be system: %v", msgs[0])
	}
	for _, m := range msgs {
		if m["role"] == "developer" {
			t.Fatalf("developer role leaked: %v", m)
		}
	}
	if strings.Count(fmt.Sprint(msgs[0]["content"]), "<permissions instructions>") != 1 {
		t.Errorf("permissions developer message not merged into system prompt")
	}

	// text.format -> response_format; verbosity dropped; effort kept.
	rf := body["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "codex_output_schema" || js["strict"] != false {
		t.Errorf("response_format wrong: %v", rf)
	}
	if props := js["schema"].(map[string]any)["properties"].(map[string]any); props["outcome"] == nil {
		t.Errorf("schema lost: %v", js)
	}
	if body["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort: %v", body["reasoning_effort"])
	}
	for _, k := range []string{"verbosity", "text", "store", "include", "prompt_cache_key", "reasoning"} {
		if _, present := body[k]; present {
			t.Errorf("Responses-only field %q leaked into chat request", k)
		}
	}

	// The final message is a well-formed output_text message.
	completed := eventsOf(w, "response.completed")
	if len(completed) != 1 {
		t.Fatalf("no response.completed:\n%s", w.Body.String())
	}
	out := completed[0]["response"].(map[string]any)["output"].([]any)
	msg := out[len(out)-1].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("final output item should be a message: %v", msg)
	}
	text := msg["content"].([]any)[0].(map[string]any)["text"].(string)
	var verdict map[string]any
	if json.Unmarshal([]byte(text), &verdict) != nil || verdict["outcome"] != "allow" {
		t.Errorf("final text not parseable verdict: %q", text)
	}
}

func TestCodexAutoReview_FencedJSONIsCleaned(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
				fenced := "```json\\n{\\\"outcome\\\":\\\"deny\\\"}\\n```"
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", chunk(`{"content":"`+fenced[:12]+`"}`, ""))
					fmt.Fprintf(w, "data: %s\n\n", chunk(`{"content":"`+fenced[12:]+`"}`, ""))
					fmt.Fprintf(w, "data: %s\n\n", chunk(`{}`, "stop"))
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"%s"}}]}`, fenced)
			}, nil)
			defer ts.Close()

			w := post(h, fixtureBody(t, "auto_review", func(m map[string]any) { m["stream"] = stream }))
			var text string
			if stream {
				text = eventsOf(w, "response.completed")[0]["response"].(map[string]any)["output_text"].(string)
			} else {
				var r map[string]any
				json.Unmarshal(w.Body.Bytes(), &r)
				text = r["output_text"].(string)
			}
			if text != `{"outcome":"deny"}` {
				t.Fatalf("expected bare JSON, got %q", text)
			}
		})
	}
}

func TestCodexAutoReview_CustomToolCallEvents(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"exec","arguments":""}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"// @exec: {}\\nawait tools."}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"exec_command({cmd:'ls'})\"}"}}]}`, ""),
		chunk(`{}`, "tool_calls"),
	}, true), nil)
	defer ts.Close()

	w := post(h, fixtureBody(t, "auto_review", nil))
	if hasEvent(w, "response.failed") {
		t.Fatalf("failed: %s", w.Body.String())
	}
	added := eventsOf(w, "response.output_item.added")
	if len(added) != 1 {
		t.Fatalf("expected one added item: %s", w.Body.String())
	}
	item := added[0]["item"].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "exec" || item["namespace"] != "functions" || item["call_id"] != "call_x" {
		t.Fatalf("added item wrong: %v", item)
	}
	if len(eventsOf(w, "response.function_call_arguments.delta")) != 0 {
		t.Error("custom tool must not emit function_call_arguments events")
	}
	deltas := eventsOf(w, "response.custom_tool_call_input.delta")
	done := eventsOf(w, "response.custom_tool_call_input.done")
	wantInput := "// @exec: {}\nawait tools.exec_command({cmd:'ls'})"
	if len(deltas) != 1 || deltas[0]["delta"] != wantInput || len(done) != 1 || done[0]["input"] != wantInput {
		t.Fatalf("custom input events wrong: delta=%v done=%v", deltas, done)
	}
	if deltas[0]["item_id"] != item["id"] {
		t.Error("delta item_id mismatch")
	}
	fin := eventsOf(w, "response.output_item.done")
	last := fin[len(fin)-1]["item"].(map[string]any)
	if last["type"] != "custom_tool_call" || last["input"] != wantInput || last["status"] != "completed" {
		t.Fatalf("done item wrong: %v", last)
	}
	comp := eventsOf(w, "response.completed")[0]["response"].(map[string]any)["output"].([]any)
	if comp[0].(map[string]any)["type"] != "custom_tool_call" {
		t.Errorf("completed output lacks custom_tool_call: %v", comp)
	}
}

func TestResponsesHandler_NamespaceToolCallMapsBackToNamespacedFunctionCall(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			stub := &chatStub{}
			h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				stub.mu.Lock()
				stub.bodies = append(stub.bodies, b)
				stub.mu.Unlock()
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", chunk(`{"tool_calls":[{"index":0,"id":"call_n","type":"function","function":{"name":"multi_agent_v1__close_agent","arguments":"{\"target\":\"a1\"}"}}]}`, ""))
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk(`{}`, "tool_calls"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_n","type":"function","function":{"name":"multi_agent_v1__close_agent","arguments":"{\"target\":\"a1\"}"}}]}}]}`))
			}, nil)
			defer ts.Close()

			w := post(h, fixtureBody(t, "main_turn", func(m map[string]any) { m["stream"] = stream }))
			tools := stub.last(t)["tools"].([]any)
			found := false
			for _, tl := range tools {
				if tl.(map[string]any)["function"].(map[string]any)["name"] == "multi_agent_v1__close_agent" {
					found = true
				}
			}
			if !found {
				t.Fatalf("namespace tool not flattened: %v", tools)
			}
			var item map[string]any
			if stream {
				fin := eventsOf(w, "response.output_item.done")
				item = fin[len(fin)-1]["item"].(map[string]any)
			} else {
				var r map[string]any
				json.Unmarshal(w.Body.Bytes(), &r)
				item = r["output"].([]any)[0].(map[string]any)
			}
			if item["type"] != "function_call" || item["name"] != "close_agent" || item["namespace"] != "multi_agent_v1" ||
				item["arguments"] != `{"target":"a1"}` || item["call_id"] != "call_n" {
				t.Fatalf("function_call item wrong: %v", item)
			}
		})
	}
}

func TestTranslateTools_NamespaceFlatteningAndCollisions(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","name":"exec"}`),
		json.RawMessage(`{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"},{"type":"function","name":"wait"}]}`),
		json.RawMessage(`{"type":"namespace","name":"mcp.server","tools":[{"type":"function","name":"do thing"},{"type":"function","name":"wait"}]}`),
		json.RawMessage(`{"type":"namespace","name":"mcp_server","tools":[{"type":"function","name":"do_thing"}]}`),
		json.RawMessage(`{"type":"web_search"}`),
	}
	tt := translateTools(tools)
	var names []string
	for _, tl := range tt.Tools {
		names = append(names, tl["function"].(map[string]any)["name"].(string))
	}
	want := []string{"exec", "functions__exec", "wait", "mcp_server__do_thing", "mcp_server__wait", "mcp_server__do_thing_2"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names\n got %v\nwant %v", names, want)
	}
	if len(tt.Stripped) != 1 || tt.Stripped[0] != "web_search" {
		t.Errorf("stripped: %v", tt.Stripped)
	}
	b, ok := tt.Registry.resolve("functions__exec")
	if !ok || b.Namespace != "functions" || b.Name != "exec" || !b.Custom {
		t.Errorf("registry resolve: %v %v", b, ok)
	}
	if b, _ := tt.Registry.resolve("exec"); b.Namespace != "" || b.Custom {
		t.Errorf("top-level exec must keep its identity: %v", b)
	}
	long := sanitizeToolName("ns__" + strings.Repeat("a", 100))
	if len(long) > 64 {
		t.Errorf("flattened name too long: %d", len(long))
	}
}

func TestTranslateInput_NamespacedAndCustomCallsInHistory(t *testing.T) {
	tt := translateTools([]json.RawMessage{
		json.RawMessage(`{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}`),
		json.RawMessage(`{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"wait"}]}`),
	})
	input := json.RawMessage(`[
		{"role":"user","content":"go"},
		{"type":"custom_tool_call","call_id":"c1","name":"exec","namespace":"functions","input":"ls -la"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"files"},
		{"type":"function_call","call_id":"c2","name":"wait","namespace":"multi_agent_v1","arguments":"{}"}
	]`)
	msgs := mustTranslate(t, input, "", translateOptions{Tools: tt.Registry})
	fn := msgs[1]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "exec" || fn["arguments"] != `{"input":"ls -la"}` {
		t.Errorf("custom call: %v", fn)
	}
	fn = msgs[3]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "multi_agent_v1__wait" {
		t.Errorf("namespaced call: %v", fn)
	}
}

func TestTranslateToolChoice(t *testing.T) {
	tt := translateTools([]json.RawMessage{json.RawMessage(`{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}]}`)})
	got, _ := json.Marshal(translateToolChoice(json.RawMessage(`{"type":"function","namespace":"ns","name":"f"}`), tt.Registry))
	if string(got) != `{"function":{"name":"ns__f"},"type":"function"}` {
		t.Errorf("got %s", got)
	}
	if translateToolChoice(json.RawMessage(`"required"`), nil) != "required" {
		t.Error("string choice should pass through")
	}
}

// --- P1 4: reasoning ---

func TestResponsesHandler_ReasoningItemsCarryReasoningText(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"role":"assistant","reasoning_content":"think "}`, ""),
		chunk(`{"reasoning_content":"hard"}`, ""),
		chunk(`{"content":"answer"}`, ""),
		chunk(`{}`, "stop"),
	}, true), nil)
	defer ts.Close()
	w := post(h, `{"model":"test-model","input":"hi","stream":true}`)

	rd := eventsOf(w, "response.reasoning_text.delta")
	sd := eventsOf(w, "response.reasoning_summary_text.delta")
	if len(rd) != 2 || len(sd) != 2 || rd[0]["delta"] != "think " || rd[0]["content_index"].(float64) != 0 {
		t.Fatalf("reasoning delta events wrong:\n%s", w.Body.String())
	}
	if d := eventsOf(w, "response.reasoning_text.done"); len(d) != 1 || d[0]["text"] != "think hard" {
		t.Errorf("reasoning_text.done: %v", d)
	}
	fin := eventsOf(w, "response.output_item.done")
	item := fin[0]["item"].(map[string]any)
	if item["type"] != "reasoning" {
		t.Fatalf("first item: %v", item)
	}
	c := item["content"].([]any)[0].(map[string]any)
	s := item["summary"].([]any)[0].(map[string]any)
	if c["type"] != "reasoning_text" || c["text"] != "think hard" || s["type"] != "summary_text" || s["text"] != "think hard" {
		t.Errorf("reasoning item wrong: %v", item)
	}
}

func TestTranslateInput_ReasoningItemsReplayAsReasoningContent(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":"q"},
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"raw thoughts"}],"encrypted_content":"xyz"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"only summary"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]},
		{"role":"user","content":"next"}
	]`)
	msgs := mustTranslate(t, input, "", translateOptions{})
	if msgs[1]["reasoning_content"] != "raw thoughts" || msgs[1]["tool_calls"] == nil {
		t.Errorf("tool-call assistant message: %v", msgs[1])
	}
	if msgs[3]["reasoning_content"] != "only summary" || msgs[3]["content"] != "done" {
		t.Errorf("assistant message: %v", msgs[3])
	}
	if _, ok := msgs[4]["reasoning_content"]; ok {
		t.Errorf("orphan reasoning attached to a user message: %v", msgs[4])
	}
}

// --- P1 5: PDFs ---

func TestTranslateInput_InputFilePDFBecomesPDFData(t *testing.T) {
	input := json.RawMessage(`[{"role":"user","content":[
		{"type":"input_text","text":"read"},
		{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"},
		{"type":"input_file","filename":"b.pdf","file_data":"JVBERi0xLjQK"},
		{"type":"input_file","filename":"c.txt","file_data":"data:text/plain;base64,aGk="},
		{"type":"input_file","file_id":"file-123"},
		{"type":"input_image","image_url":"data:application/pdf;base64,JVBERi0xLjQK"}
	]}]`)
	msgs := mustTranslate(t, input, "", translateOptions{})
	parts := msgs[0]["content"].([]map[string]any)
	if parts[1]["type"] != "pdf_data" || parts[1]["data"] != "JVBERi0xLjQK" || parts[1]["filename"] != "a.pdf" {
		t.Errorf("data URL pdf: %v", parts[1])
	}
	if parts[2]["type"] != "pdf_data" || parts[2]["data"] != "JVBERi0xLjQK" {
		t.Errorf("bare base64 pdf: %v", parts[2])
	}
	if parts[3]["type"] != "file" || parts[3]["file"].(map[string]any)["filename"] != "c.txt" {
		t.Errorf("non-pdf file: %v", parts[3])
	}
	if parts[4]["type"] != "text" {
		t.Errorf("file_id placeholder: %v", parts[4])
	}
	if parts[5]["type"] != "pdf_data" {
		t.Errorf("input_image pdf: %v", parts[5])
	}
}

func TestResponsesHandler_PDFDataWithoutPipelineBecomesFilePart(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{chunk(`{"content":"ok"}`, "stop")}, true), nil)
	defer ts.Close()
	post(h, `{"model":"test-model","stream":true,"input":[{"role":"user","content":[
		{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}]}]}`)
	part := chatMessages(stub.last(t))[0]["content"].([]any)[0].(map[string]any)
	file, _ := part["file"].(map[string]any)
	if part["type"] != "file" || file["filename"] != "a.pdf" || file["file_data"] != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("expected OpenAI file part, got %v", part)
	}
}

// --- P1 6: errors ---

func TestResponsesHandler_ContextLengthErrorIsHTTP400BeforeStreaming(t *testing.T) {
	for _, stream := range []bool{true, false} {
		h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 140000 tokens. Contact http://203.0.113.3:8000/help","type":"BadRequestError","code":400}}`))
		}, nil)
		w := post(h, fmt.Sprintf(`{"model":"test-model","input":"hi","stream":%v}`, stream))
		ts.Close()
		if w.Code != 400 {
			t.Fatalf("stream=%v: expected HTTP 400, got %d", stream, w.Code)
		}
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
			t.Fatalf("stream=%v: must not be SSE", stream)
		}
		var r struct{ Error map[string]any }
		json.Unmarshal(w.Body.Bytes(), &r)
		if r.Error["code"] != "context_length_exceeded" {
			t.Fatalf("stream=%v: code %v", stream, r.Error)
		}
		if !strings.Contains(fmt.Sprint(r.Error["message"]), "131072") || strings.Contains(w.Body.String(), "203.0.113.3") {
			t.Fatalf("stream=%v: message not cleaned properly: %v", stream, r.Error["message"])
		}
	}
}

func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
		code   string
	}{
		{400, `{"error":{"message":"bad schema"}}`, 400, "invalid_prompt"},
		{422, `{"detail":"nope"}`, 400, "invalid_prompt"},
		{401, `{}`, 502, "upstream_auth_error"},
		{403, `{}`, 502, "upstream_auth_error"},
		{429, `{}`, 429, "rate_limit_exceeded"},
		{503, `{}`, 503, "server_is_overloaded"},
		{500, `secret internals`, 500, "upstream_error"},
		{400, `{"error":{"code":"context_length_exceeded","message":"too long"}}`, 400, "context_length_exceeded"},
	}
	for _, c := range cases {
		ue := classifyUpstreamError(c.status, []byte(c.body), http.Header{})
		if ue.Status != c.want || ue.Code != c.code {
			t.Errorf("%d %s: got %d %s, want %d %s", c.status, c.body, ue.Status, ue.Code, c.want, c.code)
		}
		if strings.Contains(ue.Message, "secret") {
			t.Errorf("raw body leaked for %d", c.status)
		}
	}
}

func TestResponsesHandler_UpstreamFailuresRecordedAgainstBackend(t *testing.T) {
	h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}, nil)
	defer ts.Close()
	url := ts.URL + "/v1"
	for i := 0; i < 2; i++ {
		post(h, `{"model":"test-model","input":"hi","stream":true}`)
	}
	if got := lb.Status(url).ConsecFails; got < 2 {
		t.Fatalf("failed attempts not recorded with lb: consecutive failures = %d", got)
	}
}

// --- P1 7: probe / fallback ---

func TestProbeNativeRoute_RequiresPostInAllow(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		allow      string
		wantSup    bool
		wantConclu bool
	}{
		{"cors 204 no Allow", 204, "", false, false},
		{"cors 200 no Allow", 200, "", false, false},
		{"200 with POST", 200, "OPTIONS, POST", true, true},
		{"405 with POST", 405, "POST", true, true},
		{"405 GET only", 405, "GET", false, false},
		{"404", 404, "", false, true},
		{"500", 500, "", false, false},
	}
	for _, c := range cases {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.allow != "" {
				w.Header().Set("Allow", c.allow)
			}
			w.Header().Set("Access-Control-Allow-Methods", "*")
			w.WriteHeader(c.status)
		}))
		h := NewResponsesHandler(config.NewTestConfigStore(&config.Config{}), nil, nil)
		sup, conclusive := h.probeNativeRoute(t.Context(), &config.ModelConfig{Backend: ts.URL + "/v1"}, "/responses")
		ts.Close()
		if sup != c.wantSup || conclusive != c.wantConclu {
			t.Errorf("%s: got (%v,%v), want (%v,%v)", c.name, sup, conclusive, c.wantSup, c.wantConclu)
		}
	}
}

func TestResponsesHandler_CORSServerWithMissingRouteFallsBackToTranslation(t *testing.T) {
	var chatCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodOptions:
			w.WriteHeader(http.StatusNoContent) // CORS preflight answer on every path
		case r.URL.Path == "/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"detail":"Not Found"}`))
		default:
			chatCalls++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"via chat"}}]}`))
		}
	}))
	defer ts.Close()
	cfg := &config.Config{Models: []config.ModelConfig{{Name: "test-model", Backend: ts.URL + "/v1", Model: "m", Timeout: 10}}}
	h := NewResponsesHandler(config.NewTestConfigStore(cfg), nil, nil)
	w := post(h, `{"model":"test-model","input":"hi"}`)
	if w.Code != 200 || chatCalls != 1 || !strings.Contains(w.Body.String(), "via chat") {
		t.Fatalf("expected translation fallback, status %d chat=%d: %s", w.Code, chatCalls, w.Body.String())
	}
}

func TestResponsesHandler_NativeModelMissing404IsNotRouteMissing(t *testing.T) {
	var chatCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"The model ` + "`m`" + ` does not exist.","type":"NotFoundError","code":404}}`))
		default:
			chatCalls++
		}
	}))
	defer ts.Close()
	cfg := &config.Config{Models: []config.ModelConfig{{Name: "test-model", Backend: ts.URL + "/v1", Model: "m", Timeout: 10}}}
	h := NewResponsesHandler(config.NewTestConfigStore(cfg), nil, nil)
	w := post(h, `{"model":"test-model","input":"hi"}`)
	if w.Code != 404 || chatCalls != 0 {
		t.Fatalf("expected the model-missing error to be reported, status %d chat=%d", w.Code, chatCalls)
	}
	if supported, ok := h.loadNativeSupport(&cfg.Models[0], "/responses"); ok && !supported {
		t.Fatal("a missing model must not mark the route unsupported")
	}
}

// --- P1 8: sampling defaults ---

func TestResponsesHandler_SamplingDefaultsAppliedWithoutOverridingClient(t *testing.T) {
	temp, topP := 0.7, 0.9
	topK, maxNew := 40, 2048
	effort := "xhigh"
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{chunk(`{"content":"x"}`, "stop")}, true), func(m *config.ModelConfig) {
		m.Defaults = &config.SamplingDefaults{Temperature: &temp, TopP: &topP, TopK: &topK, MaxNewTokens: &maxNew, ReasoningEffort: &effort}
	})
	defer ts.Close()

	post(h, `{"model":"test-model","input":"hi","stream":true}`)
	b := stub.last(t)
	if b["temperature"] != 0.7 || b["top_p"] != 0.9 || b["top_k"] != float64(40) || b["max_tokens"] != float64(2048) || b["reasoning_effort"] != "xhigh" {
		t.Fatalf("defaults not applied: %v", b)
	}

	post(h, `{"model":"test-model","input":"hi","stream":true,"temperature":0.1,"max_output_tokens":99,"reasoning":{"effort":"low"}}`)
	b = stub.last(t)
	if b["temperature"] != 0.1 || b["reasoning_effort"] != "low" || b["max_completion_tokens"] != float64(99) {
		t.Fatalf("client values overridden: %v", b)
	}
	if _, ok := b["max_tokens"]; ok {
		t.Fatalf("max_tokens default added next to client max_output_tokens: %v", b)
	}
}

// --- P2 ---

func TestResponsesHandler_ToolCallDeltasKeyedByIndex(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		// No id on the continuation deltas, an id-less second call, ids repeated.
		chunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"x\""}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":1,"function":{"name":"g","arguments":"{}"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"call_a","function":{"arguments":""}}]}`, ""),
		chunk(`{}`, "tool_calls"),
	}, true), nil)
	defer ts.Close()
	w := post(h, `{"model":"test-model","input":"hi","stream":true,"tools":[{"type":"function","name":"f"},{"type":"function","name":"g"}]}`)
	out := eventsOf(w, "response.completed")[0]["response"].(map[string]any)["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("expected 2 calls, got %v", out)
	}
	f, g := out[0].(map[string]any), out[1].(map[string]any)
	if f["name"] != "f" || f["arguments"] != `{"x":1}` || f["call_id"] != "call_a" {
		t.Errorf("first call: %v", f)
	}
	if g["name"] != "g" || g["arguments"] != "{}" || g["call_id"] == "" {
		t.Errorf("second call (no id upstream): %v", g)
	}
}

func TestResponsesHandler_UsageIncludesCachedAndReasoningTokens(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler([]string{
		chunk(`{"content":"x"}`, "stop"),
		`{"id":"c","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":12}}}`,
	}, true), nil)
	defer ts.Close()
	w := post(h, `{"model":"test-model","input":"hi","stream":true}`)
	u := eventsOf(w, "response.completed")[0]["response"].(map[string]any)["usage"].(map[string]any)
	if u["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(64) ||
		u["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(12) || u["total_tokens"] != float64(120) {
		t.Fatalf("usage: %v", u)
	}
}

func TestResponsesHandler_StatefulRequestsRejectedInTranslateMode(t *testing.T) {
	stub := &chatStub{}
	h, ts := newCodexHandler(t, stub.handler(nil, true), nil)
	defer ts.Close()
	for _, extra := range []string{`"previous_response_id":"resp_1"`, `"store":true`} {
		w := post(h, `{"model":"test-model","input":"hi",`+extra+`}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "not_supported") {
			t.Errorf("%s: expected clear 400, got %d %s", extra, w.Code, w.Body.String())
		}
	}
	if len(stub.bodies) != 0 {
		t.Error("rejected requests must not reach the backend")
	}
	// Codex's own shape (store:false) is fine.
	if w := post(h, `{"model":"test-model","input":"hi","store":false,"stream":true}`); w.Code != 200 {
		t.Errorf("store:false rejected: %d", w.Code)
	}
}

func TestHandleCompact_SingleSystemMessageAndThinkStripped(t *testing.T) {
	var got map[string]any
	h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"<think>hmm</think>The summary."}}]}`))
	}, nil)
	defer ts.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(
		`{"model":"test-model","instructions":"inst","input":[{"role":"developer","content":"dev"},{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.HandleCompact(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	msgs := chatMessages(got)
	systems := 0
	for _, m := range msgs {
		if m["role"] == "system" {
			systems++
		}
	}
	if systems != 1 || msgs[0]["role"] != "system" || !strings.HasSuffix(msgs[0]["content"].(string), "inst\n\ndev") {
		t.Fatalf("expected one merged leading system message: %v", msgs)
	}
	if strings.Contains(w.Body.String(), "hmm") || !strings.Contains(w.Body.String(), "The summary.") {
		t.Fatalf("think tags not stripped: %s", w.Body.String())
	}
}

// --- Timeouts ---

func TestResponsesHandler_TimeoutArmedUntilHeadersThenDisarmedForStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real 1s timeouts")
	}
	t.Run("slow headers time out", func(t *testing.T) {
		h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		}, func(m *config.ModelConfig) { m.Timeout = 1 })
		defer ts.Close()
		start := time.Now()
		w := post(h, `{"model":"test-model","input":"hi","stream":true}`)
		if w.Code != http.StatusGatewayTimeout {
			t.Fatalf("expected 504 when headers never arrive, got %d", w.Code)
		}
		if time.Since(start) > 2500*time.Millisecond {
			t.Fatalf("timeout not enforced before headers: %v", time.Since(start))
		}
	})
	t.Run("long stream after headers survives", func(t *testing.T) {
		h, ts := newCodexHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			time.Sleep(1500 * time.Millisecond)
			fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", chunk(`{"content":"late"}`, ""), chunk(`{}`, "stop"))
		}, func(m *config.ModelConfig) { m.Timeout = 1 })
		defer ts.Close()
		w := post(h, `{"model":"test-model","input":"hi","stream":true}`)
		if !hasEvent(w, "response.completed") {
			t.Fatalf("stream cut by the model timeout:\n%s", w.Body.String())
		}
	})
}
