package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/usage"
)

// A short synthetic classifier prompt: the marker sentence and the output
// format line, nothing from any real prompt.
const (
	testAutoSeveritySystem = autoModeMarker + ".\n## Output Format\nOutput <severity>N</severity> where N is an integer 0-100."
	testAutoBlockSystem    = autoModeMarker + ".\n## Output Format\nReply <block>yes</block> or <block>no</block>."
	testAutoUser           = "<transcript>\n {\"user\":\"push it\"}\n {\"Bash\":\"git push --force\"}\n</transcript>\nRespond now."
)

func autoRequestBody(model, system string, stop []string, stream bool) string {
	m := map[string]any{
		"model":      model,
		"max_tokens": 64,
		"system":     []map[string]any{{"type": "text", "text": "x-anthropic-billing-header: cc_version=1"}, {"type": "text", "text": system}},
		"messages":   []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": testAutoUser}}}},
		"stream":     stream,
	}
	if stop != nil {
		m["stop_sequences"] = stop
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// lp builds a top_logprobs list from token -> probability.
func lp(probs map[string]float64) []map[string]any {
	var out []map[string]any
	for tok, p := range probs {
		out = append(out, map[string]any{"token": tok, "logprob": math.Log(p)})
	}
	return out
}

func logprobResponse(top []map[string]any, content string) map[string]any {
	return map[string]any{
		"id": "c1", "model": "decision-upstream",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "length",
			"message":  map[string]any{"role": "assistant", "content": content, "reasoning": "7"},
			"logprobs": map[string]any{"content": []map[string]any{{"token": "7", "logprob": -0.1, "top_logprobs": top}}},
		}},
		"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 1, "total_tokens": 1001},
	}
}

// decisionStub is a fake Chat Completions backend that records every request.
// Transcript warm-ups (an open final user message) are kept apart in warms,
// so requests holds exactly the decision calls in order.
type decisionStub struct {
	mu       sync.Mutex
	requests []map[string]any
	paths    []string
	auths    []string
	warms    []map[string]any
	// respond builds the reply for the n-th request (0-based).
	respond func(n int, req map[string]any) (int, any)
}

func isWarmUp(req map[string]any) bool {
	msgs, _ := req["messages"].([]any)
	if req["continue_final_message"] != true || len(msgs) == 0 {
		return false
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	return last["role"] == "user"
}

func (s *decisionStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(raw, &req)
	s.mu.Lock()
	if isWarmUp(req) {
		s.warms = append(s.warms, req)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "w", "choices": []any{}})
		return
	}
	n := len(s.requests)
	s.requests = append(s.requests, req)
	s.paths = append(s.paths, r.URL.Path)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()
	status, body := s.respond(n, req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (s *decisionStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// warmUps waits briefly for the background warm-ups to arrive.
func (s *decisionStub) warmUps(t *testing.T, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		got := append([]map[string]any(nil), s.warms...)
		s.mu.Unlock()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func scoreStub(top map[string]float64) *decisionStub {
	return &decisionStub{respond: func(n int, req map[string]any) (int, any) {
		if _, has := req["logprobs"]; has {
			return 200, logprobResponse(lp(top), "")
		}
		// Category call and generative fallback.
		return 200, map[string]any{
			"id": "c2", "model": "decision-upstream",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": " Force-Push! "}}},
			"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 3, "total_tokens": 1003},
		}
	}}
}

type autoEnv struct {
	handler http.Handler
	stub    *decisionStub
	usage   *usage.UsageLogger
	dbPath  string
}

func newAutoEnv(t *testing.T, stub *decisionStub, mutate func(*config.Config, string)) *autoEnv {
	t.Helper()
	ts := httptest.NewServer(stub)
	t.Cleanup(ts.Close)
	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "decision", Model: "decision-upstream", Backend: ts.URL + "/v1", APIKey: "backend-secret", Timeout: 10,
		}},
		AutoMode: &config.AutoModeConfig{Model: "decision"},
	}
	if mutate != nil {
		mutate(cfg, ts.URL)
	}
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	ul, err := usage.NewUsageLogger(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Drain the usage writer before t.TempDir's cleanup removes its database
	// (cleanups run last-in first-out); Close is idempotent.
	t.Cleanup(func() { _ = ul.Close() })
	cs := config.NewTestConfigStore(cfg)
	env := &autoEnv{stub: stub, usage: ul, dbPath: dbPath}
	env.handler = auth.AuthMiddleware(cs, NewMessagesHandler(cs, ul, nil))
	return env
}

func (e *autoEnv) post(t *testing.T, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-api-key", token)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w
}

func decodeMsg(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON: %v: %s", err, w.Body.String())
	}
	return m
}

