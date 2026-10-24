// Package server exposes investigations over HTTP (PLAN.md §4.8): a JSON API,
// a Server-Sent Events stream per investigation and an Alertmanager webhook.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/shadatr/culprit/internal/config"
	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/store"
)

// Investigator runs one investigation. *agent.Investigator implements it.
type Investigator interface {
	Run(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error)
}

// Options configures a Server. Store, Investigator and Config are required.
type Options struct {
	Store            *store.Store
	Investigator     Investigator
	Config           *config.Config
	Logger           *slog.Logger     // default slog.Default()
	Now              func() time.Time // default time.Now
	Heartbeat        time.Duration    // SSE heartbeat interval, default 15s
	QueueSize        int              // pending investigations beyond running ones, default 256
	SubscriberBuffer int              // live events buffered per SSE client, default 64
}

// Server runs investigations and serves the HTTP API. Create it with New.
type Server struct {
	store     *store.Store
	inv       Investigator
	cfg       *config.Config
	log       *slog.Logger
	now       func() time.Time
	heartbeat time.Duration
	timeout   time.Duration

	broker  *broker
	handler http.Handler

	jobs       chan string
	baseCtx    context.Context
	cancelBase context.CancelFunc
	workers    sync.WaitGroup

	webhookMu sync.Mutex // serialises webhook dedupe

	mu      sync.Mutex
	closing bool
	quit    chan struct{} // closed by Shutdown
	httpSrv *http.Server
}

const (
	maxBodyBytes  = 1 << 20
	dedupeWindow  = 30 * time.Minute
	defaultAfter  = 30 * time.Minute
	persistBudget = 10 * time.Second
)

// New builds a Server, fails investigations left queued or running by a
// previous process, and starts the worker pool.
func New(opts Options) (*Server, error) {
	if opts.Store == nil || opts.Investigator == nil || opts.Config == nil {
		return nil, errors.New("server: Store, Investigator and Config are required")
	}
	s := &Server{
		store:     opts.Store,
		inv:       opts.Investigator,
		cfg:       opts.Config,
		log:       opts.Logger,
		now:       opts.Now,
		heartbeat: opts.Heartbeat,
		broker:    newBroker(opts.SubscriberBuffer),
		quit:      make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.heartbeat <= 0 {
		s.heartbeat = 15 * time.Second
	}
	s.timeout = 5 * time.Minute
	if d := s.cfg.Budgets.MaxDuration; d > 0 {
		s.timeout = d + time.Minute
	}
	qs := opts.QueueSize
	if qs <= 0 {
		qs = 256
	}
	s.jobs = make(chan string, qs)

	if err := s.recover(context.Background()); err != nil {
		return nil, err
	}

	s.baseCtx, s.cancelBase = context.WithCancel(context.Background())
	n := s.cfg.Server.Concurrency
	if n <= 0 {
		n = 1
	}
	for range n {
		s.workers.Add(1)
		go s.worker()
	}
	s.handler = s.routes()
	return s, nil
}

// recover fails investigations interrupted by a crash or restart.
func (s *Server) recover(ctx context.Context) error {
	const msg = "interrupted by restart"
	ids, err := s.store.MarkInterrupted(ctx, msg)
	if err != nil {
		return fmt.Errorf("server: recover: %w", err)
	}
	for _, id := range ids {
		s.log.Warn("investigation interrupted", "id", id)
		s.emit(id, core.EventError, map[string]string{"message": msg})
		s.emit(id, core.EventFinished, map[string]string{"status": core.StatusFailed})
	}
	return nil
}

// Handler returns the HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler { return s.handler }

// ListenAndServe listens on addr and serves until Shutdown. It returns nil
// after a clean shutdown.
func (s *Server) ListenAndServe(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", addr, err)
	}
	return s.Serve(l)
}

// Serve serves on l until Shutdown. It returns nil after a clean shutdown.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		l.Close()
		return http.ErrServerClosed
	}
	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: SSE streams are long-lived.
	}
	s.httpSrv = hs
	s.mu.Unlock()
	s.log.Info("listening", "addr", l.Addr().String())
	if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// Shutdown stops accepting requests and new investigations, ends SSE
// streams, and waits for running investigations. When ctx ends first, it
// cancels the running investigations and waits for them to record their
// failure. Investigations still queued stay queued and are failed on the next
// start. It returns ctx.Err() if investigations had to be cancelled.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	close(s.quit)
	hs := s.httpSrv
	s.mu.Unlock()

	var errs []error
	if hs != nil {
		if err := hs.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("server: http shutdown: %w", err))
		}
	}
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("shutdown grace expired; cancelling running investigations")
		s.cancelBase()
		<-done
		errs = append(errs, ctx.Err())
	}
	s.cancelBase()
	return errors.Join(errs...)
}

