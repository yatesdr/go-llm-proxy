package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go-llm-proxy/internal/config"
)

type nativeStub struct {
	mu      sync.Mutex
	paths   []string
	headers []http.Header
	bodies  []string
	status  int
}

func (s *nativeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.headers = append(s.headers, r.Header.Clone())
	s.bodies = append(s.bodies, string(b))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.status)
	if s.status == 200 {
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"flagged-upstream","content":[{"type":"text","text":"<severity>3"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		return
	}
	io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`)
}

func (s *nativeStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

func nativeEnv(t *testing.T, native *nativeStub, suffix string, flag bool, decision *decisionStub) *autoEnv {
	ns := httptest.NewServer(native)
	t.Cleanup(ns.Close)
	return newAutoEnv(t, decision, func(cfg *config.Config, _ string) {
		cfg.Models = append(cfg.Models, config.ModelConfig{
			Name: "flagged", Model: "flagged-upstream", Backend: ns.URL + suffix, APIKey: "k", Timeout: 10, NativeClassifier: flag,
		})
	})
}

func TestNativeClassifierForwardsNatively(t *testing.T) {
	for _, suffix := range []string{"", "/v1", "/v1/"} {
		native := &nativeStub{status: 200}
		decision := scoreStub(map[string]float64{"92": 0.9})
		env := nativeEnv(t, native, suffix, true, decision)
		w := env.post(t, autoRequestBody("flagged", testAutoSeveritySystem, []string{"</severity>"}, false), "")
		decodeMsg(t, w)
		if native.count() != 1 || native.paths[0] != "/v1/messages" {
			t.Fatalf("suffix %q: native calls %v", suffix, native.paths)
		}
		if got := native.headers[0].Get("Anthropic-Version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q", got)
		}
		if decision.count() != 0 {
			t.Fatalf("decision head called %d times", decision.count())
		}
	}
}

func TestNativeClassifierOffOrOrdinaryRequestUnchanged(t *testing.T) {
	// Flag off: the decision head answers.
	native := &nativeStub{status: 200}
	decision := scoreStub(map[string]float64{"92": 0.9})
	env := nativeEnv(t, native, "", false, decision)
	decodeMsg(t, env.post(t, autoRequestBody("flagged", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
	if native.count() != 0 || decision.count() != 1 {
		t.Fatalf("flag off: native %d, decision %d", native.count(), decision.count())
	}

	// Flag on, ordinary request: translated to Chat Completions, not /v1/messages.
	native = &nativeStub{status: 200}
	decision = scoreStub(map[string]float64{"92": 0.9})
	env = nativeEnv(t, native, "", true, decision)
	env.post(t, `{"model":"flagged","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "")
	if native.count() != 1 || native.paths[0] != "/chat/completions" && native.paths[0] != "/v1/chat/completions" {
		t.Fatalf("ordinary request paths: %v", native.paths)
	}
	if decision.count() != 0 {
		t.Fatalf("decision head called for ordinary request")
	}
}

func TestNativeClassifierFallsBackOn4xx(t *testing.T) {
	native := &nativeStub{status: 400}
	decision := scoreStub(map[string]float64{"92": 0.9})
	env := nativeEnv(t, native, "", true, decision)
	decodeMsg(t, env.post(t, autoRequestBody("flagged", testAutoSeveritySystem, []string{"</severity>"}, false), ""))
	if native.count() != 1 || decision.count() != 1 {
		t.Fatalf("native %d, decision %d", native.count(), decision.count())
	}
}

func TestNativeClassifierNoFallbackOn429(t *testing.T) {
	native := &nativeStub{status: 429}
	decision := scoreStub(map[string]float64{"92": 0.9})
	env := nativeEnv(t, native, "", true, decision)
	w := env.post(t, autoRequestBody("flagged", testAutoSeveritySystem, []string{"</severity>"}, false), "")
	if w.Code != 429 || decision.count() != 0 {
		t.Fatalf("status %d, decision %d", w.Code, decision.count())
	}
}