func firstText(t *testing.T, m map[string]any) string {
	t.Helper()
	content, _ := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected one content block, got %v", m["content"])
	}
	return content[0].(map[string]any)["text"].(string)
}

// --- Detection ---

func TestAutoModeDetection(t *testing.T) {
	str := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	blocks := func(parts ...string) json.RawMessage {
		var bs []map[string]string
		for _, p := range parts {
			bs = append(bs, map[string]string{"type": "text", "text": p})
		}
		b, _ := json.Marshal(bs)
		return b
	}
	user := []json.RawMessage{json.RawMessage(`{"role":"user","content":"check this"}`)}

	cases := []struct {
		name       string
		system     json.RawMessage
		stop       []string
		messages   []json.RawMessage
		wantNil    bool
		wantFormat autoModeFormat
		wantStage  int
	}{
		{"severity stage 1", str(testAutoSeveritySystem), []string{"</severity>"}, user, false, autoFormatSeverity, 1},
		{"severity stage 2", str(testAutoSeveritySystem), nil, user, false, autoFormatSeverity, 2},
		{"block stage 1", str(testAutoBlockSystem), []string{"</block>"}, user, false, autoFormatBlock, 1},
		{"block stage 2", str(testAutoBlockSystem), nil, user, false, autoFormatBlock, 2},
		{"system as blocks", blocks("attribution line", testAutoSeveritySystem), []string{"</severity>"}, user, false, autoFormatSeverity, 1},
		{"stop sequence alone names the format", str(autoModeMarker + "."), []string{"</severity>"}, user, false, autoFormatSeverity, 1},
		{"block stop alone", str(autoModeMarker + "."), []string{"</block>"}, user, false, autoFormatBlock, 1},
		{"both markers present: severity wins", str(testAutoSeveritySystem + " <block>"), nil, user, false, autoFormatSeverity, 2},

		{"ordinary turn", str("You are Claude Code, a coding assistant."), nil, user, true, 0, 0},
		{"ordinary turn with stop sequences", str("You are a helpful assistant."), []string{"</severity>"}, user, true, 0, 0},
		{"mentions security monitor without the format", str("You are a security monitor. Review the action."), nil, user, true, 0, 0},
		{"marker without a format", str(autoModeMarker + ". Be careful."), nil, user, true, 0, 0},
		{"marker with unrelated stop sequence", str(autoModeMarker + "."), []string{"STOP"}, user, true, 0, 0},
		{"no user text", str(testAutoSeveritySystem), nil, nil, true, 0, 0},
		{"no system", nil, []string{"</severity>"}, user, true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &messagesRequest{System: tc.system, StopSequences: tc.stop, Messages: tc.messages}
			got := detectAutoModeRequest(req)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected no interception, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected interception")
			}
			if got.format != tc.wantFormat || got.stage != tc.wantStage {
				t.Fatalf("format=%v stage=%d, want format=%v stage=%d", got.format, got.stage, tc.wantFormat, tc.wantStage)
			}
		})
	}
}

func TestAutoModeUserTextJoinsBlocksVerbatim(t *testing.T) {
	msgs := []json.RawMessage{json.RawMessage(`{"role":"user","content":[{"type":"text","text":"<transcript>a</transcript>"},{"type":"text","text":"Stage suffix"}]}`)}
	if got := autoModeUserText(msgs); got != "<transcript>a</transcript>\nStage suffix" {
		t.Fatalf("got %q", got)
	}
}

// --- Score math ---

