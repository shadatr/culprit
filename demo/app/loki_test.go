package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type lokiSink struct {
	mu     sync.Mutex
	pushes []lokiPush
	status int
	block  chan struct{}
}

func (s *lokiSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.block != nil {
		<-s.block
	}
	if r.URL.Path != "/loki/api/v1/push" || r.Method != http.MethodPost {
		http.Error(w, "bad path", http.StatusNotFound)
		return
	}
	var p lokiPush
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.pushes = append(s.pushes, p)
	code := s.status
	s.mu.Unlock()
	if code == 0 {
		code = http.StatusNoContent
	}
	w.WriteHeader(code)
}

func (s *lokiSink) lines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.pushes {
		for _, st := range p.Streams {
			n += len(st.Values)
		}
	}
	return n
}

func (s *lokiSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pushes)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLokiBatchBySize(t *testing.T) {
	sink := &lokiSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	p := NewLokiPusher(LokiConfig{URL: srv.URL + "/", Labels: map[string]string{"service": "catalog"}, BatchSize: 3, Interval: time.Hour})
	defer p.Close(context.Background())
	for range 6 {
		_, _ = p.Write([]byte(`{"level":"INFO","msg":"x"}` + "\n"))
	}
	waitFor(t, func() bool { return sink.count() == 2 })
	if got := sink.lines(); got != 6 {
		t.Fatalf("lines = %d, want 6", got)
	}
}

func TestLokiFlushOnInterval(t *testing.T) {
	sink := &lokiSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	p := NewLokiPusher(LokiConfig{URL: srv.URL, BatchSize: 100, Interval: 20 * time.Millisecond})
	defer p.Close(context.Background())
	_, _ = p.Write([]byte(`{"level":"WARN","msg":"x"}`))
	waitFor(t, func() bool { return sink.lines() == 1 })
}

func TestLokiCloseFlushes(t *testing.T) {
	sink := &lokiSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	p := NewLokiPusher(LokiConfig{URL: srv.URL, BatchSize: 100, Interval: time.Hour})
	for range 5 {
		_, _ = p.Write([]byte(`{"level":"INFO"}`))
	}
	p.Close(context.Background())
	if got := sink.lines(); got != 5 {
		t.Fatalf("lines after close = %d, want 5", got)
	}
	if p.Sent() != 5 {
		t.Fatalf("sent = %d, want 5", p.Sent())
	}
	_, _ = p.Write([]byte(`{"level":"INFO"}`))
	if p.Dropped() != 1 {
		t.Fatalf("write after close should drop, dropped = %d", p.Dropped())
	}
}

func TestLokiGroupsStreamsByLevel(t *testing.T) {
	ts := time.Unix(100, 5)
	batch := []lokiEntry{
		{t: ts, level: "info", line: "a"},
		{t: ts, level: "error", line: "b"},
		{t: ts, level: "info", line: "c"},
	}
	got := buildPush(map[string]string{"service": "cart"}, batch)
	if len(got.Streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(got.Streams))
	}
	info := got.Streams[0]
	if info.Stream["level"] != "info" || info.Stream["service"] != "cart" {
		t.Fatalf("labels = %v", info.Stream)
	}
	if len(info.Values) != 2 || info.Values[0][1] != "a" || info.Values[1][1] != "c" {
		t.Fatalf("values = %v", info.Values)
	}
	if info.Values[0][0] != "100000000005" {
		t.Fatalf("timestamp = %s", info.Values[0][0])
	}
}

func TestLokiFailureDropsAndCounts(t *testing.T) {
	sink := &lokiSink{status: http.StatusInternalServerError}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	p := NewLokiPusher(LokiConfig{URL: srv.URL, BatchSize: 2, Interval: time.Hour})
	_, _ = p.Write([]byte(`{"level":"INFO"}`))
	_, _ = p.Write([]byte(`{"level":"INFO"}`))
	waitFor(t, func() bool { return p.Dropped() == 2 })
	p.Close(context.Background())
	if p.Sent() != 0 {
		t.Fatalf("sent = %d, want 0", p.Sent())
	}
}

func TestLokiWriteNeverBlocks(t *testing.T) {
	sink := &lokiSink{block: make(chan struct{})}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	defer close(sink.block)
	p := NewLokiPusher(LokiConfig{URL: srv.URL, BatchSize: 1, Interval: time.Hour, Buffer: 4,
		Client: &http.Client{Timeout: 200 * time.Millisecond}})
	start := time.Now()
	for range 1000 {
		if n, err := p.Write([]byte(`{"level":"INFO"}`)); err != nil || n == 0 {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("writes took %v with a stuck Loki", el)
	}
	if p.Dropped() == 0 {
		t.Fatal("expected drops with a full queue")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.Close(ctx) // must return even though Loki is stuck
}

func TestLevelOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"level":"INFO"}`, "info"},
		{`{"level":"WARN"}`, "warn"},
		{`{"level":"ERROR"}`, "error"},
		{`{"level":"DEBUG"}`, "debug"},
		{`{"level":"INFO+2"}`, "info+2"},
		{`not json`, "unknown"},
		{`{}`, "unknown"},
	}
	for _, tt := range tests {
		if got := levelOf([]byte(tt.in)); got != tt.want {
			t.Errorf("levelOf(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
