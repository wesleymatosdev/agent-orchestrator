package project

import (
	"context"
	"sync"
)

// Clone progress stages the UI can render. They mirror the labels git itself
// prints on stderr during a clone.
const (
	CloneStageStarting    = "starting"
	CloneStageReceiving   = "receiving"
	CloneStageResolving   = "resolving"
	CloneStageCheckingOut = "checking_out"
	CloneStageDone        = "done"
	CloneStageFailed      = "failed"
)

// CloneProgressEvent is one progress update for an in-flight repository clone.
// Events are broadcast live (not persisted); RemoteURL lets a subscriber
// ignore clones it did not start.
type CloneProgressEvent struct {
	RemoteURL string   `json:"remoteUrl"`
	Stage     string   `json:"stage"`
	Percent   *float64 `json:"percent,omitempty"`
	Message   string   `json:"message,omitempty"`
}

// cloneProgressHub broadcasts clone progress events to SSE subscribers.
// Delivery is best-effort: a slow subscriber drops updates rather than
// stalling the clone itself, and the hub holds no state so a subscriber that
// attaches late simply joins the next update.
type cloneProgressHub struct {
	mu   sync.Mutex
	subs map[chan CloneProgressEvent]struct{}
}

// subscribe registers a subscriber whose channel is removed when ctx ends.
// The channel is buffered so publishes never block on a live clone.
func (h *cloneProgressHub) subscribe(ctx context.Context) <-chan CloneProgressEvent {
	ch := make(chan CloneProgressEvent, 16)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[chan CloneProgressEvent]struct{})
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()
	return ch
}

func (h *cloneProgressHub) publish(event CloneProgressEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- event:
		default:
			// The subscriber fell behind; drop this frame instead of
			// blocking the clone goroutine.
		}
	}
}
