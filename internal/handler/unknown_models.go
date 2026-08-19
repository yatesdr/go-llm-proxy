package handler

import (
	"sort"
	"sync"
	"time"
)

const (
	maxUnknownModelIDLength = 128
	maxUnknownModels        = 200
)

// UnknownModel is one unrecognized client model ID observed since process
// start. The registry is intentionally in-memory and bounded.
type UnknownModel struct {
	ID           string
	Count        uint64
	FirstSeen    time.Time
	LastSeen     time.Time
	LastEndpoint string
}

type unknownModelRegistry struct {
	mu      sync.RWMutex
	entries map[string]UnknownModel
	now     func() time.Time
}

func newUnknownModelRegistry() *unknownModelRegistry {
	return &unknownModelRegistry{
		entries: make(map[string]UnknownModel),
		now:     time.Now,
	}
}

func capUnknownModelID(id string) string {
	runes := []rune(id)
	if len(runes) > maxUnknownModelIDLength {
		return string(runes[:maxUnknownModelIDLength])
	}
	return id
}

func (r *unknownModelRegistry) Record(modelID, endpoint string) {
	id := capUnknownModelID(modelID)
	if id == "" {
		return
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.entries[id]; ok {
		entry.Count++
		entry.LastSeen = now
		entry.LastEndpoint = endpoint
		r.entries[id] = entry
		return
	}

	if len(r.entries) >= maxUnknownModels {
		oldestID := ""
		var oldest time.Time
		for candidateID, entry := range r.entries {
			if oldestID == "" || entry.LastSeen.Before(oldest) ||
				(entry.LastSeen.Equal(oldest) && candidateID < oldestID) {
				oldestID = candidateID
				oldest = entry.LastSeen
			}
		}
		delete(r.entries, oldestID)
	}

	r.entries[id] = UnknownModel{
		ID:           id,
		Count:        1,
		FirstSeen:    now,
		LastSeen:     now,
		LastEndpoint: endpoint,
	}
}

func (r *unknownModelRegistry) Snapshot() []UnknownModel {
	r.mu.RLock()
	entries := make([]UnknownModel, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}
	r.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].LastSeen.Equal(entries[j].LastSeen) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].LastSeen.After(entries[j].LastSeen)
	})
	return entries
}

func (r *unknownModelRegistry) Remove(id string) {
	r.mu.Lock()
	delete(r.entries, capUnknownModelID(id))
	r.mu.Unlock()
}

var unknownModels = newUnknownModelRegistry()

// Record stores an unrecognized model request in the process-wide registry.
func Record(modelID, endpoint string) { unknownModels.Record(modelID, endpoint) }

// Snapshot returns registry entries sorted by most recently seen first.
func Snapshot() []UnknownModel { return unknownModels.Snapshot() }

// Remove deletes a model ID from the process-wide registry.
func Remove(id string) { unknownModels.Remove(id) }
