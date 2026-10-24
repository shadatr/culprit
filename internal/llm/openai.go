package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxAttempts is how many times a retryable request is tried in total.
const maxAttempts = 3

// errBodyLimit caps how much of an error response body ends up in an error.
const errBodyLimit = 512

// NewOpenAI returns a client for an OpenAI-compatible endpoint, e.g.
// https://api.openai.com/v1 or http://localhost:8080/v1 (Switchyard).
// apiKey is sent as a Bearer token when non-empty. A nil hc uses a client
// with a 120s timeout.
func NewOpenAI(baseURL, apiKey string, hc *http.Client) Client {
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	return &openAI{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      hc,
		backoff: defaultBackoff,
	}
}

type openAI struct {
	baseURL, apiKey string
	hc              *http.Client
	backoff         func(attempt int) time.Duration
}

func defaultBackoff(attempt int) time.Duration {
	return time.Duration(500*(1<<attempt)) * time.Millisecond // 0.5s, 1s, 2s
}

// Wire types for the Chat Completions API.

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	ToolChoice  any           `json:"tool_choice,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireCallFunction `json:"function"`
}

type wireCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []wireToolCall  `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat implements Client. It retries 429 and 5xx responses and transport
// errors up to three attempts in total, honouring Retry-After and ctx.
func (c *openAI) Chat(ctx context.Context, req Request) (*Response, error) {
	body, err := json.Marshal(toWire(req))
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}
	url := c.baseURL + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			wait := c.backoff(attempt - 1)
			var re *retryableError
			if errors.As(lastErr, &re) && re.retryAfter > 0 {
				wait = re.retryAfter
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, fmt.Errorf("llm: %w (last error: %v)", ctx.Err(), lastErr)
			case <-t.C:
			}
		}
		resp, err := c.do(ctx, url, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var re *retryableError
		if !errors.As(err, &re) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("llm: giving up after %d attempts: %w", maxAttempts, lastErr)
}

// retryableError marks failures worth another attempt.
type retryableError struct {
	err        error
	retryAfter time.Duration
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *openAI) do(ctx context.Context, url string, body []byte) (*Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	hresp, err := c.hc.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("llm: POST %s: %w", url, ctx.Err())
		}
		return nil, &retryableError{err: fmt.Errorf("llm: POST %s: %w", url, err)}
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 32<<20))
	if err != nil {
		return nil, &retryableError{err: fmt.Errorf("llm: read response: %w", err)}
	}
	if hresp.StatusCode/100 != 2 {
		e := fmt.Errorf("llm: POST %s: status %d: %s", url, hresp.StatusCode, truncate(string(raw), errBodyLimit))
		if hresp.StatusCode == http.StatusTooManyRequests || hresp.StatusCode >= 500 {
			return nil, &retryableError{err: e, retryAfter: parseRetryAfter(hresp.Header.Get("Retry-After"))}
		}
		return nil, e
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w (body: %s)", err, truncate(string(raw), errBodyLimit))
	}
	if wr.Error != nil && wr.Error.Message != "" {
		return nil, fmt.Errorf("llm: API error: %s", wr.Error.Message)
	}
	if len(wr.Choices) == 0 {
		return nil, fmt.Errorf("llm: response has no choices (body: %s)", truncate(string(raw), errBodyLimit))
	}
	ch := wr.Choices[0]
	msg := Message{Role: ch.Message.Role, Content: decodeContent(ch.Message.Content)}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	for i, tc := range ch.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = "call_" + strconv.Itoa(i)
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: decodeArguments(tc.Function.Arguments),
		})
	}
	return &Response{
		Message:      msg,
		FinishReason: ch.FinishReason,
		Usage: Usage{
			PromptTokens:     wr.Usage.PromptTokens,
			CompletionTokens: wr.Usage.CompletionTokens,
		},
	}, nil
}

func toWire(req Request) wireRequest {
	wr := wireRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID, Name: m.Name}
		content := m.Content
		// Assistant messages that only carry tool calls send null content,
		// which every compatible server accepts.
		if !(m.Role == "assistant" && content == "" && len(m.ToolCalls) > 0) {
			wm.Content = &content
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: wireCallFunction{
					Name:      tc.Name,
					Arguments: encodeArguments(tc.Arguments),
				},
			})
		}
		wr.Messages = append(wr.Messages, wm)
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		wr.Tools = append(wr.Tools, wireTool{
			Type:     "function",
			Function: wireFunction{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	if len(req.Tools) > 0 {
		switch req.ToolChoice {
		case "":
		case "auto", "required", "none":
			wr.ToolChoice = req.ToolChoice
		default:
			wr.ToolChoice = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": req.ToolChoice},
			}
		}
	}
	return wr
}

// encodeArguments turns our JSON arguments into the JSON string the API
// expects. A JSON string value (used for unparseable model output) is
// passed through unquoted.
func encodeArguments(a json.RawMessage) json.RawMessage {
	a = bytes.TrimSpace(a)
	if len(a) == 0 {
		a = json.RawMessage("{}")
	}
	var s string
	if a[0] == '"' && json.Unmarshal(a, &s) == nil {
		b, _ := json.Marshal(s)
		return b
	}
	var buf bytes.Buffer
	if json.Compact(&buf, a) == nil {
		a = buf.Bytes()
	}
	b, _ := json.Marshal(string(a))
	return b
}

// decodeArguments converts the API's argument string (or, for some servers,
// a raw JSON object) to a RawMessage. Invalid JSON is kept as a JSON string
// so the caller can report it back to the model.
func decodeArguments(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	if raw[0] != '"' {
		if json.Valid(raw) {
			return append(json.RawMessage(nil), raw...)
		}
		b, _ := json.Marshal(string(raw))
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return json.RawMessage("{}")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// decodeContent accepts a string, null, or an array of text parts.
func decodeContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		d := time.Duration(n) * time.Second
		return min(d, 30*time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return min(d, 30*time.Second)
	}
	return 0
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
