package usage

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUsageLoggerCloseDrainsQueuedRecords(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	ul, err := NewUsageLogger(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	const records = usageQueueSize + 250
	for i := 0; i < records; i++ {
		ul.Log(UsageRecord{
			Timestamp: time.Now(), Model: "canonical", AliasFrom: "reviewer", Endpoint: "/v1/test", StatusCode: 200,
		})
	}
	if err := ul.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != records {
		t.Fatalf("Close persisted %d records, want %d", got, records)
	}
	var model, aliasFrom string
	if err := db.QueryRow("SELECT model, alias_from FROM usage LIMIT 1").Scan(&model, &aliasFrom); err != nil {
		t.Fatal(err)
	}
	if model != "canonical" || aliasFrom != "reviewer" {
		t.Fatalf("stored model metadata = (%q, %q)", model, aliasFrom)
	}
}

func TestUsageLoggerConcurrentLogAndClose(t *testing.T) {
	ul, err := NewUsageLogger(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var callers sync.WaitGroup
	for i := 0; i < 100; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			ul.Log(UsageRecord{Timestamp: time.Now(), Model: "test"})
		}()
	}
	closed := make(chan error, 1)
	go func() {
		<-start
		closed <- ul.Close()
	}()
	close(start)
	callers.Wait()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	// Calls after shutdown are safe no-ops, and Close is idempotent.
	ul.Log(UsageRecord{Timestamp: time.Now(), Model: "after-close"})
	if err := ul.Close(); err != nil {
		t.Fatal(err)
	}
}
