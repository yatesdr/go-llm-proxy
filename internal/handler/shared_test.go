package handler

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingKeepaliveWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
	writes  atomic.Int32
}

func newBlockingKeepaliveWriter() *blockingKeepaliveWriter {
	return &blockingKeepaliveWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingKeepaliveWriter) Header() http.Header { return w.header }
func (w *blockingKeepaliveWriter) WriteHeader(int)     {}
func (w *blockingKeepaliveWriter) Flush()              {}

func (w *blockingKeepaliveWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.writes.Add(1)
	return len(p), nil
}

func TestStopPipelineKeepalivesWaitsForWriter(t *testing.T) {
	w := newBlockingKeepaliveWriter()
	stop := startPipelineKeepalives(w, time.Millisecond)

	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("keepalive writer did not start")
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a response write was still in progress")
	case <-time.After(20 * time.Millisecond):
	}

	close(w.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("keepalive goroutine did not exit")
	}

	writes := w.writes.Load()
	time.Sleep(5 * time.Millisecond)
	if got := w.writes.Load(); got != writes {
		t.Fatalf("writer ran after stop returned: writes changed from %d to %d", writes, got)
	}
}
