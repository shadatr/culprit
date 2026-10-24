package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
)

// action is one kind of user request with a relative weight.
type action struct {
	weight int
	do     func(ctx context.Context, hc *http.Client, base string) (int, error)
}

func get(ctx context.Context, hc *http.Client, u string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	return send(hc, req)
}

func send(hc *http.Client, req *http.Request) (int, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// A small pool of active shoppers keeps the cart summary cache warm.
func randomUser() string { return fmt.Sprintf("u%03d", rand.IntN(50)) }

var actions = []action{
	{40, func(ctx context.Context, hc *http.Client, base string) (int, error) { return get(ctx, hc, base+"/") }},
	{30, func(ctx context.Context, hc *http.Client, base string) (int, error) {
		id := 1 + rand.IntN(productCount)
		if rand.Float64() < 0.04 { // stale links from search engines
			id = productCount + 1 + rand.IntN(900)
		}
		return get(ctx, hc, fmt.Sprintf("%s/product/%d", base, id))
	}},
	{7, func(ctx context.Context, hc *http.Client, base string) (int, error) {
		body := fmt.Sprintf(`{"user":%q,"product_id":%d,"qty":%d}`, randomUser(), 1+rand.IntN(productCount), 1+rand.IntN(3))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/cart/add", bytes.NewBufferString(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		return send(hc, req)
	}},
	{22, func(ctx context.Context, hc *http.Client, base string) (int, error) {
		return get(ctx, hc, base+"/cart?user="+randomUser())
	}},
	{1, func(ctx context.Context, hc *http.Client, base string) (int, error) {
		return get(ctx, hc, base+"/favicon.ico") // always 404
	}},
}

func pick() action {
	total := 0
	for _, a := range actions {
		total += a.weight
	}
	n := rand.IntN(total)
	for _, a := range actions {
		if n < a.weight {
			return a
		}
		n -= a.weight
	}
	return actions[0]
}

// currentRate varies the base rate slowly (a 10-minute wave of +/-20%) with a
// little per-tick noise so traffic looks organic but stays steady.
func currentRate(base float64, t time.Time) float64 {
	phase := float64(t.Unix()%600) / 600 * 2 * math.Pi
	r := base * (1 + 0.2*math.Sin(phase)) * (0.9 + 0.2*rand.Float64())
	return math.Max(r, 0.5)
}

func runLoadgen(ctx context.Context, log *slog.Logger, base string, rps float64) error {
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	sem := make(chan struct{}, 64)
	var ok, fail, dropped atomic.Int64
	report := time.NewTicker(time.Minute)
	defer report.Stop()
	for {
		wait := time.Duration(float64(time.Second) * rand.ExpFloat64() / currentRate(rps, time.Now()))
		select {
		case <-ctx.Done():
			return nil
		case <-report.C:
			log.Info("loadgen progress", "ok", ok.Swap(0), "failed", fail.Swap(0), "dropped", dropped.Swap(0))
			continue
		case <-time.After(wait):
		}
		select {
		case sem <- struct{}{}:
		default:
			dropped.Add(1)
			continue
		}
		go func() {
			defer func() { <-sem }()
			code, err := pick().do(ctx, hc, base)
			if err != nil || code >= 500 {
				fail.Add(1)
				return
			}
			ok.Add(1)
		}()
	}
}
