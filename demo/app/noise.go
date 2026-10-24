package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// benign log lines that show up in a healthy system. They are red herrings: an
// investigator should notice they predate the incident and ignore them.
var noiseLines = map[string][]func(*slog.Logger){
	"frontend": {
		func(l *slog.Logger) { l.Warn("client sent deprecated header", "header", "X-Shop-Legacy-Session") },
		func(l *slog.Logger) {
			l.Warn("TLS certificate for edge proxy expires soon", "days_left", 61, "subject", "shop.example.internal")
		},
		func(l *slog.Logger) { l.Info("feature flags refreshed", "flags", 14) },
		func(l *slog.Logger) {
			l.Warn("slow template render", "template", "home", "duration_ms", 180+rand.IntN(90))
		},
	},
	"catalog": {
		func(l *slog.Logger) { l.Warn("price feed lag above threshold", "lag_s", 35+rand.IntN(30)) },
		func(l *slog.Logger) { l.Info("search index refresh completed", "docs", productCount) },
		func(l *slog.Logger) {
			l.Warn("image CDN returned 404 for thumbnail", "product_id", 1+rand.IntN(productCount))
		},
	},
	"cart": {
		func(l *slog.Logger) {
			l.Warn("abandoned cart sweeper skipped run", "reason", "previous run still active")
		},
		func(l *slog.Logger) { l.Info("cart TTL policy loaded", "ttl", "1h") },
		func(l *slog.Logger) { l.Warn("promo code lookup slow", "duration_ms", 120+rand.IntN(200)) },
	},
}

// noise emits an occasional benign line until ctx ends.
func noise(ctx context.Context, role string, log *slog.Logger) {
	lines := noiseLines[role]
	if len(lines) == 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(15*time.Second, 45*time.Second)):
			lines[rand.IntN(len(lines))](log)
		}
	}
}
