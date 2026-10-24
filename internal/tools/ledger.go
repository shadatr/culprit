package tools

import (
	"strconv"
	"sync"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

// Ledger records evidence for one investigation and assigns IDs E1, E2, ...
// The zero value is ready to use and it is safe for concurrent use.
type Ledger struct {
	mu    sync.Mutex
	items []core.Evidence
	byID  map[string]int
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger { return &Ledger{} }

// Add stores e under the next ID and returns that ID. Any ID already set on
// e is replaced; a zero At is set to the current time.
func (l *Ledger) Add(e core.Evidence) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = make(map[string]int)
	}
	e.ID = "E" + strconv.Itoa(len(l.items)+1)
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	l.byID[e.ID] = len(l.items)
	l.items = append(l.items, e)
	return e.ID
}

// List returns all evidence in insertion order.
func (l *Ledger) List() []core.Evidence {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]core.Evidence(nil), l.items...)
}

// Get returns the evidence with id.
func (l *Ledger) Get(id string) (core.Evidence, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, ok := l.byID[id]
	if !ok {
		return core.Evidence{}, false
	}
	return l.items[i], true
}

// Has reports whether id exists.
func (l *Ledger) Has(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.byID[id]
	return ok
}

// Len returns the number of evidence items.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}
