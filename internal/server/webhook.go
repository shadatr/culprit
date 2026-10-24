package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/store"
)

// alertmanagerPayload is the Alertmanager webhook body (version 4).
type alertmanagerPayload struct {
	Version     string              `json:"version"`
	GroupKey    string              `json:"groupKey"`
	Status      string              `json:"status"`
	Receiver    string              `json:"receiver"`
	ExternalURL string              `json:"externalURL"`
	Alerts      []alertmanagerAlert `json:"alerts"`
}

type alertmanagerAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// webhookResult reports what the webhook did with each firing alert.
type webhookResult struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"` // IDs of existing investigations for duplicate alerts
}

func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	var p alertmanagerPayload
	if !decodeJSON(w, r, &p) {
		return
	}
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	res := webhookResult{Created: []string{}, Skipped: []string{}}
	now := s.now()
	for _, a := range p.Alerts {
		if a.Status != "firing" {
			continue
		}
		fp := a.Fingerprint
		if fp == "" {
			fp = labelFingerprint(a.Labels)
		}
		existing, err := s.store.FindRecentByFingerprint(r.Context(), fp, now.Add(-dedupeWindow))
		switch {
		case err == nil:
			res.Skipped = append(res.Skipped, existing.ID)
			continue
		case !errors.Is(err, store.ErrNotFound):
			s.log.Error("webhook dedupe", "err", err)
			writeError(w, http.StatusInternalServerError, "could not check for duplicates")
			return
		}
		inc := s.alertIncident(a, now)
		inv := &store.Investigation{Incident: inc, Fingerprint: fp}
		if err := s.submit(r.Context(), inv); err != nil {
			if errors.Is(err, errQueueFull) {
				// Stored and failed; report it as created so the caller sees it.
				res.Created = append(res.Created, inv.ID)
				continue
			}
			s.writeSubmitError(w, err)
			return
		}
		res.Created = append(res.Created, inv.ID)
	}
	s.log.Info("alertmanager webhook", "alerts", len(p.Alerts), "created", len(res.Created), "skipped", len(res.Skipped))
	writeJSON(w, http.StatusAccepted, res)
}

// alertIncident builds an incident from one firing alert.
func (s *Server) alertIncident(a alertmanagerAlert, now time.Time) core.Incident {
	title := strings.TrimSpace(a.Annotations["summary"])
	if title == "" {
		title = a.Labels["alertname"]
	}
	if title == "" {
		title = "Alertmanager alert"
	}
	service := a.Labels["service"]
	if service == "" {
		service = a.Labels["job"]
	}
	start := a.StartsAt
	if start.IsZero() || start.After(now) {
		start = now
	}
	labels := make(map[string]string, len(a.Labels))
	for k, v := range a.Labels {
		labels[k] = v
	}
	inc := core.Incident{
		Title:     title,
		Service:   service,
		StartedAt: start,
		Labels:    labels,
		Source:    "alertmanager",
	}
	s.defaultWindow(&inc)
	if !inc.Window.End.After(inc.Window.Start) {
		inc.Window.End = inc.Window.Start.Add(time.Second)
	}
	return inc
}

// labelFingerprint hashes sorted labels, for alerts without a fingerprint.
func labelFingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