func TestScoreSeverityTokens(t *testing.T) {
	t.Run("leading spaces and non-numeric tokens", func(t *testing.T) {
		sc, ok := scoreSeverityTokens([]tokenLogprob{
			{" 90", math.Log(0.6)}, {"85", math.Log(0.2)}, {" 5", math.Log(0.1)},
			{"yes", math.Log(0.05)}, {"<", math.Log(0.02)}, {"-3", math.Log(0.01)}, {"7.5", math.Log(0.01)}, {"101", math.Log(0.01)},
		})
		if !ok {
			t.Fatalf("expected ok, mass=%v", sc.mass)
		}
		if math.Abs(sc.mass-0.9) > 1e-9 {
			t.Fatalf("mass = %v, want 0.9 (only 90, 85, 5)", sc.mass)
		}
		want := (90*0.6 + 85*0.2 + 5*0.1) / 0.9
		if math.Abs(sc.expected-want) > 1e-9 {
			t.Fatalf("E = %v, want %v", sc.expected, want)
		}
		if math.Abs(sc.pBlock-0.8/0.9) > 1e-9 {
			t.Fatalf("p_block = %v, want %v", sc.pBlock, 0.8/0.9)
		}
	})
	t.Run("boundary is exclusive: 50 is not a block", func(t *testing.T) {
		sc, ok := scoreSeverityTokens([]tokenLogprob{{"50", math.Log(0.5)}, {"51", math.Log(0.5)}})
		if !ok || math.Abs(sc.pBlock-0.5) > 1e-9 {
			t.Fatalf("p_block = %v ok=%v", sc.pBlock, ok)
		}
	})
	t.Run("numeric mass below 0.5 is rejected", func(t *testing.T) {
		if _, ok := scoreSeverityTokens([]tokenLogprob{{"7", math.Log(0.3)}, {"The", math.Log(0.6)}}); ok {
			t.Fatal("mass 0.3 must not be trusted")
		}
	})
	t.Run("no tokens", func(t *testing.T) {
		if _, ok := scoreSeverityTokens(nil); ok {
			t.Fatal("empty list must not score")
		}
	})
}

func TestScoreBlockTokens(t *testing.T) {
	sc, ok := scoreBlockTokens([]tokenLogprob{{"yes", math.Log(0.6)}, {" No", math.Log(0.2)}, {"maybe", math.Log(0.1)}})
	if !ok || math.Abs(sc.pBlock-0.75) > 1e-9 {
		t.Fatalf("p_block = %v ok=%v", sc.pBlock, ok)
	}
	if _, ok := scoreBlockTokens([]tokenLogprob{{"maybe", math.Log(0.9)}, {"yes", math.Log(0.1)}}); ok {
		t.Fatal("yes/no mass 0.1 must not be trusted")
	}
}

func TestDecideSeverityNeverFifty(t *testing.T) {
	cases := []struct {
		name      string
		sc        severityScore
		threshold float64
		wantN     int
		wantBlock bool
	}{
		{"clear allow", severityScore{pBlock: 0.01, expected: 3.2}, 0.5, 3, false},
		{"clear block", severityScore{pBlock: 0.97, expected: 91.4}, 0.5, 91, true},
		{"block side but mean low is lifted to 51", severityScore{pBlock: 0.6, expected: 50}, 0.5, 51, true},
		{"allow side but mean high is lowered to 49", severityScore{pBlock: 0.4, expected: 50}, 0.5, 49, false},
		{"exactly at threshold blocks", severityScore{pBlock: 0.5, expected: 47}, 0.5, 51, true},
		{"mean 50 allow side", severityScore{pBlock: 0.1, expected: 50.2}, 0.5, 49, false},
		{"custom threshold blocks earlier", severityScore{pBlock: 0.35, expected: 30}, 0.3, 51, true},
		{"custom threshold allows later", severityScore{pBlock: 0.6, expected: 70}, 0.7, 49, false},
		{"clamped at 100", severityScore{pBlock: 1, expected: 100.4}, 0.5, 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, block := decideSeverity(tc.sc, tc.threshold)
			if n != tc.wantN || block != tc.wantBlock {
				t.Fatalf("got n=%d block=%v, want n=%d block=%v", n, block, tc.wantN, tc.wantBlock)
			}
			if n == 50 {
				t.Fatal("N must never be 50")
			}
		})
	}
}

