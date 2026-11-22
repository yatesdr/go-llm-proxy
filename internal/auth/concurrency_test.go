package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/ratelimit"
)

// holdHandler blocks each request until release is closed, so tests can
// keep requests in flight.
type holdHandler struct {
	started chan struct{}
	release chan struct{}
}

func (h *holdHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.started <- struct{}{}
	<-h.release
	w.WriteHeader(http.StatusOK)
}

func serveWithKey(h http.Handler, key *config.KeyConfig, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if key != nil {
		req = req.WithContext(withKeyContext(req.Context(), key))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestConcurrencyMiddlewareRejectsOverLimit(t *testing.T) {
	hold := &holdHandler{started: make(chan struct{}, 4), release: make(chan struct{})}
	h := ConcurrencyMiddleware(ratelimit.NewConcurrency(), hold)
	key := &config.KeyConfig{Key: "sk-a", Name: "a", MaxConcurrent: 2}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); serveWithKey(h, key, "/v1/chat/completions") }()
		<-hold.started
	}

	rec := serveWithKey(h, key, "/v1/chat/completions")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 is missing Retry-After")
	}
	var oai struct {
		Error struct{ Type string } `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &oai)
	if oai.Error.Type != "rate_limit_error" {
		t.Errorf("OpenAI error type %q, want rate_limit_error", oai.Error.Type)
	}

	rec = serveWithKey(h, key, "/v1/messages")
	var ant struct {
		Type  string
		Error struct{ Type string } `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ant)
	if rec.Code != http.StatusTooManyRequests || ant.Type != "error" || ant.Error.Type != "rate_limit_error" {
		t.Errorf("/v1/messages rejection: status %d body %s, want Anthropic 429", rec.Code, rec.Body)
	}

	close(hold.release)
	wg.Wait()
	if rec := serveWithKey(h, key, "/v1/chat/completions"); rec.Code != http.StatusOK {
		t.Fatalf("after in-flight requests finished: status %d, want 200", rec.Code)
	}
}

func TestConcurrencyMiddlewareNoLimit(t *testing.T) {
	hold := &holdHandler{started: make(chan struct{}, 8), release: make(chan struct{})}
	h := ConcurrencyMiddleware(ratelimit.NewConcurrency(), hold)
	var wg sync.WaitGroup
	for _, key := range []*config.KeyConfig{{Key: "sk-a", Name: "a"}, nil} {
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); serveWithKey(h, key, "/v1/messages") }()
			<-hold.started // would block forever if a request were refused
		}
	}
	close(hold.release)
	wg.Wait()
}

func TestConcurrencyMiddlewareEnrolledKeysShareByEmail(t *testing.T) {
	hold := &holdHandler{started: make(chan struct{}, 2), release: make(chan struct{})}
	h := ConcurrencyMiddleware(ratelimit.NewConcurrency(), hold)
	oldKey := &config.KeyConfig{Key: "sk-old", Email: "a@example.com", MaxConcurrent: 1}
	newKey := &config.KeyConfig{Key: "sk-new", Email: "a@example.com", MaxConcurrent: 1}

	done := make(chan struct{})
	go func() { serveWithKey(h, oldKey, "/v1/messages"); close(done) }()
	<-hold.started
	if rec := serveWithKey(h, newKey, "/v1/messages"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("replacement key got %d while the old key's request runs, want 429", rec.Code)
	}
	close(hold.release)
	<-done
}
