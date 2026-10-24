package loki

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

var _ core.LogsSource = (*Client)(nil)

func newServer(t *testing.T, label string, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, label, srv.Client())
}

var (
	t0 = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	w  = core.Window{Start: t0, End: t0.Add(time.Hour)}
)

func TestServices(t *testing.T) {
	c := newServer(t, "app", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/label/app/values" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got, want := r.URL.Query().Get("start"), fmt.Sprint(t0.UnixNano()); got != want {
			t.Errorf("start = %s, want %s", got, want)
		}
		if got, want := r.URL.Query().Get("end"), fmt.Sprint(t0.Add(time.Hour).UnixNano()); got != want {
			t.Errorf("end = %s, want %s", got, want)
		}
		fmt.Fprint(rw, `{"status":"success","data":["frontend","cart","catalog"]}`)
	})
	got, err := c.Services(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cart", "catalog", "frontend"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestServicesEmpty(t *testing.T) {
	c := newServer(t, "", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/label/service/values" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(rw, `{"status":"success"}`)
	})
	got, err := c.Services(context.Background(), w)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestBuildQuery(t *testing.T) {
	c := New("http://x", "", nil)
	tests := []struct {
		q    core.LogQuery
		want string
	}{
		{core.LogQuery{}, `{service=~".+"}`},
		{core.LogQuery{Service: "cart"}, `{service="cart"}`},
		{core.LogQuery{Service: `we"ird`}, `{service="we\"ird"}`},
		{core.LogQuery{Service: "cart", Contains: `say "hi".`}, `{service="cart"} |~ "(?i)say \"hi\"\\."`},
		{core.LogQuery{Level: "error"}, `{service=~".+"} | json | level=~"(?i)error"`},
		{core.LogQuery{Service: "a", Contains: "timeout", Level: "warn"}, `{service="a"} |~ "(?i)timeout" | json | level=~"(?i)warn"`},
	}
	for _, tt := range tests {
		if got := c.BuildQuery(tt.q); got != tt.want {
			t.Errorf("BuildQuery(%+v) = %s, want %s", tt.q, got, tt.want)
		}
	}
}

func TestQuery(t *testing.T) {
	ns := func(d time.Duration) string { return fmt.Sprint(t0.Add(d).UnixNano()) }
	var params map[string]string
	c := newServer(t, "", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		params = map[string]string{}
		for k := range r.URL.Query() {
			params[k] = r.URL.Query().Get(k)
		}
		fmt.Fprintf(rw, `{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"service":"cart","level":"error"},"values":[["%s","plain failure"],["%s","{\"level\":\"info\",\"msg\":\"json with stream level\"}"]]},
			{"stream":{"service":"cart","__error__":"JSONParserErr"},"values":[["%s","{\"level\":\"warn\",\"message\":\"from json\"}"],["%s","not json"],["%s","{\"msg\":42}"]]}
		]}}`, ns(3*time.Minute), ns(5*time.Minute), ns(time.Minute), ns(4*time.Minute), ns(6*time.Minute))
	})
	got, err := c.Query(context.Background(), core.LogQuery{Service: "cart", Window: w})
	if err != nil {
		t.Fatal(err)
	}
	wantParams := map[string]string{
		"query":     `{service="cart"}`,
		"direction": "forward",
		"limit":     "1000",
		"start":     fmt.Sprint(t0.UnixNano()),
		"end":       fmt.Sprint(t0.Add(time.Hour).UnixNano()),
	}
	if !reflect.DeepEqual(params, wantParams) {
		t.Fatalf("params = %v\nwant %v", params, wantParams)
	}
	type row struct {
		T     time.Time
		Level string
		Msg   string
	}
	var rows []row
	for _, l := range got {
		if l.Service != "cart" {
			t.Errorf("service = %q", l.Service)
		}
		if _, ok := l.Labels["__error__"]; ok {
			t.Errorf("internal label leaked: %v", l.Labels)
		}
		rows = append(rows, row{l.T, l.Level, l.Message})
	}
	want := []row{
		{t0.Add(time.Minute), "warn", "from json"},
		{t0.Add(3 * time.Minute), "error", "plain failure"},
		{t0.Add(4 * time.Minute), "", "not json"},
		{t0.Add(5 * time.Minute), "error", "json with stream level"},
		{t0.Add(6 * time.Minute), "", `{"msg":42}`},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v\nwant %+v", rows, want)
	}
}

func TestQueryLimit(t *testing.T) {
	c := newServer(t, "", func(rw http.ResponseWriter, r *http.Request) {
		if l := r.URL.Query().Get("limit"); l != "2" {
			t.Errorf("limit = %s", l)
		}
		// Two streams, each within the limit, but more than the limit merged.
		fmt.Fprintf(rw, `{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"service":"a"},"values":[["%d","a1"],["%d","a2"]]},
			{"stream":{"service":"b"},"values":[["%d","b1"],["%d","b2"]]}]}}`,
			t0.UnixNano(), t0.Add(2*time.Second).UnixNano(), t0.Add(time.Second).UnixNano(), t0.Add(3*time.Second).UnixNano())
	})
	got, err := c.Query(context.Background(), core.LogQuery{Window: w, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Message != "a1" || got[1].Message != "b1" {
		t.Fatalf("got %+v", got)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"bad query", http.StatusBadRequest, "parse error at line 1, col 2: syntax error", "HTTP 400: parse error"},
		{"bad json", http.StatusOK, "nope", "decode"},
		{"failure status", http.StatusOK, `{"status":"error"}`, `status "error"`},
		{"wrong type", http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, "unexpected result type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, "", func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(tt.status)
				fmt.Fprint(rw, tt.body)
			})
			_, err := c.Query(context.Background(), core.LogQuery{Window: w})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
