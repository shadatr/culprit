package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(url, key string) *openAI {
	c := NewOpenAI(url, key, nil).(*openAI)
	c.backoff = func(int) time.Duration { return time.Millisecond }
	return c
}

func TestOpenAIRequestMapping(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("bad body: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"get_metric","arguments":"{\"selector\":\"up\"}"}},
			{"id":"c2","type":"function","function":{"name":"get_topology","arguments":""}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	}))
	defer srv.Close()

	temp := 0.1
	c := newTestClient(srv.URL+"/v1/", "sk-test")
	resp, err := c.Chat(context.Background(), Request{
		Model: "m",
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "list_metrics", Arguments: json.RawMessage(`{"match": "http"}`)}}},
			{Role: "tool", ToolCallID: "a", Name: "list_metrics", Content: "ok"},
		},
		Tools:       []ToolDef{{Name: "submit_report", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:  "submit_report",
		Temperature: &temp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" {
		t.Errorf("auth = %q", auth)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q", path)
	}
	msgs := got["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if asst["content"] != nil {
		t.Errorf("assistant content = %v, want null", asst["content"])
	}
	fn := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"match":"http"}` {
		t.Errorf("arguments = %#v, want JSON string", fn["arguments"])
	}
	tool := msgs[2].(map[string]any)
	if tool["tool_call_id"] != "a" || tool["role"] != "tool" {
		t.Errorf("tool message = %v", tool)
	}
	tools := got["tools"].([]any)[0].(map[string]any)
	if tools["type"] != "function" || tools["function"].(map[string]any)["name"] != "submit_report" {
		t.Errorf("tools = %v", tools)
	}
	tc := got["tool_choice"].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "submit_report" {
		t.Errorf("tool_choice = %v", tc)
	}

	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d", len(resp.Message.ToolCalls))
	}
	if string(resp.Message.ToolCalls[0].Arguments) != `{"selector":"up"}` {
		t.Errorf("args0 = %s", resp.Message.ToolCalls[0].Arguments)
	}
	if string(resp.Message.ToolCalls[1].Arguments) != `{}` {
		t.Errorf("args1 = %s", resp.Message.ToolCalls[1].Arguments)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 || resp.FinishReason != "tool_calls" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestOpenAIToolChoice(t *testing.T) {
	tests := []struct {
		choice string
		tools  bool
		want   string // JSON of tool_choice, "" when absent
	}{
		{"", true, ""},
		{"auto", true, `"auto"`},
		{"required", true, `"required"`},
		{"none", true, `"none"`},
		{"get_metric", true, `{"function":{"name":"get_metric"},"type":"function"}`},
		{"auto", false, ""},
	}
	for _, tt := range tests {
		req := Request{Model: "m", ToolChoice: tt.choice}
		if tt.tools {
			req.Tools = []ToolDef{{Name: "get_metric"}}
		}
		b, _ := json.Marshal(toWire(req))
		var m map[string]json.RawMessage
		json.Unmarshal(b, &m)
		got := string(m["tool_choice"])
		if got != tt.want {
			t.Errorf("choice %q tools %v: got %s want %s", tt.choice, tt.tools, got, tt.want)
		}
	}
}

func TestOpenAINoKeyNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("unexpected auth %q", h)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	resp, err := newTestClient(srv.URL, "").Chat(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" || resp.Message.Role != "assistant" {
		t.Errorf("msg = %+v", resp.Message)
	}
}

func TestOpenAIRetries(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		wantErr  bool
		wantHits int32
	}{
		{"429 then ok", []int{429, 200}, false, 2},
		{"503 twice then ok", []int{503, 502, 200}, false, 3},
		{"always 500", []int{500, 500, 500, 500}, true, 3},
		{"400 not retried", []int{400, 200}, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				st := tt.statuses[n-1]
				if st != 200 {
					w.WriteHeader(st)
					io.WriteString(w, `{"error":{"message":"`+strings.Repeat("x", 2000)+`"}}`)
					return
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			defer srv.Close()
			_, err := newTestClient(srv.URL, "k").Chat(context.Background(), Request{Model: "m"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "status") || len(err.Error()) > 900 {
					t.Errorf("error not informative or not truncated: %d chars", len(err.Error()))
				}
			}
			if hits.Load() != tt.wantHits {
				t.Errorf("hits = %d, want %d", hits.Load(), tt.wantHits)
			}
		})
	}
}

func TestOpenAIContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "")
	c.backoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Chat(ctx, Request{Model: "m"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("did not honour ctx")
	}
}

func TestOpenAIBadResponses(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"no choices", `{"choices":[]}`, "no choices"},
		{"bad json", `not json`, "decode response"},
		{"api error", `{"error":{"message":"model not found"}}`, "model not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			_, err := newTestClient(srv.URL, "").Chat(context.Background(), Request{Model: "m"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeArguments(t *testing.T) {
	tests := []struct{ in, want string }{
		{`"{\"a\":1}"`, `{"a":1}`},
		{`""`, `{}`},
		{`null`, `{}`},
		{`{"a":1}`, `{"a":1}`},
		{`"not json"`, `"not json"`},
	}
	for _, tt := range tests {
		if got := string(decodeArguments(json.RawMessage(tt.in))); got != tt.want {
			t.Errorf("decode(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestEncodeArgumentsRoundTrip(t *testing.T) {
	for _, in := range []string{`{"a": [1, 2]}`, ``, `"raw text"`} {
		enc := encodeArguments(json.RawMessage(in))
		var s string
		if err := json.Unmarshal(enc, &s); err != nil {
			t.Fatalf("encode(%q) not a JSON string: %s", in, enc)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("2"); d != 2*time.Second {
		t.Errorf("got %v", d)
	}
	if d := parseRetryAfter("9999"); d != 30*time.Second {
		t.Errorf("cap: got %v", d)
	}
	if d := parseRetryAfter("junk"); d != 0 {
		t.Errorf("junk: got %v", d)
	}
}
