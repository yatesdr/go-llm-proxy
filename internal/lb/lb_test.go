package lb

import (
	"testing"

	"go-llm-proxy/internal/config"
)

func TestAffinityKey_StableAcrossTurns(t *testing.T) {
	turn1 := []byte(`{"model":"m","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hi"}]}`)
	turn2 := []byte(`{"model":"m","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}]}`)
	if AffinityKey(turn1) != AffinityKey(turn2) {
		t.Error("expected same affinity across appended turns")
	}
	if AffinityKey(turn1) == 0 {
		t.Error("expected non-zero affinity for a parseable conversation")
	}
}

func TestAffinityKey_DistinguishesSessions(t *testing.T) {
	a := []byte(`{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"session A opener"}]}`)
	b := []byte(`{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"session B opener"}]}`)
	if AffinityKey(a) == AffinityKey(b) {
		t.Error("expected different sessions to hash differently")
	}
}

func TestAffinityKey_AnthropicAndResponsesShapes(t *testing.T) {
	msg := []byte(`{"system":"be nice","messages":[{"role":"user","content":"hi"}]}`)
	if AffinityKey(msg) == 0 {
		t.Error("anthropic-shaped body should produce affinity")
	}
	respArr := []byte(`{"instructions":"be nice","input":[{"role":"user","content":"hi"}]}`)
	if AffinityKey(respArr) == 0 {
		t.Error("responses-shaped body should produce affinity")
	}
	respStr := []byte(`{"input":"just a prompt"}`)
	if AffinityKey(respStr) == 0 {
		t.Error("string-input responses body should produce affinity")
	}
	if AffinityKey([]byte(`not json`)) != 0 || AffinityKey(nil) != 0 {
		t.Error("unparseable bodies should have no affinity")
	}
}

func TestResolveModel_SingleBackendPassthrough(t *testing.T) {
	cfg := &config.Config{Models: []config.ModelConfig{
		{Name: "m", Backends: []config.BackendConfig{{URL: "http://one:8000/v1", APIKey: "k"}}, Backend: "http://one:8000/v1", APIKey: "k"},
	}}
	m := &cfg.Models[0]
	view, release := ResolveModel(cfg, m, nil)
	defer release()
	if view != m {
		t.Error("single-backend model should pass through without a copy")
	}
	if got := Inflight("http://one:8000/v1"); got != 1 {
		t.Errorf("expected 1 in-flight, got %d", got)
	}
	release()
	release() // double release must be safe
	if got := Inflight("http://one:8000/v1"); got != 0 {
		t.Errorf("expected 0 in-flight after release, got %d", got)
	}
}

func TestResolveModel_SkipsDrainedBackend(t *testing.T) {
	cfg := &config.Config{
		Models: []config.ModelConfig{{Name: "m", Backends: []config.BackendConfig{
			{URL: "http://drained:8000", Disabled: true},
			{URL: "http://live:8000", APIKey: "bk"},
		}}},
	}
	view, release := ResolveModel(cfg, &cfg.Models[0], nil)
	defer release()
	if view.Backend != "http://live:8000" {
		t.Errorf("expected drained backend skipped, got %q", view.Backend)
	}
	if view.APIKey != "bk" {
		t.Errorf("expected backend api key, got %q", view.APIKey)
	}
}
