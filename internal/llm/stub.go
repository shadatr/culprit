package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// TODO(WS-A): replace this file with openai.go and scripted.go.

// NewOpenAI returns a client for an OpenAI-compatible endpoint, e.g.
// https://api.openai.com/v1 or http://localhost:8080/v1 (Switchyard).
func NewOpenAI(baseURL, apiKey string, hc *http.Client) Client {
	return &openAI{baseURL: baseURL, apiKey: apiKey, hc: hc}
}

type openAI struct {
	baseURL, apiKey string
	hc              *http.Client
}

func (c *openAI) Chat(ctx context.Context, req Request) (*Response, error) {
	return nil, errors.New("llm: not implemented")
}

// Scripted replays canned responses in order and records every request.
type Scripted struct {
	mu        sync.Mutex
	Responses []Response
	Requests  []Request
}

// Chat returns the next canned response.
func (s *Scripted) Chat(ctx context.Context, req Request) (*Response, error) {
	return nil, errors.New("llm: scripted not implemented")
}
