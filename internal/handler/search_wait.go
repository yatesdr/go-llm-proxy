package handler

import (
	"context"
	"log/slog"
	"time"
)

// waitForSearchOrDisconnect waits for a search goroutine to complete while
// emitting periodic SSE keepalive comments to the client. Returns true if
// the search finished normally; false if the client disconnected before
// the search completed.
//
// On disconnect, this function blocks (up to disconnectGrace) for the
// goroutine to observe the cancelled context and exit, so callers can
// safely read the goroutine's output variables without racing. This also
// plugs what would otherwise be a goroutine leak: without this wait, the
// handler returns while the search continues in the background, and under
// sustained client-abort traffic the goroutines pile up.
//
// keepaliveMsg is the SSE comment to flush on each tick (e.g. ": searching\n\n").
func waitForSearchOrDisconnect(
	ctx context.Context,
	searchDone <-chan struct{},
	emitKeepalive func(),
	tickerInterval, disconnectGrace time.Duration,
	logger string,
) (completed bool) {
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-searchDone:
			return true
		case <-ticker.C:
			if emitKeepalive != nil {
				emitKeepalive()
			}
		case <-ctx.Done():
			// Wait briefly for the goroutine to notice the cancellation
			// and exit. Bounded so a misbehaving goroutine can't hold the
			// handler hostage.
			timer := time.NewTimer(disconnectGrace)
			defer timer.Stop()
			select {
			case <-searchDone:
			case <-timer.C:
				slog.Warn("search goroutine did not exit within grace period after client disconnect",
					"source", logger)
			}
			return false
		}
	}
}
