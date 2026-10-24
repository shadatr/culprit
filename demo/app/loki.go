package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// LokiConfig configures a LokiPusher.
type LokiConfig struct {
	URL       string            // base URL, e.g. http://loki:3100
	Labels    map[string]string // static stream labels (service); level is added per line
	BatchSize int               // flush when this many lines are buffered (default 200)
	Interval  time.Duration     // flush at least this often (default 1s)
	Buffer    int               // queued lines before new ones are dropped (default 5000)
	Client    *http.Client      // default: 3s timeout
}

type lokiEntry struct {
	t     time.Time
	level string
	line  string
}

// LokiPusher ships log lines to Loki's push API. It implements io.Writer so it
// can sit behind slog.NewJSONHandler. It never blocks the caller: when the
// queue is full, or a push fails, lines are dropped and counted.
type LokiPusher struct {
	cfg     LokiConfig
	pushURL string
	ch      chan lokiEntry
	done    chan struct{}
	closing sync.Once
	wg      sync.WaitGroup

	dropped atomic.Int64
	sent    atomic.Int64
	now     func() time.Time

	errOut       io.Writer // push failures are reported here, at most every 30s
	lastReported time.Time // only touched by the loop goroutine
}

// NewLokiPusher starts the background flusher. Call Close to flush and stop.
func NewLokiPusher(cfg LokiConfig) *LokiPusher {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 5000
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 3 * time.Second}
	}
	p := &LokiPusher{
		cfg:     cfg,
		pushURL: trimSlash(cfg.URL) + "/loki/api/v1/push",
		ch:      make(chan lokiEntry, cfg.Buffer),
		done:    make(chan struct{}),
		now:     time.Now,
		errOut:  os.Stderr,
	}
	p.wg.Add(1)
	go p.loop()
	return p
}

// Write enqueues one log record (slog handlers write one record per call).
// It always reports success so logging to stdout is never affected.
func (p *LokiPusher) Write(b []byte) (int, error) {
	line := string(bytes.TrimRight(b, "\n"))
	e := lokiEntry{t: p.now(), level: levelOf(b), line: line}
	select {
	case <-p.done:
		p.dropped.Add(1)
	default:
		select {
		case p.ch <- e:
		default:
			p.dropped.Add(1)
		}
	}
	return len(b), nil
}

// Dropped returns how many lines were discarded.
func (p *LokiPusher) Dropped() int64 { return p.dropped.Load() }

// Sent returns how many lines Loki accepted.
func (p *LokiPusher) Sent() int64 { return p.sent.Load() }

// Close flushes buffered lines (bounded by ctx) and stops the flusher.
func (p *LokiPusher) Close(ctx context.Context) {
	p.closing.Do(func() { close(p.done) })
	finished := make(chan struct{})
	go func() { p.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
	}
}

func (p *LokiPusher) loop() {
	defer p.wg.Done()
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	batch := make([]lokiEntry, 0, p.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.push(batch)
		batch = batch[:0]
	}
	for {
		select {
		case e := <-p.ch:
			batch = append(batch, e)
			if len(batch) >= p.cfg.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		case <-p.done:
			for {
				select {
				case e := <-p.ch:
					batch = append(batch, e)
					if len(batch) >= p.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type lokiPush struct {
	Streams []lokiStream `json:"streams"`
}

// buildPush groups entries into one stream per level, preserving order.
func buildPush(static map[string]string, batch []lokiEntry) lokiPush {
	idx := map[string]int{}
	var out lokiPush
	for _, e := range batch {
		i, ok := idx[e.level]
		if !ok {
			labels := make(map[string]string, len(static)+1)
			for k, v := range static {
				labels[k] = v
			}
			labels["level"] = e.level
			out.Streams = append(out.Streams, lokiStream{Stream: labels})
			i = len(out.Streams) - 1
			idx[e.level] = i
		}
		out.Streams[i].Values = append(out.Streams[i].Values,
			[2]string{strconv.FormatInt(e.t.UnixNano(), 10), e.line})
	}
	return out
}

func (p *LokiPusher) push(batch []lokiEntry) {
	body, err := json.Marshal(buildPush(p.cfg.Labels, batch))
	if err != nil {
		p.dropped.Add(int64(len(batch)))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.Client.Timeout+time.Second)
	defer cancel()
	if err := p.post(ctx, body); err != nil {
		p.dropped.Add(int64(len(batch)))
		p.reportFailure(err)
		return
	}
	p.sent.Add(int64(len(batch)))
}

// reportFailure writes a push error to errOut, rate limited. It deliberately
// bypasses slog so a broken Loki cannot feed its own error lines back in.
func (p *LokiPusher) reportFailure(err error) {
	if p.errOut == nil || p.now().Sub(p.lastReported) < 30*time.Second {
		return
	}
	p.lastReported = p.now()
	fmt.Fprintf(p.errOut, "loki pusher: %v (%d lines dropped so far)\n", err, p.dropped.Load())
}

func (p *LokiPusher) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("loki: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("loki: push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("loki: push: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// levelOf extracts slog's "level" field and lowercases it (INFO -> info).
func levelOf(b []byte) string {
	var v struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Level == "" {
		return "unknown"
	}
	switch v.Level {
	case "DEBUG":
		return "debug"
	case "INFO":
		return "info"
	case "WARN":
		return "warn"
	case "ERROR":
		return "error"
	}
	return bytesLower(v.Level)
}

func bytesLower(s string) string { return string(bytes.ToLower([]byte(s))) }

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
