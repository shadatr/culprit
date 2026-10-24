// Package loki reads logs from the Loki HTTP API.
package loki

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/shadatr/culprit/internal/core"
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

// Services lists values of the service label in w.
// TODO(WS-B): implement via /loki/api/v1/label/<label>/values.
func (c *Client) Services(ctx context.Context, w core.Window) ([]string, error) {
	return nil, errors.New("loki: not implemented")
}

// Query returns matching lines, oldest first.
// TODO(WS-B): implement via /loki/api/v1/query_range.
func (c *Client) Query(ctx context.Context, q core.LogQuery) ([]core.LogLine, error) {
	return nil, errors.New("loki: not implemented")
}
