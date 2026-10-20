package handler

import (
	"testing"

	"go-llm-proxy/internal/config"
)

func TestModelInfoMarksConnectorAndPrivateBackendsLocal(t *testing.T) {
	cfg := &config.Config{Models: []config.ModelConfig{
		{Name: "via-connector", Backend: "connect://woodcrest/cn3-flare/v1"},
		{Name: "lan", Backend: "http://192.168.13.33:8000/v1"},
		{Name: "loopback", Backend: "http://localhost:8000/v1"},
		{Name: "hosted", Backend: "https://api.openai.com/v1"},
	}}
	want := map[string]bool{"via-connector": true, "lan": true, "loopback": true, "hosted": false}
	for _, m := range modelInfoFromConfig(cfg) {
		if m.Local != want[m.ID] {
			t.Errorf("%s: local = %v, want %v", m.ID, m.Local, want[m.ID])
		}
	}
}