// submit stores a new investigation and queues it.
func (s *Server) submit(ctx context.Context, inv *store.Investigation) error {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return errShuttingDown
	}
	inv.Status = core.StatusQueued
	if err := s.store.Create(ctx, inv); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		// Left queued; the next start fails it.
		return nil
	}
	select {
	case s.jobs <- inv.ID:
		return nil
	default:
	}
	go s.fail(inv.ID, "queue full")
	return errQueueFull
}

var (
	errShuttingDown = errors.New("server is shutting down")
	errQueueFull    = errors.New("investigation queue is full")
)

func (s *Server) worker() {
	defer s.workers.Done()
	for {
		select {
		case <-s.quit:
			return
		case id := <-s.jobs:
			select {
			case <-s.quit:
				return // leave it queued
			default:
			}
			s.run(id)
		}
	}
}

func (s *Server) persistCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), persistBudget)
}

// emit persists one server-generated event and publishes it.
func (s *Server) emit(id, typ string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		s.log.Error("encode event", "id", id, "type", typ, "err", err)
		return
	}
	s.record(id, core.Event{Type: typ, At: s.now(), Data: b})
}

// record persists ev and publishes it to live subscribers.
func (s *Server) record(id string, ev core.Event) {
	ctx, cancel := s.persistCtx()
	defer cancel()
	stored, err := s.store.AppendEvent(ctx, id, ev)
	if err != nil {
		s.log.Error("store event", "id", id, "type", ev.Type, "err", err)
		return
	}
	s.broker.publish(id, stored)
}

func (s *Server) fail(id, msg string) {
	ctx, cancel := s.persistCtx()
	defer cancel()
	if err := s.store.SetError(ctx, id, msg); err != nil {
		s.log.Error("store error", "id", id, "err", err)
	}
	s.emit(id, core.EventError, map[string]string{"message": msg})
	s.emit(id, core.EventFinished, map[string]string{"status": core.StatusFailed})
}

// run executes one investigation and records its outcome.
func (s *Server) run(id string) {
	pctx, pcancel := s.persistCtx()
	inv, err := s.store.Get(pctx, id)
	if err == nil {
		err = s.store.UpdateStatus(pctx, id, core.StatusRunning)
	}
	pcancel()
	if err != nil {
		s.log.Error("start investigation", "id", id, "err", err)
		s.fail(id, "could not start: "+err.Error())
		return
	}
	log := s.log.With("id", id)
	log.Info("investigation started", "title", inv.Incident.Title, "service", inv.Incident.Service)
	started := s.now()

	// The agent may or may not emit report/error/finished itself; the server
	// owns finished and fills in report/error when the agent did not.
	var sawReport, sawError bool
	sink := func(ev core.Event) {
		switch ev.Type {
		case core.EventFinished:
			return
		case core.EventReport:
			sawReport = true
		case core.EventError:
			sawError = true
		}
		s.record(id, ev)
	}

	ctx, cancel := context.WithTimeout(s.baseCtx, s.timeout)
	rep, runErr := s.safeRun(ctx, inv.Incident, sink)
	cancel()

	pctx, pcancel = s.persistCtx()
	defer pcancel()
	status := core.StatusFailed
	if rep != nil {
		if rep.IncidentID == "" {
			rep.IncidentID = id
		}
		if err := s.store.SaveReport(pctx, id, rep); err != nil {
			log.Error("save report", "err", err)
		}
		status = rep.Status
		if status == "" {
			status = core.StatusCompleted
		}
		if !sawReport {
			s.emit(id, core.EventReport, rep)
		}
	}
	if runErr == nil && rep == nil {
		runErr = errors.New("investigator returned no report")
	}
	if runErr != nil {
		if err := s.store.SetError(pctx, id, runErr.Error()); err != nil {
			log.Error("save error", "err", err)
		}
		if !sawError {
			s.emit(id, core.EventError, map[string]string{"message": runErr.Error()})
		}
	}
	s.emit(id, core.EventFinished, map[string]string{"status": status})
	log.Info("investigation finished", "status", status, "duration", s.now().Sub(started).Round(time.Millisecond), "err", runErr)
}

// safeRun calls the investigator, converting a panic into an error.
func (s *Server) safeRun(ctx context.Context, inc core.Incident, sink core.EventSink) (rep *core.Report, err error) {
	defer func() {
		if p := recover(); p != nil {
			rep, err = nil, fmt.Errorf("investigator panic: %v", p)
		}
	}()
	return s.inv.Run(ctx, inc, sink)
}
