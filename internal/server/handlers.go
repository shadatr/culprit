package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/store"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/investigations", s.handleCreate)
	mux.HandleFunc("GET /api/investigations", s.handleList)
	mux.HandleFunc("GET /api/investigations/{id}", s.handleGet)
	mux.HandleFunc("GET /api/investigations/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /api/webhooks/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return s.logRequests(s.recoverPanics(s.cors(limitBody(mux))))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var inc core.Incident
	if !decodeJSON(w, r, &inc) {
		return
	}
	if err := s.normalizeIncident(&inc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if inc.ID != "" && !idPattern.MatchString(inc.ID) {
		writeError(w, http.StatusBadRequest, "id must be 1-64 characters of letters, digits, '_', '-' or '.'")
		return
	}
	inv := &store.Investigation{ID: inc.ID, Incident: inc}
	if err := s.submit(r.Context(), inv); err != nil {
		s.writeSubmitError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": inv.ID})
}

func (s *Server) writeSubmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "investigation id already exists")
	case errors.Is(err, errShuttingDown), errors.Is(err, errQueueFull):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.log.Error("create investigation", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create investigation")
	}
}

// normalizeIncident validates inc and fills defaults: a window of the
// configured lookback before StartedAt up to the earlier of StartedAt+30m and
// now, and Source "manual".
func (s *Server) normalizeIncident(inc *core.Incident) error {
	inc.Title = strings.TrimSpace(inc.Title)
	inc.Service = strings.TrimSpace(inc.Service)
	if inc.Title == "" {
		return errors.New("title is required")
	}
	if inc.StartedAt.IsZero() {
		return errors.New("started_at is required")
	}
	s.defaultWindow(inc)
	if !inc.Window.End.After(inc.Window.Start) {
		return errors.New("window.end must be after window.start")
	}
	if inc.Source == "" {
		inc.Source = "manual"
	}
	return nil
}

func (s *Server) defaultWindow(inc *core.Incident) {
	if inc.Window.Start.IsZero() {
		inc.Window.Start = inc.StartedAt.Add(-s.cfg.Server.Lookback)
	}
	if inc.Window.End.IsZero() {
		end := inc.StartedAt.Add(defaultAfter)
		if now := s.now(); now.Before(end) {
			end = now
		}
		inc.Window.End = end
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 500 {
		writeError(w, http.StatusBadRequest, "limit must be an integer in 1..500")
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}
	list, err := s.store.List(r.Context(), limit, offset)
	if err != nil {
		s.log.Error("list investigations", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list investigations")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*store.Investigation, bool) {
	inv, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "investigation not found")
		return nil, false
	}
	if err != nil {
		s.log.Error("get investigation", "err", err)
		writeError(w, http.StatusInternalServerError, "could not load investigation")
		return nil, false
	}
	return inv, true
}

// handleEvents streams events as SSE: stored events after Last-Event-ID (or
// ?after=), then live ones, ending after the finished event.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.lookup(w, r)
	if !ok {
		return
	}
	after := 0
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "Last-Event-ID / after must be a non-negative integer")
			return
		}
		after = n
	}

	// Subscribe before replaying so nothing published in between is missed;
	// duplicates are skipped by sequence number.
	sub, unsubscribe := s.broker.subscribe(inv.ID)
	defer unsubscribe()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	last := after
	// write sends ev; it reports whether the stream should continue.
	write := func(ev core.Event) (cont bool, err error) {
		if ev.Seq <= last {
			return true, nil
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return false, err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b); err != nil {
			return false, err
		}
		last = ev.Seq
		return ev.Type != core.EventFinished, nil
	}
	resync := func() bool {
		evs, err := s.store.Events(r.Context(), inv.ID, last)
		if err != nil {
			s.log.Warn("sse replay", "id", inv.ID, "err", err)
			return false
		}
		for _, ev := range evs {
			cont, err := write(ev)
			if err != nil || !cont {
				rc.Flush()
				return false
			}
		}
		return rc.Flush() == nil
	}
	if !resync() {
		return
	}

	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.quit:
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-sub.lagged:
			if !resync() {
				return
			}
		case ev := <-sub.ch:
			cont, err := write(ev)
			if err != nil || rc.Flush() != nil || !cont {
				return
			}
		}
	}
}

func queryInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}

// decodeJSON decodes the body into v, writing an error response on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
