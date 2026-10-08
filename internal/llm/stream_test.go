package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"censi/harness/internal/llm"
	"censi/harness/internal/wire"
)

// decodeJSON reads a request body into a map.
func decodeJSON(r *http.Request, into *map[string]any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

// sseServer replays a recorded event stream.
func sseServer(t *testing.T, frames []string, capture *map[string]any) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			var body map[string]any
			_ = decodeJSON(r, &body)
			*capture = body
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestStreamAssemblesTextAndEmitsDeltas is the streaming contract: the caller
// sees text as it arrives, and the final response equals what a buffered call
// would have produced.
func TestStreamAssemblesTextAndEmitsDeltas(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":", "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":8}}}`,
	}

	srv := sseServer(t, frames, nil)
	defer srv.Close()

	var deltas []string

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	resp, err := p.Stream(context.Background(), sampleRequest(), func(delta llm.Delta) {
		deltas = append(deltas, delta.Text)
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if got := strings.Join(deltas, ""); got != "Hello, world" {
		t.Fatalf("deltas = %q", got)
	}
	if resp.Message.Content[0].Text != "Hello, world" {
		t.Fatalf("assembled text = %q", resp.Message.Content[0].Text)
	}

	// Usage arrives only on the final frame, and carries the cache split the
	// whole design depends on.
	if resp.Usage.CacheReadTokens != 8 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// Some gateways accept stream=true upstream but buffer and normalise the
// provider response into one JSON completion. That is still a valid answer;
// treating it as an empty SSE stream made OAuth calls fail after the Gateway
// had already completed the model request successfully.
func TestStreamAcceptsABufferedJSONCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"choices":[{"message":{"content":"Hello from the gateway","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":8,"completion_tokens":4}
		}`)
	}))
	defer srv.Close()

	var deltas []string
	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	resp, err := p.Stream(context.Background(), sampleRequest(), func(delta llm.Delta) {
		deltas = append(deltas, delta.Text)
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if got := strings.Join(deltas, ""); got != "Hello from the gateway" {
		t.Fatalf("deltas = %q", got)
	}
	if resp.Message.Content[0].Text != "Hello from the gateway" || resp.Usage.OutputTokens != 4 {
		t.Fatalf("response = %+v", resp)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "read" || string(calls[0].Arguments) != `{"path":"README.md"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
}

// TestStreamReassemblesFragmentedToolCalls covers the case that makes streaming
// tool use fiddly: a tool call arrives in pieces, and only the concatenation of
// the argument fragments is valid JSON.
func TestStreamReassemblesFragmentedToolCalls(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
	}

	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	resp, err := p.Stream(context.Background(), sampleRequest(), nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(calls))
	}
	if calls[0].Name != "read" || calls[0].CallID != "call-1" {
		t.Fatalf("call = %+v", calls[0])
	}
	if string(calls[0].Arguments) != `{"path":"a.txt"}` {
		t.Fatalf("assembled arguments = %s", calls[0].Arguments)
	}
}

// TestStreamRequestsUsageOnTheFinalFrame: without stream_options the usage block
// is omitted, and the cache accounting disappears with it.
func TestStreamRequestsUsageOnTheFinalFrame(t *testing.T) {
	var captured map[string]any

	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`}, &captured)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	if _, err := p.Stream(context.Background(), sampleRequest(), nil); err != nil {
		t.Fatalf("stream: %v", err)
	}

	if captured["stream"] != true {
		t.Fatalf("stream = %v, want true", captured["stream"])
	}

	opts, ok := captured["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage true", captured["stream_options"])
	}
}

// TestStreamRejectsAnEmptyStream: reporting an empty assistant turn would append
// a message the model never produced.
func TestStreamRejectsAnEmptyStream(t *testing.T) {
	srv := sseServer(t, []string{`{"choices":[{"delta":{},"finish_reason":"length"}]}`}, nil)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	_, err := p.Stream(context.Background(), sampleRequest(), nil)
	if err == nil {
		t.Fatal("an empty stream must be an error")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Fatalf("the finish reason must be reported: %v", err)
	}
}

