package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var _ core.MetricsSource = (*Client)(nil)

func newServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", srv.Client())
}

func TestListMetrics(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/label/__name__/values" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"status":"success","data":["up","http_requests:rate1m","HTTP_errors:ratio1m","process_cpu:ratio"]}`)
	})
	tests := []struct {
		match string
		want  []string
	}{
		{"", []string{"HTTP_errors:ratio1m", "http_requests:rate1m", "process_cpu:ratio", "up"}},
		{"http", []string{"HTTP_errors:ratio1m", "http_requests:rate1m"}},
		{"RATIO", []string{"HTTP_errors:ratio1m", "process_cpu:ratio"}},
		{"nothing", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.match, func(t *testing.T) {
			got, err := c.ListMetrics(context.Background(), tt.match)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultStep(t *testing.T) {
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		span time.Duration
		want time.Duration
	}{
		{time.Minute, 15 * time.Second},
		{time.Hour, 15 * time.Second},
		{2*time.Hour + 30*time.Minute, 38 * time.Second}, // 37.5s rounds up
		{24 * time.Hour, 6 * time.Minute},
	}
	for _, tt := range tests {
		if got := DefaultStep(core.Window{Start: base, End: base.Add(tt.span)}); got != tt.want {
			t.Errorf("DefaultStep(%v) = %v, want %v", tt.span, got, tt.want)
		}
	}
}

func TestQuery(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := core.Window{Start: start, End: start.Add(time.Hour)}
	var gotQuery map[string]string
	c := newServer(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotQuery = map[string]string{}
		for k := range r.URL.Query() {
			gotQuery[k] = r.URL.Query().Get(k)
		}
		fmt.Fprintf(rw, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"__name__":"http_requests:rate1m","service":"cart"},
			 "values":[[%d,"1.5"],[%d.5,"NaN"],[%d,"+Inf"],[%d,"-Inf"],[%d,"2"]]},
			{"metric":{"service":"catalog"},"values":[[%d,"3"]]}
		]}}`, start.Unix(), start.Unix()+15, start.Unix()+30, start.Unix()+45, start.Unix()+60, start.Unix())
	})
	got, err := c.Query(context.Background(), ` http_requests:rate1m{service=~"ca.*"} `, w, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantQ := map[string]string{
		"query": `http_requests:rate1m{service=~"ca.*"}`,
		"start": fmt.Sprint(start.Unix()),
		"end":   fmt.Sprint(start.Add(time.Hour).Unix()),
		"step":  "15",
	}
	if !reflect.DeepEqual(gotQuery, wantQ) {
		t.Fatalf("query params = %v, want %v", gotQuery, wantQ)
	}
	want := []core.Series{
		{Name: "http_requests:rate1m", Labels: map[string]string{"service": "cart"}, Points: []core.Point{
			{T: start, V: 1.5}, {T: start.Add(time.Minute), V: 2},
		}},
		{Name: "http_requests:rate1m", Labels: map[string]string{"service": "catalog"}, Points: []core.Point{{T: start, V: 3}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestQueryExplicitStep(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c := newServer(t, func(rw http.ResponseWriter, r *http.Request) {
		if s := r.URL.Query().Get("step"); s != "60" {
			t.Errorf("step = %q", s)
		}
		fmt.Fprint(rw, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	})
	got, err := c.Query(context.Background(), "up", core.Window{Start: start, End: start.Add(time.Hour)}, time.Minute)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestQueryCapsSeries(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c := newServer(t, func(rw http.ResponseWriter, r *http.Request) {
		var parts []string
		for i := range MaxSeries + 50 {
			parts = append(parts, fmt.Sprintf(`{"metric":{"__name__":"up","i":"%d"},"values":[[%d,"1"]]}`, i, start.Unix()))
		}
		fmt.Fprintf(rw, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(parts, ","))
	})
	got, err := c.Query(context.Background(), "up", core.Window{Start: start, End: start.Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxSeries {
		t.Fatalf("len = %d, want %d", len(got), MaxSeries)
	}
}

func TestQueryRejectsNonSelectors(t *testing.T) {
	called := false
	c := newServer(t, func(rw http.ResponseWriter, r *http.Request) { called = true })
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := core.Window{Start: start, End: start.Add(time.Hour)}
	for _, sel := range []string{"rate(up[5m])", "up[5m]", "up offset 1h", "sum by (job) (up)", "up > 1", ""} {
		if _, err := c.Query(context.Background(), sel, w, 0); err == nil {
			t.Errorf("Query(%q): want error", sel)
		}
	}
	if _, err := c.Query(context.Background(), "up", core.Window{Start: start, End: start}, 0); err == nil {
		t.Error("empty window: want error")
	}
	if called {
		t.Fatal("server must not be contacted for invalid input")
	}
}

func TestErrors(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := core.Window{Start: start, End: start.Add(time.Hour)}
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"prometheus error json", http.StatusBadRequest, `{"status":"error","errorType":"bad_data","error":"parse error"}`, "bad_data: parse error"},
		{"plain text 502", http.StatusBadGateway, "upstream down", "HTTP 502: upstream down"},
		{"bad json 200", http.StatusOK, "not json", "decode"},
		{"wrong result type", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`, "unexpected result type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(tt.status)
				fmt.Fprint(rw, tt.body)
			})
			_, err := c.Query(context.Background(), "up", w, 0)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
