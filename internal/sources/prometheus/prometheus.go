// Package prometheus reads metrics from the Prometheus HTTP API.
package prometheus

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/shadatr/culprit/internal/core"
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

// ListMetrics returns metric names containing match.
// TODO(WS-B): implement via /api/v1/label/__name__/values.
func (c *Client) ListMetrics(ctx context.Context, match string) ([]string, error) {
	return nil, errors.New("prometheus: not implemented")
}

// Query runs a range query for a selector.
// TODO(WS-B): implement via /api/v1/query_range; reject non-selector input.
func (c *Client) Query(ctx context.Context, selector string, w core.Window, step time.Duration) ([]core.Series, error) {
	return nil, errors.New("prometheus: not implemented")
}
