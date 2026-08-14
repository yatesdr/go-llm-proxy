package lb

import (
	"encoding/json"
	"hash/fnv"
)

// affinityCapBytes bounds how much of each prefix component is hashed.
// Components beyond this size are stable in their first bytes anyway (system
// prompts, first user turns), and capping keeps hashing O(1) per request.
const affinityCapBytes = 4096

// AffinityKey derives the stickiness key from a request body: a hash of the
// conversation's stable prefix. All wire formats the proxy speaks are
// covered by the same probe — Chat Completions (messages), Anthropic
// Messages (system + messages), and Responses (instructions + input).
//
// Every turn of an append-only conversation re-sends the same opening
// elements, so the whole session hashes to the same key — and therefore the
// same backend — while distinct sessions spread across the pool. Returns 0
// (no affinity) when the body has none of the known fields or isn't JSON.
func AffinityKey(body []byte) uint64 {
	if len(body) == 0 {
		return 0
	}
	var probe struct {
		Messages     []json.RawMessage `json:"messages"`
		System       json.RawMessage   `json:"system"`
		Instructions string            `json:"instructions"`
		Input        json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0
	}

	h := fnv.New64a()
	wrote := false
	write := func(b []byte) {
		if len(b) == 0 {
			return
		}
		if len(b) > affinityCapBytes {
			b = b[:affinityCapBytes]
		}
		_, _ = h.Write(b)
		wrote = true
	}

	write(probe.System)
	write([]byte(probe.Instructions))

	// The first two messages cover the common "system + first user" opening
	// of Chat Completions bodies, and "first user (+ first assistant)" for
	// Anthropic Messages where system rides in its own field.
	for i, msg := range probe.Messages {
		if i >= 2 {
			break
		}
		write(msg)
	}

	// Responses API: input is either a string or an array of items; hash the
	// first item (or the string's head).
	if len(probe.Input) > 0 {
		if probe.Input[0] == '[' {
			var items []json.RawMessage
			if err := json.Unmarshal(probe.Input, &items); err == nil && len(items) > 0 {
				write(items[0])
			}
		} else {
			write(probe.Input)
		}
	}

	if !wrote {
		return 0
	}
	return h.Sum64()
}