// The warm-up ends exactly where the next decision's prompt diverges: right
// after the transcript's last entry, with the same system text, open-ended.
func TestWarmTranscriptPayload(t *testing.T) {
	model := &config.ModelConfig{Model: "up"}
	ar := &autoModeRequest{system: "sys", user: "<transcript>\n{\"user\":\"a\"}\n{\"Bash\":\"ls\"}\n</transcript>\nRespond now."}
	p := warmTranscriptPayload(model, ar)
	msgs := p["messages"].([]map[string]any)
	if msgs[0]["content"] != "sys" || msgs[1]["role"] != "user" ||
		msgs[1]["content"] != "<transcript>\n{\"user\":\"a\"}\n{\"Bash\":\"ls\"}\n" {
		t.Fatalf("messages = %v", msgs)
	}
	if p["continue_final_message"] != true || p["add_generation_prompt"] != false || p["max_tokens"] != 1 || p["model"] != "up" {
		t.Fatalf("payload = %v", p)
	}
	if _, has := p["temperature"]; has {
		t.Error("temperature must be left out (Flare rejects 0)")
	}
	// The next decision adds an action before </transcript>: its user text
	// starts with this warm-up's.
	next := strings.Replace(ar.user, "</transcript>", "{\"Bash\":\"pwd\"}\n</transcript>", 1)
	if !strings.HasPrefix(next, msgs[1]["content"].(string)) {
		t.Fatal("the next decision must extend the warm-up")
	}
	if warmTranscriptPayload(model, &autoModeRequest{system: "sys", user: "no transcript"}) != nil {
		t.Fatal("no transcript, no warm-up")
	}
}

// Every decision sends one warm-up of its transcript alongside it.
func TestAutoModeSendsTranscriptWarmUp(t *testing.T) {
	stub := scoreStub(map[string]float64{"5": 0.9})
	env := newAutoEnv(t, stub, nil)
	decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
	warms := stub.warmUps(t, 1)
	if len(warms) != 1 || stub.count() != 1 {
		t.Fatalf("warm-ups %d, decisions %d", len(warms), stub.count())
	}
	msgs := warms[0]["messages"].([]any)
	if got := msgs[1].(map[string]any)["content"]; got != strings.TrimSuffix(testAutoUser, "</transcript>\nRespond now.") {
		t.Fatalf("warm-up user text = %q", got)
	}
	if warms[0]["model"] != "decision-upstream" {
		t.Fatalf("model = %v", warms[0]["model"])
	}
}

// A warm-up the backend rejects changes nothing: the decision is answered.
func TestAutoModeWarmUpFailureIsHarmless(t *testing.T) {
	stub := scoreStub(map[string]float64{"5": 0.9})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(raw, &req)
		if isWarmUp(req) {
			http.Error(w, "continue_final_message unsupported", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		stub.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	env := newAutoEnv(t, stub, func(c *config.Config, _ string) { c.Models[0].Backend = ts.URL + "/v1" })
	for i := 0; i < 3; i++ {
		m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
		if firstText(t, m) != "<severity>5" {
			t.Fatalf("decision %d: %v", i, m)
		}
	}
}

// --- Handler ---

// The requested model name is irrelevant: claude-sonnet-5 has no alias and is
// not configured, yet the classifier request is answered.
func TestAutoModeInterceptsWithoutAlias(t *testing.T) {
	stub := scoreStub(map[string]float64{"92": 0.9, "5": 0.05, "The": 0.02})
	env := newAutoEnv(t, stub, nil)
	m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
	if m["model"] != "claude-sonnet-5" {
		t.Fatalf("model = %v, want the requested name", m["model"])
	}
	if stub.count() != 1 {
		t.Fatalf("expected exactly one decision call, got %d", stub.count())
	}
}

func TestAutoModeUpstreamRequestShape(t *testing.T) {
	stub := scoreStub(map[string]float64{"92": 0.95})
	env := newAutoEnv(t, stub, nil)
	decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))

	if stub.paths[0] != "/v1/chat/completions" || stub.auths[0] != "Bearer backend-secret" {
		t.Fatalf("path %q auth %q", stub.paths[0], stub.auths[0])
	}
	req := stub.requests[0]
	wantNum := map[string]float64{"max_tokens": 1, "temperature": 0, "top_logprobs": 20}
	for k, v := range wantNum {
		if req[k] != v {
			t.Errorf("%s = %v, want %v", k, req[k], v)
		}
	}
	if req["model"] != "decision-upstream" {
		t.Errorf("model = %v, want the upstream name", req["model"])
	}
	if req["logprobs"] != true || req["continue_final_message"] != true || req["add_generation_prompt"] != false {
		t.Errorf("flags wrong: %v", req)
	}
	for _, unwanted := range []string{"stop", "stream", "tools"} {
		if _, has := req[unwanted]; has {
			t.Errorf("%s must not be sent", unwanted)
		}
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	role := func(i int) string { return msgs[i].(map[string]any)["role"].(string) }
	content := func(i int) string { return msgs[i].(map[string]any)["content"].(string) }
	if role(0) != "system" || role(1) != "user" || role(2) != "assistant" {
		t.Fatalf("roles wrong: %v", msgs)
	}
	if !strings.Contains(content(0), "x-anthropic-billing-header") || !strings.Contains(content(0), testAutoSeveritySystem) {
		t.Errorf("system text must hold every system block: %q", content(0))
	}
	if content(1) != testAutoUser {
		t.Errorf("user text altered: %q", content(1))
	}
	if content(2) != "<severity>" {
		t.Errorf("prefill = %q", content(2))
	}
}

func TestAutoModeBlockFormatRequestShape(t *testing.T) {
	stub := scoreStub(map[string]float64{"yes": 0.9, "no": 0.05})
	env := newAutoEnv(t, stub, nil)
	m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoBlockSystem, []string{"</block>"}, false), ""))
	msgs := stub.requests[0]["messages"].([]any)
	if got := msgs[2].(map[string]any)["content"]; got != "<block>" {
		t.Fatalf("prefill = %v", got)
	}
	if firstText(t, m) != "<block>yes" || m["stop_sequence"] != "</block>" || m["stop_reason"] != "stop_sequence" {
		t.Fatalf("stage 1 block reply wrong: %v", m)
	}
}

