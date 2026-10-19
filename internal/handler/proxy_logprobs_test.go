package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testLogprobs = `{"content":[{"token":"7","logprob":-0.05,"bytes":[55],"top_logprobs":[{"token":"7","logprob":-0.05,"bytes":[55]},{"token":"8","logprob":-3.2,"bytes":[56]}]}]}`

// Chat Completions passthrough must keep choices[].logprobs: the response is
// re-serialized to strip think tags, and the typed structs used to drop them.
func TestProxyHandler_NonStreamingKeepsLogprobs(t *testing.T) {
	for _, withTags := range []bool{false, true} {
		t.Run(fmt.Sprintf("think_tags=%v", withTags), func(t *testing.T) {
			content := "answer"
			if withTags {
				content = "<think>hidden</think>answer"
			}
			proxy, ts := newTestProxyHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"c1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":%q},"logprobs":%s,"finish_reason":"stop"}]}`, content, testLogprobs)
			})
			defer ts.Close()

			body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"logprobs":true,"top_logprobs":2}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, req)

			var resp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
					Logprobs json.RawMessage `json:"logprobs"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Choices) != 1 {
				t.Fatalf("bad response %d: %s", w.Code, w.Body.String())
			}
			if resp.Choices[0].Message.Content != "answer" {
				t.Fatalf("content = %q", resp.Choices[0].Message.Content)
			}
			var got, want any
			if err := json.Unmarshal(resp.Choices[0].Logprobs, &got); err != nil || got == nil {
				t.Fatalf("logprobs missing from response: %s", w.Body.String())
			}
			json.Unmarshal([]byte(testLogprobs), &want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("logprobs changed:\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestProxyHandler_StreamingKeepsLogprobs(t *testing.T) {
	proxy, ts := newTestProxyHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		// Reasoning is stripped and think tags filtered, so both chunks are
		// re-serialized by the proxy.
		chunks := []string{
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"reasoning":"thinking"},"logprobs":` + testLogprobs + `,"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"content":"<think>x</think>ok"},"logprobs":` + testLogprobs + `,"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"logprobs":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	n := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		if strings.Contains(line, `"logprobs":{"content"`) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected logprobs on both chunks, found %d in:\n%s", n, w.Body.String())
	}
}
