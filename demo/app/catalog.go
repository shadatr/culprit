package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	productCount   = 100
	warehouseCount = 3000
	acquireTimeout = time.Second

	// statusClientClosed is nginx's 499: the caller cancelled the request.
	statusClientClosed = 499
)

// Product is what catalog serves.
type Product struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	PriceCents int    `json:"price_cents"`
	Stock      int    `json:"stock"`
}

type catalog struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	faults  *Faults
	queries *prometheus.HistogramVec

	mu     sync.Mutex
	leaked []*pgxpool.Conn
}

func setupCatalog(ctx context.Context, mux *http.ServeMux, log *slog.Logger, faults *Faults, m *Metrics) (func(), error) {
	cfg, err := pgxpool.ParseConfig(envOr("DATABASE_URL", "postgres://shop:shop@postgres:5432/shop?sslmode=disable"))
	if err != nil {
		return nil, fmt.Errorf("catalog: parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = int32(envFloat("DB_MAX_CONNS", 10))
	cfg.MinConns = 2
	pool, err := connectWithRetry(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	c := &catalog{
		pool:   pool,
		log:    log,
		faults: faults,
		queries: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "db_query_duration_seconds",
			Help:    "Database query latency, including waiting for a pool connection.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"query"}),
	}
	m.Register(c.queries,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "db_pool_connections_in_use", Help: "Pool connections currently acquired.",
		}, func() float64 { return float64(pool.Stat().AcquiredConns()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "db_pool_connections_max", Help: "Pool size limit.",
		}, func() float64 { return float64(pool.Stat().MaxConns()) }),
	)
	faults.onSet = func(name string, on bool) {
		if name == "pool_leak" && !on {
			c.releaseLeaked()
		}
	}
	if err := c.seed(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	mux.HandleFunc("GET /products", c.list)
	mux.HandleFunc("GET /products/{id}", c.get)
	return func() { c.releaseLeaked(); pool.Close() }, nil
}

func connectWithRetry(ctx context.Context, cfg *pgxpool.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for range 30 {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = pool.Ping(pctx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		log.Warn("database not ready, retrying", "err", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("catalog: connect: %w", lastErr)
}

// seed creates and fills the schema once. inventory has no index on
// product_id on purpose: one aggregate per product is a sequential scan, which
// is what makes the N+1 code path expensive.
func (c *catalog) seed(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS products (
  id          int PRIMARY KEY,
  name        text NOT NULL,
  price_cents int  NOT NULL,
  stock       int  NOT NULL
);
CREATE TABLE IF NOT EXISTS inventory (
  warehouse_id int NOT NULL,
  product_id   int NOT NULL,
  qty          int NOT NULL
);`
	if _, err := c.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("catalog: schema: %w", err)
	}
	var n int
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM products`).Scan(&n); err != nil {
		return fmt.Errorf("catalog: count: %w", err)
	}
	if n > 0 {
		return nil
	}
	c.log.Info("seeding catalog database", "products", productCount, "warehouses", warehouseCount)
	const fill = `
INSERT INTO inventory (warehouse_id, product_id, qty)
  SELECT w, p, (w * 7 + p * 13) % 20 FROM generate_series(1, $1::int) w, generate_series(1, $2::int) p;
INSERT INTO products (id, name, price_cents, stock)
  SELECT p, 'Product ' || p, 499 + (p * 137) % 9500,
         (SELECT sum(qty) FROM inventory i WHERE i.product_id = p)
  FROM generate_series(1, $2::int) p;
ANALYZE;`
	// Multiple statements with parameters need the simple protocol.
	_, err := c.pool.Exec(ctx, fill, pgx.QueryExecModeSimpleProtocol, warehouseCount, productCount)
	if err != nil {
		return fmt.Errorf("catalog: seed: %w", err)
	}
	return nil
}

// acquire gets a pool connection, turning an acquire timeout into the error
// operators would grep for.
func (c *catalog) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()
	conn, err := c.pool.Acquire(actx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			s := c.pool.Stat()
			return nil, fmt.Errorf("timeout acquiring connection from pool (in use %d/%d)", s.AcquiredConns(), s.MaxConns())
		}
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	return conn, nil
}

func (c *catalog) observe(query string, start time.Time) {
	c.queries.WithLabelValues(query).Observe(time.Since(start).Seconds())
}

func (c *catalog) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > productCount {
		limit = 24
	}
	var (
		out []Product
		err error
	)
	if c.faults.On("n_plus_one") {
		out, err = c.listWithLiveStock(ctx, limit)
	} else {
		out, err = c.listProducts(ctx, limit)
	}
	if err != nil {
		c.fail(w, r, "list products", err)
		return
	}
	// Pricing rules and recommendations: modelled as CPU-free latency.
	if err := sleepCtx(ctx, jitter(40*time.Millisecond, 70*time.Millisecond)); err != nil {
		w.WriteHeader(statusClientClosed)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *catalog) listProducts(ctx context.Context, limit int) ([]Product, error) {
	start := time.Now()
	defer c.observe("list_products", start)
	conn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, `SELECT id, name, price_cents, stock FROM products ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query list_products: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Product, error) {
		var p Product
		err := row.Scan(&p.ID, &p.Name, &p.PriceCents, &p.Stock)
		return p, err
	})
}

// listWithLiveStock is the 1.8.2 "live stock" feature: it reads products and
// then aggregates inventory once per product (N+1).
func (c *catalog) listWithLiveStock(ctx context.Context, limit int) ([]Product, error) {
	start := time.Now()
	conn, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, `SELECT id, name, price_cents FROM products ORDER BY id LIMIT $1`, limit)
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("query list_products: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Product, error) {
		var p Product
		err := row.Scan(&p.ID, &p.Name, &p.PriceCents)
		return p, err
	})
	conn.Release()
	c.observe("list_products", start)
	if err != nil {
		return nil, fmt.Errorf("query list_products: %w", err)
	}
	for i := range out {
		qs := time.Now()
		conn, err := c.acquire(ctx)
		if err != nil {
			return nil, err
		}
		err = conn.QueryRow(ctx, `SELECT coalesce(sum(qty), 0) FROM inventory WHERE product_id = $1`, out[i].ID).Scan(&out[i].Stock)
		conn.Release()
		c.observe("stock_by_product", qs)
		if err != nil {
			return nil, fmt.Errorf("query stock_by_product: %w", err)
		}
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		c.log.Warn("slow database work for request", "route", "/products", "queries", len(out)+1, "duration_ms", el.Milliseconds())
	}
	return out, nil
}

func (c *catalog) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	start := time.Now()
	conn, err := c.acquire(ctx)
	if err != nil {
		c.observe("get_product", start)
		c.fail(w, r, "get product", err)
		return
	}
	var p Product
	err = conn.QueryRow(ctx, `SELECT id, name, price_cents, stock FROM products WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &p.PriceCents, &p.Stock)
	c.observe("get_product", start)
	// pool_leak: the "featured product" early-return path forgets Release.
	if c.faults.On("pool_leak") && err == nil && rand.Float64() < 0.06 {
		c.mu.Lock()
		c.leaked = append(c.leaked, conn)
		c.mu.Unlock()
	} else {
		conn.Release()
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.log.Info("product not found", "product_id", id)
		http.Error(w, "not found", http.StatusNotFound)
		return
	case err != nil:
		c.fail(w, r, "get product", err)
		return
	}
	if err := sleepCtx(ctx, jitter(20*time.Millisecond, 50*time.Millisecond)); err != nil {
		w.WriteHeader(statusClientClosed)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (c *catalog) releaseLeaked() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.leaked {
		conn.Release()
	}
	c.leaked = nil
}

func (c *catalog) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		// Caller gave up (e.g. frontend timeout); nothing useful to send.
		c.log.Info("client went away", "route", routeOf(r.Pattern), "op", what)
		w.WriteHeader(statusClientClosed)
		return
	}
	c.log.Error(what+" failed", "route", routeOf(r.Pattern), "err", err.Error())
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// jitter returns base plus a uniformly random extra in [0, spread).
func jitter(base, spread time.Duration) time.Duration {
	return base + time.Duration(rand.Int64N(int64(spread)))
}