func TestAutoModeStage1SeverityResponse(t *testing.T) {
	cases := []struct {
		name string
		top  map[string]float64
		want string
	}{
		{"block", map[string]float64{"92": 0.5, " 88": 0.45}, "<severity>90"},
		{"allow", map[string]float64{"3": 0.9, "8": 0.05}, "<severity>3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAutoEnv(t, scoreStub(tc.top), nil)
			m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
			if got := firstText(t, m); got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
			if m["stop_reason"] != "stop_sequence" || m["stop_sequence"] != "</severity>" {
				t.Errorf("stop fields: %v / %v", m["stop_reason"], m["stop_sequence"])
			}
			if m["type"] != "message" || m["role"] != "assistant" || !strings.HasPrefix(m["id"].(string), "msg_") {
				t.Errorf("envelope wrong: %v", m)
			}
			u := m["usage"].(map[string]any)
			if u["input_tokens"].(float64) != 1000 || u["output_tokens"].(float64) != 1 {
				t.Errorf("usage = %v", u)
			}
		})
	}
}

func TestAutoModeStage2AllowHasNoCategoryCall(t *testing.T) {
	stub := scoreStub(map[string]float64{"4": 0.97})
	env := newAutoEnv(t, stub, nil)
	m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, nil, false), ""))
	if got := firstText(t, m); got != "<severity>4</severity>" {
		t.Fatalf("text = %q", got)
	}
	if m["stop_reason"] != "end_turn" || m["stop_sequence"] != nil {
		t.Fatalf("stop fields: %v / %v", m["stop_reason"], m["stop_sequence"])
	}
	if stub.count() != 1 {
		t.Fatalf("an allow needs no category call, got %d calls", stub.count())
	}
}

