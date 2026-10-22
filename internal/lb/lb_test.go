package lb

import (
	"context"
	"errors"
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

func TestAffinityKey_PromptCacheKey(t *testing.T) {
	// The same prompt_cache_key sticks even when the opening items differ
	// (e.g. the auto-review side request vs the main turn share a session key
	// only if the client says so); different keys separate sessions.
	a := []byte(`{"prompt_cache_key":"sess-1","input":[{"role":"user","content":"one"}]}`)
	b := []byte(`{"prompt_cache_key":"sess-1","input":[{"role":"user","content":"two"}]}`)
	c := []byte(`{"prompt_cache_key":"sess-2","input":[{"role":"user","content":"one"}]}`)
	if AffinityKey(a) == 0 || AffinityKey(a) != AffinityKey(b) {
		t.Error("same prompt_cache_key should give the same affinity")
	}
	if AffinityKey(a) == AffinityKey(c) {
		t.Error("different prompt_cache_key should give different affinity")
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

func TestRecordOutcomeSuccessClearsProbeDown(t *testing.T) {
	url := "http://probe-recover.test:8000"
	SetProbeHealth(url, false)
	if Status(url).ProbeDown != true {
		t.Fatal("setup: probe-down not set")
	}
	RecordOutcome(url, false) // a failure must not clear it
	if !Status(url).ProbeDown {
		t.Fatal("failure cleared probe-down")
	}
	RecordOutcome(url, true) // real traffic worked: the backend is up
	if Status(url).ProbeDown {
		t.Fatal("real-traffic success should clear a stale probe-down verdict")
	}
}

func TestProbeDownBackendRecoversViaTraffic(t *testing.T) {
	backends := mkBackends(2, "boot-down")
	key := uint64(99)
	winner := selectBackend(backends, key)
	other := backends[0]
	if other.URL == winner.URL {
		other = backends[1]
	}
	SetProbeHealth(winner.URL, false) // down at boot
	if got := selectBackend(backends, key); got.URL != other.URL {
		t.Fatalf("probe-down winner should be skipped, got %s", got.URL)
	}
	// Only the other is serving; once the winner is answered by traffic
	// (e.g. a failover attempt or a direct request) it rejoins selection.
	RecordOutcome(winner.URL, true)
	if got := selectBackend(backends, key); got.URL != winner.URL {
		t.Fatalf("recovered backend should rejoin selection, got %s", got.URL)
	}
}

func TestRecordAttempt(t *testing.T) {
	url := "http://record-attempt.test:8000"
	boom := errors.New("boom")
	cases := []struct {
		name       string
		status     int
		err        error
		clientGone bool
		wantFailed bool
		wantFails  int
	}{
		{"transport error", 0, boom, false, true, 1},
		{"5xx", 503, nil, false, true, 2},
		{"auth rejected", 401, nil, false, true, 3},
		{"client hung up", 0, context.Canceled, true, false, 3},
		{"429 no signal", 429, nil, false, false, 3},
		{"400 no signal", 400, nil, false, false, 3},
		{"2xx closes", 200, nil, false, false, 0},
	}
	for _, c := range cases {
		if got := RecordAttempt(url, c.status, c.err, c.clientGone); got != c.wantFailed {
			t.Errorf("%s: failed=%v want %v", c.name, got, c.wantFailed)
		}
		if got := Status(url).ConsecFails; got != c.wantFails {
			t.Errorf("%s: consecutive failures=%d want %d", c.name, got, c.wantFails)
		}
	}
}

func TestAffinityKeyIgnoresCacheControl(t *testing.T) {
	a := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"yo","cache_control":{"type":"ephemeral"}}]}]}`)
	b := []byte(`{"system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"text","text":"yo"}]}]}`)
	if AffinityKey(a) == 0 || AffinityKey(a) != AffinityKey(b) {
		t.Fatalf("keys differ: %d vs %d", AffinityKey(a), AffinityKey(b))
	}
}
