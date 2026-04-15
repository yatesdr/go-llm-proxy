package config

import "testing"

func TestLookupBedrockContextWindow_KnownModels(t *testing.T) {
	cases := map[string]int{
		"zai.glm-4.7-flash":                            128000,
		"amazon.nova-lite-v1:0":                        300000,
		"amazon.nova-pro-v1:0":                         300000,
		"amazon.nova-micro-v1:0":                       128000,
		"anthropic.claude-3-5-sonnet-20241022-v2:0":    200000,
		"anthropic.claude-sonnet-4-20250514-v1:0":      200000,
		"anthropic.claude-3-haiku-20240307-v1:0":       200000,
		"meta.llama3-3-70b-instruct-v1:0":              128000,
		"cohere.command-r-plus-v1:0":                   128000,
		"mistral.mistral-large-2407-v1:0":              128000,

		// Inference-profile IDs carry a leading region qualifier; we must
		// strip it before matching.
		"us.anthropic.claude-sonnet-4-20250514-v1:0":   200000,
		"eu.anthropic.claude-3-5-sonnet-20241022-v2:0": 200000,
		"apac.amazon.nova-pro-v1:0":                    300000,
	}
	for id, want := range cases {
		if got := lookupBedrockContextWindow(id); got != want {
			t.Errorf("lookupBedrockContextWindow(%q) = %d, want %d", id, got, want)
		}
	}
}

func TestLookupBedrockContextWindow_Unknown(t *testing.T) {
	for _, id := range []string{
		"",
		"unknown.model-v1",
		"some.future.model",
	} {
		if got := lookupBedrockContextWindow(id); got != 0 {
			t.Errorf("lookupBedrockContextWindow(%q) = %d, want 0", id, got)
		}
	}
}
