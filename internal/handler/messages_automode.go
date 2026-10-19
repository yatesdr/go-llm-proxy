package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/lb"
	"go-llm-proxy/internal/usage"
)

// Claude Code auto mode.
//
// With --permission-mode auto, Claude Code has a classifier review every
// action that is not read-only. Behind a gateway it sends that review as an
// ordinary non-streaming POST /v1/messages whose system prompt is a security
// monitor prompt asking for "<severity>N</severity>" (current builds) or
// "<block>yes|no" (older builds). When auto_mode is configured, the proxy
// recognises these requests and answers them with one logprob read on the
// decision model instead of a generated reply: the model is asked to continue
// an assistant turn that already says "<severity>", and the distribution of
// the first token (GLM tokenises 0-100 as one token each) is the score.

// autoModeMarker is the sentence every classifier system prompt contains.
const autoModeMarker = "You are a security monitor for autonomous AI coding agents"

const (
	autoSeverityStop = "</severity>"
	autoBlockStop    = "</block>"
	// autoMinMass is the least probability mass the numeric (or yes/no)
	// tokens must hold for the logprob read to be trusted.
	autoMinMass = 0.5
	// autoTopLogprobs is how many alternatives the decision call requests.
	autoTopLogprobs = 20
)

type autoModeFormat int

const (
	autoFormatSeverity autoModeFormat = iota
	autoFormatBlock
)

func (f autoModeFormat) String() string {
	if f == autoFormatBlock {
		return "block"
	}
	return "severity"
}

// autoModeRequest is a recognised classifier request.
type autoModeRequest struct {
	format autoModeFormat
	stage  int // 1 when the request carries stop sequences, else 2
	system string
	user   string
}

// detectAutoModeRequest reports whether req is a Claude Code auto-mode
// classifier request: the system text holds the marker sentence and either a
// stop sequence or the system text itself names the severity / block output
// format. Anything else (ordinary turns, a prompt that merely mentions a
// security monitor) is not intercepted.
func detectAutoModeRequest(req *messagesRequest) *autoModeRequest {
	system := translateAnthropicSystem(req.System)
	if !strings.Contains(system, autoModeMarker) {
		return nil
	}
	hasSeverity, hasBlock := false, false
	for _, s := range req.StopSequences {
		switch s {
		case autoSeverityStop:
			hasSeverity = true
		case autoBlockStop:
			hasBlock = true
		}
	}
	if strings.Contains(system, "<severity>N</severity>") {
		hasSeverity = true
	}
	if strings.Contains(system, "<block>") {
		hasBlock = true
	}
	if !hasSeverity && !hasBlock {
		return nil
	}
	user := autoModeUserText(req.Messages)
	if user == "" {
		return nil
	}
	ar := &autoModeRequest{format: autoFormatSeverity, stage: 2, system: system, user: user}
	if hasBlock && !hasSeverity {
		ar.format = autoFormatBlock
	}
	if len(req.StopSequences) > 0 {
		ar.stage = 1
	}
	return ar
}

