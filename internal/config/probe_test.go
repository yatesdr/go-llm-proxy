package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeBackendForModelMatchesModelID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"other","max_model_len":4096},{"id":"qwen3.5-9b-vision","max_model_len":16384}]}`))
	}))
	defer srv.Close()

	res := ProbeBackendForModel(srv.URL+"/v1", "", "", "qwen3.5-9b-vision")
	if res.ContextWindow != 16384 || res.Engine != "openai-compatible" {
		t.Fatalf("matched model: got %+v, want context 16384 from openai-compatible", res)
	}
	res = ProbeBackendForModel(srv.URL+"/v1", "", "", "missing")
	if res.ContextWindow != 0 {
		t.Fatalf("unmatched model among several: got context %d, want 0", res.ContextWindow)
	}
}