func TestAutoModeStage2BlockAsksForCategory(t *testing.T) {
	stub := scoreStub(map[string]float64{"95": 0.97})
	env := newAutoEnv(t, stub, nil)
	m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, nil, false), ""))
	if got := firstText(t, m); got != "<severity>95</severity><category>ForcePush</category>" {
		t.Fatalf("text = %q", got)
	}
	if m["stop_reason"] != "end_turn" || m["stop_sequence"] != nil {
		t.Fatalf("stop fields: %v / %v", m["stop_reason"], m["stop_sequence"])
	}
	if stub.count() != 2 {
		t.Fatalf("expected decision + category calls, got %d", stub.count())
	}
	cat := stub.requests[1]
	if cat["max_tokens"].(float64) != 16 || cat["temperature"].(float64) != 0 {
		t.Errorf("category sampling wrong: %v", cat)
	}
	if _, has := cat["logprobs"]; has {
		t.Error("category call must not request logprobs")
	}
	if stop, _ := cat["stop"].([]any); len(stop) != 1 || stop[0] != "</category>" {
		t.Errorf("stop = %v", cat["stop"])
	}
	if cat["continue_final_message"] != true || cat["add_generation_prompt"] != false {
		t.Errorf("flags wrong: %v", cat)
	}
	msgs := cat["messages"].([]any)
	if got := msgs[2].(map[string]any)["content"]; got != "<severity>95</severity><category>" {
		t.Errorf("category prefill = %v", got)
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 2000 {
		t.Errorf("usage should sum both calls: %v", u)
	}
}

func TestAutoModeCategoryFailureOmitsCategory(t *testing.T) {
	stub := &decisionStub{respond: func(n int, req map[string]any) (int, any) {
		if _, has := req["logprobs"]; has {
			return 200, logprobResponse(lp(map[string]float64{"95": 0.97}), "")
		}
		return 500, map[string]any{"error": "boom"}
	}}
	env := newAutoEnv(t, stub, nil)
	m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, nil, false), ""))
	if got := firstText(t, m); got != "<severity>95</severity>" {
		t.Fatalf("text = %q", got)
	}
}

func TestAutoModeBlockFormatResponses(t *testing.T) {
	cases := []struct {
		name     string
		top      map[string]float64
		stop     []string
		wantText string
		wantStop any
		wantWhy  string
	}{
		{"stage 1 yes", map[string]float64{"yes": 0.9}, []string{"</block>"}, "<block>yes", "</block>", "stop_sequence"},
		{"stage 1 no", map[string]float64{"no": 0.9}, []string{"</block>"}, "<block>no", "</block>", "stop_sequence"},
		{"stage 2 no", map[string]float64{"no": 0.9}, nil, "<block>no</block>", nil, "end_turn"},
		{"stage 2 yes", map[string]float64{"yes": 0.9}, nil, "<block>yes</block>", nil, "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := scoreStub(tc.top)
			env := newAutoEnv(t, stub, nil)
			m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoBlockSystem, tc.stop, false), ""))
			if got := firstText(t, m); got != tc.wantText {
				t.Errorf("text = %q, want %q", got, tc.wantText)
			}
			if m["stop_sequence"] != tc.wantStop || m["stop_reason"] != tc.wantWhy {
				t.Errorf("stop = %v / %v", m["stop_reason"], m["stop_sequence"])
			}
			if stub.count() != 1 {
				t.Errorf("block format never asks for a category, got %d calls", stub.count())
			}
		})
	}
}

func TestAutoModeThresholdFromConfig(t *testing.T) {
	// p_block is 0.35: allow at the default 0.5, block at 0.3.
	top := map[string]float64{"80": 0.35, "10": 0.65}
	env := newAutoEnv(t, scoreStub(top), nil)
	if got := firstText(t, decodeMsg(t, env.post(t, autoRequestBody("c", testAutoSeveritySystem, []string{"</severity>"}, false), ""))); got != "<severity>35" {
		t.Fatalf("default threshold: %q", got)
	}
	env = newAutoEnv(t, scoreStub(top), func(c *config.Config, _ string) { c.AutoMode.BlockProbability = 0.3 })
	got := firstText(t, decodeMsg(t, env.post(t, autoRequestBody("c", testAutoSeveritySystem, []string{"</severity>"}, false), "")))
	if got != "<severity>51" {
		t.Fatalf("threshold 0.3: %q", got)
	}
}

