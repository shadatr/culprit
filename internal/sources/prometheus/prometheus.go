// Package prometheus reads metrics from the Prometheus HTTP API.
package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/sources/fixture"
)

const (
	// MaxSeries caps how many series one Query returns.
	MaxSeries = 200
	// targetPoints is the number of samples per series aimed for when no
	// step is given.
	targetPoints = 240
	minStep      = 15 * time.Second
	maxBodyBytes = 64 << 20
)

// Client implements core.MetricsSource.
type Client struct {
	baseURL string
	hc      *http.Client
}

// New returns a client for a Prometheus server such as http://localhost:9090.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: baseURL, hc: hc}
}

// Name implements core.MetricsSource.
func (c *Client) Name() string { return "prometheus" }

// apiResponse is the common Prometheus API envelope.
type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

// ListMetrics returns metric names containing match (case-insensitive),
// sorted. An empty match returns every name.
func (c *Client) ListMetrics(ctx context.Context, match string) ([]string, error) {
	var names []string
	if err := c.get(ctx, "/api/v1/label/__name__/values", nil, &names); err != nil {
		return nil, err
	}
	match = strings.ToLower(match)
	out := names[:0]
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), match) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DefaultStep returns the step used when Query is called with step <= 0:
// the window divided into 240 samples, rounded to whole seconds and at
// least 15s.
func DefaultStep(w core.Window) time.Duration {
	step := w.End.Sub(w.Start) / targetPoints
	step = step.Round(time.Second)
	if step < minStep {
		step = minStep
	}
	return step
}

// Query runs a range query for a selector. Only plain series selectors are
// accepted (PLAN.md §3); anything else is rejected before contacting
// Prometheus. At most MaxSeries series are returned; NaN and infinite
// samples are dropped.
func (c *Client) Query(ctx context.Context, selector string, w core.Window, step time.Duration) ([]core.Series, error) {
	sel, err := fixture.ParseSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	if !w.End.After(w.Start) {
		return nil, fmt.Errorf("prometheus: empty window %s..%s", w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339))
	}
	if step <= 0 {
		step = DefaultStep(w)
	}
	q := url.Values{
		"query": {sel.String()},
		"start": {formatTime(w.Start)},
		"end":   {formatTime(w.End)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	}
	if err := c.get(ctx, "/api/v1/query_range", q, &data); err != nil {
		return nil, err
	}
	if data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus: unexpected result type %q", data.ResultType)
	}
	res := data.Result
	if len(res) > MaxSeries {
		slog.Warn("prometheus: series capped", "selector", selector, "returned", len(res), "cap", MaxSeries)
		res = res[:MaxSeries]
	}
	out := make([]core.Series, 0, len(res))
	for _, r := range res {
		s := core.Series{Name: r.Metric["__name__"], Labels: make(map[string]string, len(r.Metric))}
		for k, v := range r.Metric {
			if k != "__name__" {
				s.Labels[k] = v
			}
		}
		if s.Name == "" {
			s.Name = sel.Name
		}
		for _, v := range r.Values {
			p, ok := parseSample(v)
			if ok {
				s.Points = append(s.Points, p)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// parseSample decodes a [unix_seconds, "value"] pair, rejecting NaN and
// infinite values.
func parseSample(v [2]any) (core.Point, bool) {
	ts, ok := v[0].(float64)
	if !ok {
		return core.Point{}, false
	}
	str, ok := v[1].(string)
	if !ok {
		return core.Point{}, false
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return core.Point{}, false
	}
	sec, frac := math.Modf(ts)
	return core.Point{T: time.Unix(int64(sec), int64(math.Round(frac*1e3))*int64(time.Millisecond)).UTC(), V: f}, true
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1e3, 'f', -1, 64)
}

// get performs a GET against path and decodes the envelope's data into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := strings.TrimRight(c.baseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("prometheus: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("prometheus: %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("prometheus: read %s: %w", path, err)
	}
	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("prometheus: %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 200))
		}
		return fmt.Errorf("prometheus: decode %s: %w", path, err)
	}
	if env.Status != "success" {
		msg := env.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if env.ErrorType != "" {
			msg = env.ErrorType + ": " + msg
		}
		return fmt.Errorf("prometheus: %s: %s", path, msg)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("prometheus: decode %s data: %w", path, err)
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
