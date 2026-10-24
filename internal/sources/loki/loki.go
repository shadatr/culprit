// Package loki reads logs from the Loki HTTP API.
//
// Level filtering: a LogQuery.Level is applied as
//
//	| json | level=~"(?i)<level>"
//
// This one pipeline covers both common layouts. When the stream carries a
// "level" label, Loki keeps it and renames any JSON "level" key to
// "level_extracted", so the filter tests the stream label. When it does not,
// the json stage extracts "level" from JSON lines. Lines that are not JSON
// keep only their stream labels (with an __error__ label, which we ignore),
// so they still match through a stream label but are otherwise dropped.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

const (
	defaultLimit = 1000
	maxBodyBytes = 64 << 20
)

// Client implements core.LogsSource.
type Client struct {
	baseURL      string
	serviceLabel string
	hc           *http.Client
}

// New returns a client for a Loki server such as http://localhost:3100.
// serviceLabel is the stream label that names the service (default "service").
func New(baseURL, serviceLabel string, hc *http.Client) *Client {
	if serviceLabel == "" {
		serviceLabel = "service"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{baseURL: baseURL, serviceLabel: serviceLabel, hc: hc}
}

// Name implements core.LogsSource.
func (c *Client) Name() string { return "loki" }

// Services lists values of the service label in w, sorted.
func (c *Client) Services(ctx context.Context, w core.Window) ([]string, error) {
	q := url.Values{}
	setWindow(q, w)
	var out []string
	if err := c.get(ctx, "/loki/api/v1/label/"+url.PathEscape(c.serviceLabel)+"/values", q, &out); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// BuildQuery returns the LogQL expression used for q.
// Contains is matched case-insensitively, consistent with the fixture source.
func (c *Client) BuildQuery(q core.LogQuery) string {
	var b strings.Builder
	if q.Service != "" {
		fmt.Fprintf(&b, "{%s=%s}", c.serviceLabel, strconv.Quote(q.Service))
	} else {
		fmt.Fprintf(&b, "{%s=~\".+\"}", c.serviceLabel)
	}
	if q.Contains != "" {
		fmt.Fprintf(&b, " |~ %s", strconv.Quote("(?i)"+regexp.QuoteMeta(q.Contains)))
	}
	if q.Level != "" {
		fmt.Fprintf(&b, " | json | level=~%s", strconv.Quote("(?i)"+regexp.QuoteMeta(q.Level)))
	}
	return b.String()
}

// Query returns matching lines, oldest first.
func (c *Client) Query(ctx context.Context, q core.LogQuery) ([]core.LogLine, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	v := url.Values{
		"query":     {c.BuildQuery(q)},
		"direction": {"forward"},
		"limit":     {strconv.Itoa(limit)},
	}
	setWindow(v, q.Window)
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"` // [unix_ns, line]
		} `json:"result"`
	}
	if err := c.get(ctx, "/loki/api/v1/query_range", v, &data); err != nil {
		return nil, err
	}
	if data.ResultType != "streams" {
		return nil, fmt.Errorf("loki: unexpected result type %q", data.ResultType)
	}
	var out []core.LogLine
	for _, r := range data.Result {
		labels := make(map[string]string, len(r.Stream))
		for k, val := range r.Stream {
			if !strings.HasPrefix(k, "__") {
				labels[k] = val
			}
		}
		for _, e := range r.Values {
			ns, err := strconv.ParseInt(e[0], 10, 64)
			if err != nil {
				continue
			}
			msg, lvl := parseLine(e[1])
			if l := labels["level"]; l != "" {
				lvl = l
			}
			out = append(out, core.LogLine{
				T:       time.Unix(0, ns).UTC(),
				Service: labels[c.serviceLabel],
				Level:   lvl,
				Message: msg,
				Labels:  labels,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// parseLine extracts the message and level from a JSON log line. Non-JSON
// lines are returned as the message with no level.
func parseLine(line string) (msg, level string) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "{") {
		return line, ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return line, ""
	}
	msg = line
	for _, k := range []string{"msg", "message"} {
		if v, ok := obj[k].(string); ok {
			msg = v
			break
		}
	}
	for _, k := range []string{"level", "lvl", "severity"} {
		if v, ok := obj[k].(string); ok {
			level = v
			break
		}
	}
	return msg, level
}

func setWindow(v url.Values, w core.Window) {
	if !w.Start.IsZero() {
		v.Set("start", strconv.FormatInt(w.Start.UnixNano(), 10))
	}
	if !w.End.IsZero() {
		v.Set("end", strconv.FormatInt(w.End.UnixNano(), 10))
	}
}

// get performs a GET against path and decodes the envelope's data into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := strings.TrimRight(c.baseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("loki: build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("loki: %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("loki: read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Loki returns plain-text errors for bad queries.
		return fmt.Errorf("loki: %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 300))
	}
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("loki: decode %s: %w", path, err)
	}
	if env.Status != "success" {
		return fmt.Errorf("loki: %s: status %q", path, env.Status)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("loki: decode %s data: %w", path, err)
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
