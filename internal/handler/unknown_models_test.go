package handler

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnknownModelRegistryConcurrentRecord(t *testing.T) {
	r := newUnknownModelRegistry()
	const workers = 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Record("reviewer-model", "responses")
		}()
	}
	wg.Wait()

	got := r.Snapshot()
	if len(got) != 1 || got[0].Count != workers || got[0].LastEndpoint != "responses" {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestUnknownModelRegistryCapEvictionAndIDLimit(t *testing.T) {
	r := newUnknownModelRegistry()
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	for i := 0; i < maxUnknownModels+1; i++ {
		r.Record(fmt.Sprintf("model-%03d", i), "responses")
	}
	got := r.Snapshot()
	if len(got) != maxUnknownModels {
		t.Fatalf("entry count = %d, want %d", len(got), maxUnknownModels)
	}
	for _, entry := range got {
		if entry.ID == "model-000" {
			t.Fatal("oldest entry was not evicted")
		}
	}

	longID := strings.Repeat("é", maxUnknownModelIDLength+10)
	r.Record(longID, "messages")
	if id := r.Snapshot()[0].ID; len([]rune(id)) != maxUnknownModelIDLength {
		t.Fatalf("capped ID length = %d", len([]rune(id)))
	}
}

func TestUnknownModelRegistryRemoveAndSort(t *testing.T) {
	r := newUnknownModelRegistry()
	now := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	r.Record("older", "messages")
	r.Record("newer", "responses")
	got := r.Snapshot()
	if len(got) != 2 || got[0].ID != "newer" {
		t.Fatalf("snapshot order = %+v", got)
	}
	r.Remove("newer")
	got = r.Snapshot()
	if len(got) != 1 || got[0].ID != "older" {
		t.Fatalf("snapshot after remove = %+v", got)
	}
}
