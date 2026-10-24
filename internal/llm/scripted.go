package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Scripted replays canned responses in order and records every request. It
// is a test double and is safe for concurrent use. Set Responses before the
// first call; read Requests through Calls once the calls are done.
type Scripted struct {
	mu        sync.Mutex
	Responses []Response
	Requests  []Request
	next      int
}

// Chat returns the next canned response, or an error once Responses are
// exhausted or ctx is done.
func (s *Scripted) Chat(ctx context.Context, req Request) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("llm: scripted: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, cloneRequest(req))
	if s.next >= len(s.Responses) {
		return nil, fmt.Errorf("llm: scripted: responses exhausted after %d calls", len(s.Responses))
	}
	r := cloneResponse(s.Responses[s.next])
	s.next++
	return &r, nil
}

// Calls returns a copy of the requests recorded so far.
func (s *Scripted) Calls() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.Requests))
	for i, r := range s.Requests {
		out[i] = cloneRequest(r)
	}
	return out
}

// Remaining reports how many canned responses have not been returned yet.
func (s *Scripted) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Responses) - s.next
}

func cloneRequest(r Request) Request {
	r.Messages = cloneMessages(r.Messages)
	r.Tools = append([]ToolDef(nil), r.Tools...)
	if r.Temperature != nil {
		t := *r.Temperature
		r.Temperature = &t
	}
	return r
}

func cloneResponse(r Response) Response {
	r.Message = cloneMessages([]Message{r.Message})[0]
	return r
}

func cloneMessages(ms []Message) []Message {
	out := make([]Message, len(ms))
	for i, m := range ms {
		if m.ToolCalls != nil {
			tcs := make([]ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tc.Arguments = append(json.RawMessage(nil), tc.Arguments...)
				tcs[j] = tc
			}
			m.ToolCalls = tcs
		}
		out[i] = m
	}
	return out
}
