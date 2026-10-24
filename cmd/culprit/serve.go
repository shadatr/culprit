package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shadatr/culprit/internal/app"
	"github.com/shadatr/culprit/internal/server"
	"github.com/shadatr/culprit/internal/store"
)

// serveShutdownGrace is how long running investigations get to finish after
// SIGINT/SIGTERM before they are cancelled.
const serveShutdownGrace = 30 * time.Second

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file (default: built-in defaults)")
	addr := fs.String("addr", "", "listen address (overrides server.addr)")
	scenario := fs.String("scenario", "", "serve every investigation over this fixture scenario directory instead of live sources")
	model := fs.String("model", "", "override the configured LLM model")
	dbPath := fs.String("db", "", "SQLite database path (overrides server.db_path)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: culprit serve [-config file] [-addr :8090] [-scenario dir] [-db path]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	cfg, err := loadConfigFlag(*cfgPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	if *dbPath != "" {
		cfg.Server.DBPath = *dbPath
	}
	if *model != "" {
		cfg.LLM.Model = *model
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := app.LLM(cfg)
	var inv server.Investigator
	if *scenario != "" {
		i, sc, err := app.Scenario(cfg, client, *scenario)
		if err != nil {
			return err
		}
		inv = i
		log.Info("serving fixture scenario", "scenario", sc.Name, "dir", *scenario)
	} else {
		i, err := app.Live(cfg, client)
		if err != nil {
			return err
		}
		inv = i
	}

	st, err := store.Open(cfg.Server.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	srv, err := server.New(server.Options{Store: st, Investigator: inv, Config: cfg, Logger: log})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe(cfg.Server.Addr) }()

	select {
	case err := <-errc:
		// Listener failed; stop workers before returning.
		sctx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return err
	case <-ctx.Done():
	}
	stop() // a second signal kills the process
	log.Info("shutting down", "grace", serveShutdownGrace)
	sctx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("serve: shutdown: %w", err)
	}
	if err := <-errc; err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}