// autoModeUserText joins the text of the request's user messages verbatim.
func autoModeUserText(msgs []json.RawMessage) string {
	var parts []string
	for _, raw := range msgs {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			parts = append(parts, s)
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" || b.Type == "" {
				texts = append(texts, b.Text)
			}
		}
		parts = append(parts, strings.Join(texts, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// --- Score math ---

// tokenLogprob is one alternative from top_logprobs.
type tokenLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// severityScore is the reading of a first-token distribution.
type severityScore struct {
	pBlock   float64 // probability mass above the boundary, normalised
	expected float64 // E[v] over the normalised distribution
	mass     float64 // raw probability held by numeric tokens
}

// scoreSeverityTokens keeps the alternatives that are integers 0-100 (after
// trimming spaces), sums their probabilities per value, and normalises. ok is
// false when the numeric mass is below autoMinMass.
func scoreSeverityTokens(top []tokenLogprob) (severityScore, bool) {
	probs := make(map[int]float64)
	mass := 0.0
	for _, t := range top {
		v, ok := parseSeverityToken(t.Token)
		if !ok {
			continue
		}
		p := math.Exp(t.Logprob)
		if math.IsNaN(p) || math.IsInf(p, 0) {
			continue
		}
		probs[v] += p
		mass += p
	}
	if mass < autoMinMass {
		return severityScore{mass: mass}, false
	}
	var sc severityScore
	sc.mass = mass
	for v, p := range probs {
		p /= mass
		sc.expected += float64(v) * p
		if v > 50 {
			sc.pBlock += p
		}
	}
	return sc, true
}

// parseSeverityToken parses a token that is an integer 0-100 once spaces are
// trimmed ("7", " 85"). Signs, decimals and words are not scores.
func parseSeverityToken(tok string) (int, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" || len(tok) > 3 {
		return 0, false
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(tok)
	if err != nil || v < 0 || v > 100 {
		return 0, false
	}
	return v, true
}

// scoreBlockTokens reads yes/no tokens (case-insensitive, spaces trimmed).
func scoreBlockTokens(top []tokenLogprob) (severityScore, bool) {
	var yes, no float64
	for _, t := range top {
		p := math.Exp(t.Logprob)
		if math.IsNaN(p) || math.IsInf(p, 0) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(t.Token)) {
		case "yes":
			yes += p
		case "no":
			no += p
		}
	}
	mass := yes + no
	if mass < autoMinMass {
		return severityScore{mass: mass}, false
	}
	return severityScore{pBlock: yes / mass, mass: mass}, true
}

// decideSeverity turns a score into the integer the classifier reply carries
// and whether the action is blocked. N is round(E), forced to the side p_block
// chooses so it is never exactly 50.
func decideSeverity(sc severityScore, threshold float64) (n int, block bool) {
	n = int(math.Round(sc.expected))
	block = sc.pBlock >= threshold
	if block {
		if n < 51 {
			n = 51
		}
	} else if n > 49 {
		n = 49
	}
	if n > 100 {
		n = 100
	}
	if n < 0 {
		n = 0
	}
	return n, block
}

// --- Upstream calls ---

type decisionResponse struct {
	Choices []struct {
		Message struct {
			Content          *string `json:"content"`
			Reasoning        *string `json:"reasoning"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"message"`
		Logprobs *struct {
			Content []struct {
				TopLogprobs []tokenLogprob `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage *api.ChunkUsage `json:"usage"`
}

// firstTokenAlternatives returns choices[0].logprobs.content[0].top_logprobs.
func (d *decisionResponse) firstTokenAlternatives() []tokenLogprob {
	if len(d.Choices) == 0 || d.Choices[0].Logprobs == nil || len(d.Choices[0].Logprobs.Content) == 0 {
		return nil
	}
	return d.Choices[0].Logprobs.Content[0].TopLogprobs
}

// text returns the generated text from whichever field the reasoning parser
// left it in.
func (d *decisionResponse) text() string {
	if len(d.Choices) == 0 {
		return ""
	}
	m := d.Choices[0].Message
	for _, s := range []*string{m.Content, m.Reasoning, m.ReasoningContent} {
		if s != nil && strings.TrimSpace(*s) != "" {
			return *s
		}
	}
	return ""
}

// callDecision posts one Chat Completions request to the decision backend.
// Transport failures and 5xx/auth rejections are recorded against the
// backend; the caller records the success through logUsage.
func (h *MessagesHandler) callDecision(ctx context.Context, r *http.Request, model *config.ModelConfig, payload map[string]any) (*decisionResponse, int, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal decision request: %w", err)
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(model.Backend, "/")+api.ChatCompletionsPath, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, fmt.Errorf("create decision request: %w", err)
	}
	upReq.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("X-Request-ID"); v != "" {
		upReq.Header.Set("X-Request-ID", v)
	}
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}
	resp, err := h.client.Do(upReq)
	if err != nil || (resp != nil && resp.StatusCode >= 400) {
		lb.RecordAttempt(model.Backend, statusOf(resp), err, r.Context().Err() != nil)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("decision request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read decision response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("decision backend returned HTTP %d", resp.StatusCode)
	}
	var dr decisionResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode decision response: %w", err)
	}
	return &dr, resp.StatusCode, nil
}

// decisionPayload builds the spec's prefill request: the assistant turn is
// left open on prefill and the model continues it.
func decisionPayload(model *config.ModelConfig, ar *autoModeRequest, prefill string) map[string]any {
	return map[string]any{
		"model":                  model.Model,
		"max_tokens":             1,
		"temperature":            0,
		"logprobs":               true,
		"top_logprobs":           autoTopLogprobs,
		"continue_final_message": true,
		"add_generation_prompt":  false,
		"messages": []map[string]any{
			{"role": "system", "content": ar.system},
			{"role": "user", "content": ar.user},
			{"role": "assistant", "content": prefill},
		},
	}
}

// categoryPayload asks for the BLOCK rule name once the severity is known.
func categoryPayload(model *config.ModelConfig, ar *autoModeRequest, n int) map[string]any {
	return map[string]any{
		"model":                  model.Model,
		"max_tokens":             16,
		"temperature":            0,
		"stop":                   []string{"</category>"},
		"continue_final_message": true,
		"add_generation_prompt":  false,
		"messages": []map[string]any{
			{"role": "system", "content": ar.system},
			{"role": "user", "content": ar.user},
			{"role": "assistant", "content": fmt.Sprintf("<severity>%d</severity><category>", n)},
		},
	}
}

// sanitizeCategory keeps letters, digits and spaces only.
func sanitizeCategory(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// --- Interception ---

// serveAutoMode answers a classifier request from the decision model's
// logprobs. It returns handled=true once a response has been written. When it
// returns handled=false with a non-empty fallbackModel, the caller continues
// down the ordinary translated path with the model swapped to fallbackModel
// (so the reply is still a valid classifier answer); with an empty
// fallbackModel the request is not ours and is handled unchanged.
func (h *MessagesHandler) serveAutoMode(w http.ResponseWriter, r *http.Request, cfg *config.Config, req *messagesRequest, body []byte) (handled bool, fallbackModel string) {
	ar := detectAutoModeRequest(req)
	if ar == nil {
		return false, ""
	}
	decision := cfg.AutoModeModel()
	if decision == nil {
		return false, ""
	}
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, decision.Name) {
		return false, ""
	}

	started := time.Now()
	logDecision := func(fallback string, sc severityScore, n int) {
		attrs := []any{
			"stage", ar.stage, "format", ar.format.String(),
			"model", decision.Name, "latency_ms", time.Since(started).Milliseconds(),
		}
		if fallback != "" {
			attrs = append(attrs, "fallback", fallback)
			slog.Info("auto mode decision", attrs...)
			return
		}
		attrs = append(attrs, "p_block", math.Round(sc.pBlock*10000)/10000)
		if ar.format == autoFormatSeverity {
			attrs = append(attrs, "severity", n)
		}
		slog.Info("auto mode decision", attrs...)
	}

	chosen, release := lb.ResolveModel(cfg, decision, body)
	defer release()
	if chosen == nil {
		logDecision("no backend", severityScore{}, 0)
		return false, decision.Name
	}

	keyName, keyHash := "", ""
	if key != nil {
		keyName = key.Name
		keyHash = usage.HashKey(key.Key)
	}

	// The decision call is never a stream, so the timeout stays armed for the
	// whole exchange (no disarmIfStream).
	ctx, cancel, _ := armUntilHeaders(r.Context(), chosen)
	defer cancel()

	prefill := "<severity>"
	if ar.format == autoFormatBlock {
		prefill = "<block>"
	}
	payload := decisionPayload(chosen, ar, prefill)
	reqBytes := int64(len(ar.system) + len(ar.user))

	dr, status, err := h.callDecision(ctx, r, chosen, payload)
	if err != nil {
		slog.Warn("auto mode decision call failed", "model", decision.Name, "error", err)
		logDecision("upstream error", severityScore{}, 0)
		return false, decision.Name
	}
	totals := newUsageTotals()
	totals.add(dr.Usage)
	logUsageOnce := func() {
		logUsageChat(h.usage, usageLogInput{
			startTime: started, statusCode: status,
			keyName: keyName, keyHash: keyHash,
			model: decision.Name, aliasFrom: aliasFor(req.Model, decision.Name), endpoint: "/v1/messages",
			backend: chosen.Backend, requestBytes: reqBytes,
		}, totals.usage())
	}

	top := dr.firstTokenAlternatives()
	if len(top) == 0 {
		logUsageOnce()
		logDecision("no logprobs", severityScore{}, 0)
		return false, decision.Name
	}
	var sc severityScore
	var ok bool
	if ar.format == autoFormatBlock {
		sc, ok = scoreBlockTokens(top)
	} else {
		sc, ok = scoreSeverityTokens(top)
	}
	if !ok {
		logUsageOnce()
		logDecision(fmt.Sprintf("low token mass %.2f", sc.mass), sc, 0)
		return false, decision.Name
	}

	n, block := decideSeverity(sc, cfg.AutoMode.Threshold())
	category := ""
	if ar.format == autoFormatSeverity && ar.stage == 2 && block {
		if cr, _, cerr := h.callDecision(ctx, r, chosen, categoryPayload(chosen, ar, n)); cerr == nil {
			totals.add(cr.Usage)
			category = sanitizeCategory(cr.text())
		} else {
			slog.Warn("auto mode category call failed; omitting category", "error", cerr)
		}
	}

	text, stopReason, stopSequence := autoModeAnswer(ar, n, block, category)
	msg := autoMessage{
		id: api.RandomID("msg_"), model: req.Model, text: text,
		stopReason: stopReason, stopSequence: stopSequence, usage: totals.usage(),
	}
	respBytes := msg.write(w, req.Stream)
	logDecision("", sc, n)
	logUsageChat(h.usage, usageLogInput{
		startTime: started, statusCode: status,
		keyName: keyName, keyHash: keyHash,
		model: decision.Name, aliasFrom: aliasFor(req.Model, decision.Name), endpoint: "/v1/messages",
		backend: chosen.Backend, requestBytes: reqBytes, responseBytes: respBytes,
	}, totals.usage())
	return true, ""
}

// aliasFor returns the requested name for the usage record when it differs
// from the model that actually served the request.
func aliasFor(requested, served string) string {
	if requested == served {
		return ""
	}
	return requested
}

// autoModeAnswer renders the classifier reply text and its stop fields.
func autoModeAnswer(ar *autoModeRequest, n int, block bool, category string) (text, stopReason string, stopSequence any) {
	if ar.format == autoFormatBlock {
		answer := "no"
		if block {
			answer = "yes"
		}
		if ar.stage == 1 {
			return "<block>" + answer, "stop_sequence", autoBlockStop
		}
		return "<block>" + answer + "</block>", "end_turn", nil
	}
	if ar.stage == 1 {
		return fmt.Sprintf("<severity>%d", n), "stop_sequence", autoSeverityStop
	}
	text = fmt.Sprintf("<severity>%d</severity>", n)
	if n > 50 && category != "" {
		text += "<category>" + category + "</category>"
	}
	return text, "end_turn", nil
}

// usageTotals sums token usage over the decision and category calls.
type usageTotals struct{ u api.ChunkUsage }

func newUsageTotals() *usageTotals { return &usageTotals{} }

func (t *usageTotals) add(u *api.ChunkUsage) {
	if u == nil {
		return
	}
	t.u.PromptTokens += u.PromptTokens
	t.u.CompletionTokens += u.CompletionTokens
	t.u.TotalTokens += u.TotalTokens
	if u.PromptTokensDetails != nil {
		if t.u.PromptTokensDetails == nil {
			t.u.PromptTokensDetails = &api.PromptTokensDetails{}
		}
		t.u.PromptTokensDetails.CachedTokens += u.PromptTokensDetails.CachedTokens
	}
}

func (t *usageTotals) usage() *api.ChunkUsage { return &t.u }

// autoMessage is the Anthropic Messages reply to a classifier request.
type autoMessage struct {
	id           string
	model        string
	text         string
	stopReason   string
	stopSequence any
	usage        *api.ChunkUsage
}

// write sends the reply as one JSON message, or as the minimal valid event
// sequence when the client asked for a stream. It returns the bytes written.
func (m autoMessage) write(w http.ResponseWriter, stream bool) int64 {
	httputil.SetSecurityHeaders(w)
	usageObj := anthropicUsage(m.usage)
	if usageObj == nil {
		usageObj = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(map[string]any{
			"id": m.id, "type": "message", "role": "assistant",
			"content":     []any{map[string]any{"type": "text", "text": m.text}},
			"model":       m.model,
			"stop_reason": m.stopReason, "stop_sequence": m.stopSequence,
			"usage": usageObj,
		})
		n, _ := w.Write(append(data, '\n'))
		return int64(n)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var total int64
	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		data, _ := json.Marshal(payload)
		n, _ := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		total += int64(n)
	}
	startUsage := map[string]any{}
	for k, v := range usageObj {
		startUsage[k] = v
	}
	startUsage["output_tokens"] = 0
	emit("message_start", map[string]any{"message": map[string]any{
		"id": m.id, "type": "message", "role": "assistant", "content": []any{},
		"model": m.model, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage,
	}})
	emit("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	emit("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": m.text}})
	emit("content_block_stop", map[string]any{"index": 0})
	emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": m.stopReason, "stop_sequence": m.stopSequence},
		"usage": map[string]any{"output_tokens": usageObj["output_tokens"]},
	})
	emit("message_stop", map[string]any{})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return total
}
