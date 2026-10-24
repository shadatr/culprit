package server

import (
	"sync"

	"github.com/shadatr/culprit/internal/core"
)

// subscriber receives live events for one investigation. When its buffer is
// full, events are dropped and lagged is signalled; the reader then resyncs
// from the store, which already holds every event.
type subscriber struct {
	ch     chan core.Event
	lagged chan struct{} // capacity 1
}

// broker fans events out to live SSE subscribers per investigation.
type broker struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
	buf  int
}

func newBroker(buf int) *broker {
	if buf <= 0 {
		buf = 64
	}
	return &broker{subs: map[string]map[*subscriber]struct{}{}, buf: buf}
}

// subscribe registers a subscriber for id. Call the returned func to remove it.
func (b *broker) subscribe(id string) (*subscriber, func()) {
	s := &subscriber{ch: make(chan core.Event, b.buf), lagged: make(chan struct{}, 1)}
	b.mu.Lock()
	m := b.subs[id]
	if m == nil {
		m = map[*subscriber]struct{}{}
		b.subs[id] = m
	}
	m[s] = struct{}{}
	b.mu.Unlock()
	return s, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[id], s)
		if len(b.subs[id]) == 0 {
			delete(b.subs, id)
		}
	}
}

// publish sends ev to every subscriber of id without blocking.
func (b *broker) publish(id string, ev core.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[id] {
		select {
		case s.ch <- ev:
		default:
			select {
			case s.lagged <- struct{}{}:
			default:
			}
		}
	}
}

// count returns the number of subscribers of id (tests).
func (b *broker) count(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[id])
}
