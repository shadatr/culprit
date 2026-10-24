// Command app is the Culprit demo shop. One binary, four roles:
// frontend, catalog, cart and loadgen.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

func main() {
	role := flag.String("role", os.Getenv("ROLE"), "frontend | catalog | cart | loadgen")
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	health := flag.Bool("healthcheck", false, "probe http://127.0.0.1<addr>/healthz and exit (for container healthchecks)")
	flag.Parse()

	if *health {
		os.Exit(healthcheck(*addr))
	}
	if err := run(*role, *addr); err != nil {
		fmt.Fprintln(os.Stderr, "app:", err)
		os.Exit(1)
	}
}

func run(role, addr string) error {
	if _, ok := knownFaults[role]; !ok && role != "loadgen" {
		return fmt.Errorf("unknown role %q", role)
	}
	version := envOr("APP_VERSION", "dev")
	log, pusher := newLogger(role, version, os.Getenv("LOKI_URL"))
	defer func() {
		if pusher != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			pusher.Close(ctx)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if role == "loadgen" {
		log.Info("loadgen starting", "target", envOr("TARGET_URL", "http://frontend:8080"))
		return runLoadgen(ctx, log, envOr("TARGET_URL", "http://frontend:8080"), envFloat("RPS", 12))
	}

	faults, err := NewFaults(role, os.Getenv("FAULT"))
	if err != nil {
		return err
	}
	m := newMetrics(role)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	admin := AdminHandler(faults, os.Getenv("ADMIN_TOKEN"), log)
	mux.HandleFunc("GET /admin/fault", admin)
	mux.HandleFunc("POST /admin/fault", admin)

	var cleanup func()
	switch role {
	case "frontend":
		err = setupFrontend(ctx, mux, log, faults)
	case "catalog":
		cleanup, err = setupCatalog(ctx, mux, log, faults, m)
	case "cart":
		cleanup, err = setupCart(ctx, mux, log, m)
	}
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	go noise(ctx, role, log)

	srv := &http.Server{
		Addr:              addr,
		Handler:           m.Instrument(mux, log),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("service starting", "addr", addr, "app_version", version, "go_version", runtime.Version())
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}
	return nil
}

func healthcheck(addr string) int {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1" + portOf(addr) + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	var f float64
	if _, err := fmt.Sscanf(os.Getenv(k), "%g", &f); err == nil && f > 0 {
		return f
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil && d > 0 {
		return d
	}
	return def
}