func TestAutoModeStreamingShape(t *testing.T) {
	env := newAutoEnv(t, scoreStub(map[string]float64{"95": 0.97}), nil)
	w := env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, true), "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", w.Code, w.Header().Get("Content-Type"))
	}
	var names []string
	var datas []map[string]any
	for _, block := range strings.Split(strings.TrimSpace(w.Body.String()), "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed SSE block %q", block)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &d); err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(lines[0], "event: ")
		if d["type"] != name {
			t.Fatalf("event %q carries type %v", name, d["type"])
		}
		names = append(names, name)
		datas = append(datas, d)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", names)
	}
	start := datas[0]["message"].(map[string]any)
	if start["model"] != "claude-sonnet-5" || start["stop_reason"] != nil {
		t.Errorf("message_start: %v", start)
	}
	if delta := datas[2]["delta"].(map[string]any); delta["type"] != "text_delta" || delta["text"] != "<severity>95" {
		t.Errorf("text delta: %v", delta)
	}
	md := datas[4]["delta"].(map[string]any)
	if md["stop_reason"] != "stop_sequence" || md["stop_sequence"] != "</severity>" {
		t.Errorf("message_delta: %v", md)
	}
}

// --- Fallback ---

func TestAutoModeFallbacks(t *testing.T) {
	cases := []struct {
		name    string
		respond func(n int, req map[string]any) (int, any)
	}{
		{"upstream error", func(n int, req map[string]any) (int, any) {
			if _, has := req["logprobs"]; has {
				return 503, map[string]any{"error": "down"}
			}
			return 200, generativeReply("<severity>12")
		}},
		{"no logprobs", func(n int, req map[string]any) (int, any) {
			if _, has := req["logprobs"]; has {
				return 200, map[string]any{"id": "c", "model": "m", "choices": []map[string]any{{"index": 0, "finish_reason": "length", "message": map[string]any{"role": "assistant", "content": "7"}}}}
			}
			return 200, generativeReply("<severity>12")
		}},
		{"numeric mass below 0.5", func(n int, req map[string]any) (int, any) {
			if _, has := req["logprobs"]; has {
				return 200, logprobResponse(lp(map[string]float64{"7": 0.2, "The": 0.7}), "")
			}
			return 200, generativeReply("<severity>12")
		}},
		{"unparseable body", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &decisionStub{respond: tc.respond}
			if tc.respond == nil {
				stub.respond = func(n int, req map[string]any) (int, any) {
					if _, has := req["logprobs"]; has {
						return 200, "not an object"
					}
					return 200, generativeReply("<severity>12")
				}
			}
			env := newAutoEnv(t, stub, nil)
			body := autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false)
			m := decodeMsg(t, env.post(t, body, ""))
			if got := firstText(t, m); got != "<severity>12" {
				t.Fatalf("fallback text = %q", got)
			}
			if stub.count() != 2 {
				t.Fatalf("expected decision call then generative call, got %d", stub.count())
			}
			gen := stub.requests[1]
			if gen["model"] != "decision-upstream" {
				t.Errorf("fallback must run on the decision model, got %v", gen["model"])
			}
			if _, has := gen["logprobs"]; has {
				t.Error("fallback is the ordinary generative path")
			}
			if stop, _ := gen["stop"].([]any); len(stop) != 1 || stop[0] != "</severity>" {
				t.Errorf("the request's stop sequences must survive: %v", gen["stop"])
			}
			msgs := gen["messages"].([]any)
			if msgs[0].(map[string]any)["role"] != "system" || msgs[len(msgs)-1].(map[string]any)["role"] != "user" {
				t.Errorf("fallback messages = %v", msgs)
			}
		})
	}
}

func generativeReply(text string) map[string]any {
	return map[string]any{
		"id": "c3", "model": "decision-upstream",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "stop_reason": "</severity>",
			"message": map[string]any{"role": "assistant", "content": text}}},
		"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 4, "total_tokens": 904},
	}
}

// --- Config, keys and neighbours ---

func TestAutoModeOffLeavesRequestsAlone(t *testing.T) {
	stub := scoreStub(map[string]float64{"95": 0.97})
	env := newAutoEnv(t, stub, func(c *config.Config, _ string) { c.AutoMode = nil })
	w := env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), "")
	if w.Code != http.StatusNotFound || stub.count() != 0 {
		t.Fatalf("status %d, upstream calls %d: without auto_mode the unknown model is a 404", w.Code, stub.count())
	}
}

