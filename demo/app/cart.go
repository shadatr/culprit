package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

const summaryTTL = 5 * time.Minute

// CartItem is one line in a cart.
type CartItem struct {
	ProductID int `json:"product_id"`
	Qty       int `json:"qty"`
}

// CartSummary is the cached view of a cart.
type CartSummary struct {
	User  string     `json:"user"`
	Items []CartItem `json:"items"`
	Count int        `json:"count"`
}

type cart struct {
	rdb    *redis.Client
	log    *slog.Logger
	lookup *prometheus.CounterVec
}

func setupCart(_ context.Context, mux *http.ServeMux, log *slog.Logger, m *Metrics) (func(), error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         envOr("REDIS_ADDR", "redis:6379"),
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  300 * time.Millisecond,
		WriteTimeout: 300 * time.Millisecond,
		MaxRetries:   1,
		PoolSize:     20,
	})
	c := &cart{
		rdb: rdb,
		log: log,
		lookup: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cache_requests_total",
			Help: "Cart summary cache lookups by result (hit, miss, error).",
		}, []string{"cache", "result"}),
	}
	for _, r := range []string{"hit", "miss", "error"} {
		c.lookup.WithLabelValues("cart_summary", r) // export zeros from the start
	}
	m.Register(c.lookup)
	mux.HandleFunc("POST /cart", c.add)
	mux.HandleFunc("GET /cart", c.get)
	return func() { _ = rdb.Close() }, nil
}

func (c *cart) add(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User      string `json:"user"`
		ProductID int    `json:"product_id"`
		Qty       int    `json:"qty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.User == "" || req.ProductID <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Qty <= 0 {
		req.Qty = 1
	}
	ctx := r.Context()
	key := "cart:" + req.User
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HIncrBy(ctx, key, strconv.Itoa(req.ProductID), int64(req.Qty))
		p.Expire(ctx, key, time.Hour)
		p.Del(ctx, "cartsummary:"+req.User)
		return nil
	})
	if err != nil {
		c.redisFail(w, r, "add to cart", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (c *cart) get(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	if user == "" {
		http.Error(w, "user required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	cached, err := c.rdb.Get(ctx, "cartsummary:"+user).Bytes()
	switch {
	case err == nil:
		c.lookup.WithLabelValues("cart_summary", "hit").Inc()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	case errors.Is(err, redis.Nil):
		c.lookup.WithLabelValues("cart_summary", "miss").Inc()
	default:
		c.lookup.WithLabelValues("cart_summary", "error").Inc()
		c.redisFail(w, r, "read cart summary", err)
		return
	}
	s, err := c.load(ctx, user)
	if err != nil {
		c.redisFail(w, r, "load cart", err)
		return
	}
	b, _ := json.Marshal(s)
	if err := c.rdb.Set(ctx, "cartsummary:"+user, b, summaryTTL).Err(); err != nil {
		c.log.Warn("could not cache cart summary", "err", err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

func (c *cart) load(ctx context.Context, user string) (CartSummary, error) {
	m, err := c.rdb.HGetAll(ctx, "cart:"+user).Result()
	if err != nil {
		return CartSummary{}, fmt.Errorf("hgetall: %w", err)
	}
	s := CartSummary{User: user, Items: []CartItem{}}
	for k, v := range m {
		id, _ := strconv.Atoi(k)
		q, _ := strconv.Atoi(v)
		s.Items = append(s.Items, CartItem{ProductID: id, Qty: q})
		s.Count += q
	}
	return s, nil
}

func (c *cart) redisFail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		w.WriteHeader(statusClientClosed)
		return
	}
	c.log.Error("redis command failed", "op", what, "route", routeOf(r.Pattern), "err", err.Error())
	http.Error(w, "cart storage unavailable", http.StatusServiceUnavailable)
}
