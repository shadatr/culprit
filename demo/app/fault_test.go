package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewFaults(t *testing.T) {
	tests := []struct {
		role, env string
		wantErr   bool
		active    []string
	}{
		{"catalog", "", false, nil},
		{"catalog", "n_plus_one", false, []string{"n_plus_one"}},
		{"catalog", " pool_leak , n_plus_one", false, []string{"n_plus_one", "pool_leak"}},
		{"catalog", "bad_config", true, nil},
		{"frontend", "bad_config", false, []string{"bad_config"}},
		{"cart", "redis_down", true, nil},
	}
	for _, tt := range tests {
		f, err := NewFaults(tt.role, tt.env)
		if (err != nil) != tt.wantErr {
			t.Fatalf("NewFaults(%q, %q) err = %v", tt.role, tt.env, err)
		}
		if err != nil {
			continue
		}
		if got := strings.Join(f.Active(), ","); got != strings.Join(tt.active, ",") {
			t.Errorf("NewFaults(%q, %q) active = %q", tt.role, tt.env, got)
		}
	}
}

func TestAdminHandler(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name, token, remote, header, body string
		want                              int
		active                            bool
	}{
		{"disabled without token", "", "127.0.0.1:1", "x", `{"fault":"n_plus_one","enabled":true}`, 404, false},
		{"remote peer refused", "s3", "8.8.8.8:1", "s3", `{"fault":"n_plus_one","enabled":true}`, 403, false},
		{"wrong token", "s3", "127.0.0.1:1", "nope", `{"fault":"n_plus_one","enabled":true}`, 401, false},
		{"unknown fault", "s3", "127.0.0.1:1", "s3", `{"fault":"bad_config","enabled":true}`, 400, false},
		{"bad json", "s3", "127.0.0.1:1", "s3", `{`, 400, false},
		{"enable from loopback", "s3", "127.0.0.1:1", "s3", `{"fault":"n_plus_one","enabled":true}`, 200, true},
		{"enable from compose network", "s3", "172.18.0.5:4000", "s3", `{"fault":"n_plus_one","enabled":true}`, 200, true},
		{"enable from ipv6 loopback", "s3", "[::1]:1", "s3", `{"fault":"n_plus_one","enabled":true}`, 200, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := NewFaults("catalog", "")
			req := httptest.NewRequest(http.MethodPost, "/admin/fault", strings.NewReader(tt.body))
			req.RemoteAddr = tt.remote
			req.Header.Set("X-Admin-Token", tt.header)
			rec := httptest.NewRecorder()
			AdminHandler(f, tt.token, quiet)(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
			if f.On("n_plus_one") != tt.active {
				t.Fatalf("n_plus_one = %v, want %v", f.On("n_plus_one"), tt.active)
			}
		})
	}
}

func TestFaultHook(t *testing.T) {
	f, _ := NewFaults("catalog", "pool_leak")
	var got []string
	f.onSet = func(name string, on bool) {
		if !on {
			got = append(got, name)
		}
	}
	if err := f.Set("pool_leak", false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "pool_leak" {
		t.Fatalf("hook calls = %v", got)
	}
}

func TestRouteOf(t *testing.T) {
	tests := map[string]string{
		"":                   "unmatched",
		"GET /products/{id}": "/products/{id}",
		"/healthz":           "/healthz",
		"POST /cart":         "/cart",
	}
	for in, want := range tests {
		if got := routeOf(in); got != want {
			t.Errorf("routeOf(%q) = %q, want %q", in, got, want)
		}
	}
}