func TestAutoModeOrdinaryTurnsAreUntouched(t *testing.T) {
	stub := scoreStub(map[string]float64{"95": 0.97})
	env := newAutoEnv(t, stub, nil)
	body := `{"model":"decision","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}`
	w := env.post(t, body, "")
	m := decodeMsg(t, w)
	if got := firstText(t, m); got != "Force-Push!" {
		t.Fatalf("ordinary turn should be proxied normally, got %q", got)
	}
	if _, has := stub.requests[0]["logprobs"]; has {
		t.Fatal("an ordinary turn must not become a decision call")
	}
}

func TestAutoModeKeyWithoutDecisionModelFallsThrough(t *testing.T) {
	stub := scoreStub(map[string]float64{"95": 0.97})
	env := newAutoEnv(t, stub, func(c *config.Config, url string) {
		c.Models = append(c.Models, config.ModelConfig{Name: "other", Model: "other", Backend: url + "/v1", Timeout: 10})
		c.Keys = []config.KeyConfig{
			{Key: "sk-limited", Name: "limited", Models: []string{"other"}},
			{Key: "sk-full", Name: "full"},
		}
	})
	body := autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false)

	w := env.post(t, body, "sk-limited")
	if w.Code != http.StatusForbidden || stub.count() != 0 {
		t.Fatalf("key not allowed the decision model: status %d, calls %d (want normal handling: 403 for claude-sonnet-5, no calls)", w.Code, stub.count())
	}
	m := decodeMsg(t, env.post(t, body, "sk-full"))
	if got := firstText(t, m); got != "<severity>95" {
		t.Fatalf("allowed key: %q", got)
	}
}

func TestAutoModeAcceptsAliasAsDecisionModel(t *testing.T) {
	stub := scoreStub(map[string]float64{"2": 0.97})
	env := newAutoEnv(t, stub, func(c *config.Config, _ string) {
		c.Aliases = map[string]string{"fast": "decision"}
		c.AutoMode.Model = "fast"
	})
	m := decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
	if got := firstText(t, m); got != "<severity>2" {
		t.Fatalf("text = %q", got)
	}
}

func TestAutoModeUsageLoggedUnderDecisionModel(t *testing.T) {
	env := newAutoEnv(t, scoreStub(map[string]float64{"95": 0.97}), nil)
	decodeMsg(t, env.post(t, autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
	if err := env.usage.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", env.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var model, alias, endpoint string
	var status, in, out int
	if err := db.QueryRow(`SELECT model, alias_from, endpoint, status_code, input_tokens, output_tokens FROM usage`).
		Scan(&model, &alias, &endpoint, &status, &in, &out); err != nil {
		t.Fatal(err)
	}
	if model != "decision" || alias != "claude-sonnet-5" || endpoint != "/v1/messages" || status != 200 || in != 1000 || out != 1 {
		t.Fatalf("usage row = %q %q %q %d %d %d", model, alias, endpoint, status, in, out)
	}
}

// A connect:// decision backend resolves in the shared transport: the call
// reaches the hub at /<site>/<service>/v1/chat/completions.
func TestAutoModeConnectBackendThroughHub(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotApp string
	var gotBody map[string]any
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotApp = r.URL.Path, r.Header.Get("X-Eidonix-Application")
		json.Unmarshal(raw, &gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logprobResponse(lp(map[string]float64{"96": 0.95}), ""))
	}))
	defer hub.Close()
	httputil.SetConnect(hub.URL, "go-llm-proxy")
	t.Cleanup(func() { httputil.SetConnect("", "") })

	cfg := &config.Config{
		Models:   []config.ModelConfig{{Name: "decision", Model: "decision-upstream", Backend: "connect://site-a/glm/v1", Timeout: 10}},
		AutoMode: &config.AutoModeConfig{Model: "decision"},
	}
	cs := config.NewTestConfigStore(cfg)
	h := NewMessagesHandler(cs, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(autoRequestBody("claude-sonnet-5", testAutoSeveritySystem, []string{"</severity>"}, false)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	m := decodeMsg(t, w)
	if got := firstText(t, m); got != "<severity>96" {
		t.Fatalf("text = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/site-a/glm/v1/chat/completions" || gotApp != "go-llm-proxy" {
		t.Fatalf("hub saw path=%q app=%q", gotPath, gotApp)
	}
	if gotBody["model"] != "decision-upstream" || gotBody["logprobs"] != true {
		t.Fatalf("hub body: %v", gotBody)
	}
}
