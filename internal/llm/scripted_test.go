package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestScriptedOrderAndExhaustion(t *testing.T) {
	s := &Scripted{Responses: []Response{
		{Message: Message{Role: "assistant", Content: "one"}},
		{Message: Message{Role: "assistant", Content: "two"}},
	}}
	ctx := context.Background()
	for _, want := range []string{"one", "two"} {
		r, err := s.Chat(ctx, Request{Model: want})
		if err != nil {
			t.Fatal(err)
		}
		if r.Message.Content != want {
			t.Errorf("got %q want %q", r.Message.Content, want)
		}
	}
	if _, err := s.Chat(ctx, Request{}); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("err = %v", err)
	}
	calls := s.Calls()
	if len(calls) != 3 || calls[0].Model != "one" {
		t.Errorf("calls = %+v", calls)
	}
	if s.Remaining() != 0 {
		t.Errorf("remaining = %d", s.Remaining())
	}
}

func TestScriptedRecordsCopies(t *testing.T) {
	s := &Scripted{Responses: []Response{{}}}
	msgs := []Message{{Role: "user", Content: "a"}}
	s.Chat(context.Background(), Request{Messages: msgs})
	msgs[0].Content = "mutated"
	if s.Calls()[0].Messages[0].Content != "a" {
		t.Error("recorded request aliases caller memory")
	}
}

func TestScriptedContextDone(t *testing.T) {
	s := &Scripted{Responses: []Response{{}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Chat(ctx, Request{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestScriptedConcurrent(t *testing.T) {
	s := &Scripted{Responses: make([]Response, 50)}
	var wg sync.WaitGroup
	for range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Chat(context.Background(), Request{})
		}()
	}
	wg.Wait()
	if len(s.Calls()) != 60 || s.Remaining() != 0 {
		t.Errorf("calls %d remaining %d", len(s.Calls()), s.Remaining())
	}
}
