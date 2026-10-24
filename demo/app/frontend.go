package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// badConfigTimeout is the catalog timeout the faulty config ships with.
const badConfigTimeout = 50 * time.Millisecond

// errUpstreamStatus is returned when an upstream answers with a 5xx.
var errUpstreamStatus = errors.New("upstream error status")

type upstream struct {
	name    string
	base    string
	timeout func() time.Duration
	hc      *http.Client
}

type frontend struct {
	log     *slog.Logger
	catalog *upstream
	cart    *upstream
}

func setupFrontend(_ context.Context, mux *http.ServeMux, log *slog.Logger, faults *Faults) error {
	catalogTimeout := envDuration("CATALOG_TIMEOUT", 2*time.Second)
	log.Info("catalog client configured", "timeout", catalogTimeout.String())
	hc := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second}}
	f := &frontend{
		log: log,
		catalog: &upstream{
			name: "catalog", base: envOr("CATALOG_URL", "http://catalog:8080"), hc: hc,
			timeout: func() time.Duration {
				if faults.On("bad_config") {
					return badConfigTimeout
				}
				return catalogTimeout
			},
		},
		cart: &upstream{
			name: "cart", base: envOr("CART_URL", "http://cart:8080"), hc: hc,
			timeout: func() time.Duration { return 2 * time.Second },
		},
	}
	mux.HandleFunc("GET /{$}", f.home)
	mux.HandleFunc("GET /product/{id}", f.product)
	mux.HandleFunc("POST /cart/add", f.addToCart)
	mux.HandleFunc("GET /cart", f.viewCart)
	return nil
}

// call performs one upstream request and returns status and body.
func (u *upstream) call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, u.timeout())
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.base+path, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, fmt.Errorf("context deadline exceeded calling %s", u.name)
		}
		return 0, nil, fmt.Errorf("calling %s: %w", u.name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, fmt.Errorf("context deadline exceeded calling %s", u.name)
		}
		return 0, nil, fmt.Errorf("reading %s response: %w", u.name, err)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, b, fmt.Errorf("%s returned %d: %w", u.name, resp.StatusCode, errUpstreamStatus)
	}
	return resp.StatusCode, b, nil
}

func (f *frontend) upstreamFail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		w.WriteHeader(statusClientClosed)
		return
	}
	f.log.Error(err.Error(), "route", routeOf(r.Pattern))
	code := http.StatusBadGateway
	if !errors.Is(err, errUpstreamStatus) && strings.Contains(err.Error(), "deadline exceeded") {
		code = http.StatusGatewayTimeout
	}
	http.Error(w, "something went wrong, please retry", code)
}

func (f *frontend) home(w http.ResponseWriter, r *http.Request) {
	_, body, err := f.catalog.call(r.Context(), http.MethodGet, "/products?limit=24", nil)
	if err != nil {
		f.upstreamFail(w, r, err)
		return
	}
	var ps []Product
	if err := json.Unmarshal(body, &ps); err != nil {
		f.upstreamFail(w, r, fmt.Errorf("decoding catalog response: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"page": "home", "products": len(ps)})
}

func (f *frontend) product(w http.ResponseWriter, r *http.Request) {
	code, body, err := f.catalog.call(r.Context(), http.MethodGet, "/products/"+url.PathEscape(r.PathValue("id")), nil)
	if err != nil {
		f.upstreamFail(w, r, err)
		return
	}
	if code == http.StatusNotFound {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}
	if code != http.StatusOK {
		http.Error(w, "bad request", code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (f *frontend) addToCart(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code, body, err := f.cart.call(r.Context(), http.MethodPost, "/cart", b)
	if err != nil {
		f.upstreamFail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (f *frontend) viewCart(w http.ResponseWriter, r *http.Request) {
	code, body, err := f.cart.call(r.Context(), http.MethodGet, "/cart?user="+url.QueryEscape(r.URL.Query().Get("user")), nil)
	if err != nil {
		f.upstreamFail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