// TestStreamSkipsUnparseableFrames keeps a keep-alive or provider comment from
// ending a turn.
func TestStreamSkipsUnparseableFrames(t *testing.T) {
	frames := []string{
		`: keep-alive`,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`not json at all`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}

	srv := sseServer(t, frames, nil)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	resp, err := p.Stream(context.Background(), sampleRequest(), nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if resp.Message.Content[0].Text != "ok" {
		t.Fatalf("text = %q", resp.Message.Content[0].Text)
	}
}

// TestStreamSendsReasoningEffort pins the outbound parameter. Asserting what is
// sent, not what comes back, is the lesson from the missing tools.
func TestStreamSendsReasoningEffort(t *testing.T) {
	var captured map[string]any

	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`}, &captured)
	defer srv.Close()

	req := sampleRequest()
	req.ReasoningEffort = "low"

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	if _, err := p.Stream(context.Background(), req, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}

	reasoning, ok := captured["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("no reasoning parameter was sent: %v", captured["reasoning"])
	}
	if reasoning["effort"] != "low" {
		t.Fatalf("effort = %v, want low", reasoning["effort"])
	}
}

// TestReasoningEffortIsOmittedWhenUnset: sending an effort of "" is not the same
// as sending nothing - it asks for a level the endpoint does not have.
func TestReasoningEffortIsOmittedWhenUnset(t *testing.T) {
	for _, level := range []string{"", "default", "auto"} {
		var captured map[string]any

		srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`}, &captured)
		defer srv.Close()

		req := sampleRequest()
		req.ReasoningEffort = level

		p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
		if _, err := p.Stream(context.Background(), req, nil); err != nil {
			t.Fatalf("level %q: %v", level, err)
		}

		if _, present := captured["reasoning"]; present {
			t.Fatalf("level %q sent a reasoning parameter: %v", level, captured["reasoning"])
		}
	}
}

// TestReasoningEffortDoesNotPerturbThePrefix is the property that makes the
// setting cheap to change mid-session.
//
// Effort is a sampling parameter; it does not alter the token sequence the
// provider caches. If it entered the hash, every change would discard the whole
// cached prefix for a setting that changes no bytes the model reads.
func TestReasoningEffortDoesNotPerturbThePrefix(t *testing.T) {
	base := sampleRequest()

	low := sampleRequest()
	low.ReasoningEffort = "low"

	high := sampleRequest()
	high.ReasoningEffort = "high"

	baseHash, err := wire.Hash(base)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	baseUnits, err := wire.Units(base)
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	for name, req := range map[string]wire.Request{"low": low, "high": high} {
		gotHash, err := wire.Hash(req)
		if err != nil {
			t.Fatalf("%s: hash: %v", name, err)
		}
		if gotHash != baseHash {
			t.Fatalf("%s changed the prompt hash, which would discard the cached prefix", name)
		}

		gotUnits, err := wire.Units(req)
		if err != nil {
			t.Fatalf("%s: units: %v", name, err)
		}
		if ok, at := wire.Extends(baseUnits, gotUnits); !ok {
			t.Fatalf("%s broke prefix extension at unit %d", name, at)
		}
	}
}

// TestOutboundModelComesFromTheRequest is the fix for a model switcher that
// switched nothing.
//
// The transport sent the provider's own model unconditionally, so choosing a
// different one changed the assembled request and its series while the request
// that reached the gateway still named the original. Like the missing tool
// declarations, this is only visible by asserting what is SENT.
func TestOutboundModelComesFromTheRequest(t *testing.T) {
	var captured map[string]any

	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`}, &captured)
	defer srv.Close()

	req := sampleRequest()
	req.Model = "pro"

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
	if _, err := p.Stream(context.Background(), req, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}

	if captured["model"] != "pro" {
		t.Fatalf("the request went out as %v, want the model the caller chose", captured["model"])
	}
}

// TestOutboundModelFallsBackToTheProvider keeps the old behaviour for callers
// that never set one.
func TestOutboundModelFallsBackToTheProvider(t *testing.T) {
	var captured map[string]any

	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`}, &captured)
	defer srv.Close()

	// The shared fixture names a model, so clear it to exercise the fallback.
	req := sampleRequest()
	req.Model = ""

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
	if _, err := p.Stream(context.Background(), req, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}

	if captured["model"] != "worker" {
		t.Fatalf("model = %v, want the provider's default", captured["model"])
	}
}
