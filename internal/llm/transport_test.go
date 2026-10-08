package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

type tokenSourceFunc func(context.Context) (string, error)

func (f tokenSourceFunc) AccessToken(ctx context.Context) (string, error) {
	return f(ctx)
}

// TestParseUsageHandlesEveryReportedShape proves the normaliser reads cache
// counters by field presence rather than by which provider is believed to be on
// the other end. A router may serve one session through several upstreams, so
// branching on an assumed provider identity would misparse exactly when it
// matters.
func TestParseUsageHandlesEveryReportedShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want llm.Usage
	}{
		{
			// Flat hit/miss counters with no prompt total.
			name: "flat hit and miss",
			raw:  `{"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100,"completion_tokens":50}`,
			want: llm.Usage{UncachedInputTokens: 100, CacheReadTokens: 900, OutputTokens: 50},
		},
		{
			// A prompt total plus a nested hit counter: the uncached remainder
			// must be derived, not copied.
			name: "nested details with total",
			raw:  `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":900,"cache_write_tokens":0}}`,
			want: llm.Usage{UncachedInputTokens: 100, CacheReadTokens: 900, OutputTokens: 50},
		},
		{
			name: "nested details with writes",
			raw:  `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800,"cache_write_tokens":100}}`,
			want: llm.Usage{UncachedInputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 100, OutputTokens: 50},
		},
		{
			name: "alternate field names",
			raw:  `{"input_tokens":1000,"output_tokens":50,"cache_read_input_tokens":900,"cache_creation_input_tokens":100}`,
			want: llm.Usage{UncachedInputTokens: 0, CacheReadTokens: 900, CacheWriteTokens: 100, OutputTokens: 50},
		},
		{
			// No cache detail at all: everything was uncached.
			name: "no cache detail",
			raw:  `{"prompt_tokens":1000,"completion_tokens":50}`,
			want: llm.Usage{UncachedInputTokens: 1000, OutputTokens: 50},
		},
		{
			name: "absent usage",
			raw:  `{}`,
			want: llm.Usage{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}

			got := llm.ParseUsage(raw)
			if got != tc.want {
				t.Fatalf("ParseUsage() = %+v, want %+v", got, tc.want)
			}
			if got.UncachedInputTokens != got.BilledInputTokens()-got.CacheReadTokens-got.CacheWriteTokens {
				t.Fatalf("buckets are not disjoint: %+v", got)
			}
		})
	}
}

// TestParseUsageNeverProducesNegativeUncached covers a provider that reports a
// cache read larger than the prompt total. Clamping is preferable to emitting a
// negative count that would corrupt the cost model downstream.
func TestParseUsageNeverProducesNegativeUncached(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":150}}`), &raw); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}

	got := llm.ParseUsage(raw)
	if got.UncachedInputTokens < 0 {
		t.Fatalf("uncached input went negative: %+v", got)
	}
}

// capturingServer stands in for a live endpoint so the transport contract can be
// checked without naming or contacting any provider.
func capturingServer(t *testing.T, status int, body string, capture *map[string]any, headers *http.Header) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headers != nil {
			*headers = r.Header.Clone()
		}
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		if capture != nil {
			*capture = decoded
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func sampleRequest() wire.Request {
	return wire.Request{
		Model:  "default",
		System: "You are terse.",
		Messages: []session.Message{
			{Role: session.RoleUser, Content: []session.Block{session.Text("hi")}, Source: "user"},
		},
	}
}

// TestTransportSendsThePortableShape pins what actually goes on the wire: the
// system prompt as a plain string, the session key as a header, and no
// harness-internal fields.
func TestTransportSendsThePortableShape(t *testing.T) {
	var captured map[string]any
	var headers http.Header

	srv := capturingServer(t, 200,
		`{"choices":[{"message":{"role":"assistant","content":"alpha"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		&captured, &headers)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, APIKey: "test-key", Model: "default", SessionID: "session-abc"}

	if _, err := p.Complete(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if headers.Get("X-Session-Id") != "session-abc" {
		t.Fatalf("the session key must be sent for provider affinity; got %q", headers.Get("X-Session-Id"))
	}
	if headers.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("unexpected authorization header: %q", headers.Get("Authorization"))
	}
	if captured["model"] != "default" {
		t.Fatalf("model = %v", captured["model"])
	}

	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("expected system + user, got %v", captured["messages"])
	}

	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("the system prompt must lead, got %v", first["role"])
	}
	if _, isString := first["content"].(string); !isString {
		t.Fatalf("the system prompt must travel as a string for portability, got %T", first["content"])
	}

	encoded, _ := json.Marshal(captured)
	if strings.Contains(string(encoded), `"source"`) {
		t.Fatalf("a harness-internal field reached the wire: %s", encoded)
	}
}

// TestTransportAcceptsBothResponseContentShapes keeps the reader lenient: a
// shape difference should not fail a whole step.
func TestTransportAcceptsBothResponseContentShapes(t *testing.T) {
	cases := map[string]string{
		"string":      `{"choices":[{"message":{"content":"alpha"}}],"usage":{}}`,
		"block array": `{"choices":[{"message":{"content":[{"type":"text","text":"alpha"}]}}],"usage":{}}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := capturingServer(t, 200, body, nil, nil)
			defer srv.Close()

			p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
			resp, err := p.Complete(context.Background(), sampleRequest())
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
			if got := resp.Message.Content[0].Text; got != "alpha" {
				t.Fatalf("content = %q, want alpha", got)
			}
		})
	}
}

// TestTransportSurfacesUpstreamFailures ensures a non-2xx is an error rather
// than a silently empty completion.
func TestTransportSurfacesUpstreamFailures(t *testing.T) {
	srv := capturingServer(t, 429, `{"error":{"message":"slow down"}}`, nil, nil)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	_, err := p.Complete(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("a 429 must surface as an error")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("the status must be reported: %v", err)
	}
	if !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("the upstream message must be retained: %v", err)
	}
}

// TestTransportRequiresABaseURL guards a misconfiguration that would otherwise
// look like a network failure.
func TestTransportRequiresABaseURL(t *testing.T) {
	p := &llm.HTTPProvider{Model: "default"}
	if _, err := p.Complete(context.Background(), sampleRequest()); err == nil {
		t.Fatal("a missing base URL must fail loudly")
	}
}

func TestTransportPrefersOAuthEndpointAndToken(t *testing.T) {
	var seenPath string
	var seenAuthorization string

	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer oauth.Close()

	p := &llm.HTTPProvider{
		BaseURL:      "http://api-key-endpoint.invalid/v1",
		APIKey:       "fallback-key",
		OAuthBaseURL: oauth.URL + "/app/v1",
		OAuth: tokenSourceFunc(func(context.Context) (string, error) {
			return "oauth-token", nil
		}),
		Model: "worker",
	}

	if _, err := p.Complete(context.Background(), wire.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if seenPath != "/app/v1/chat/completions" {
		t.Fatalf("path = %q", seenPath)
	}
	if seenAuthorization != "Bearer oauth-token" {
		t.Fatalf("authorization = %q", seenAuthorization)
	}
}

func TestTransportFallsBackToAPIKeyWhenOAuthHasNoSession(t *testing.T) {
	var seenAuthorization string

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer fallback.Close()

	p := &llm.HTTPProvider{
		BaseURL:      fallback.URL + "/v1",
		APIKey:       "fallback-key",
		OAuthBaseURL: "http://oauth-endpoint.invalid/app/v1",
		OAuth: tokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("not signed in")
		}),
		Model: "worker",
	}

	if _, err := p.Complete(context.Background(), wire.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if seenAuthorization != "Bearer fallback-key" {
		t.Fatalf("authorization = %q", seenAuthorization)
	}
}

// TestLiveCacheHitRate is the end-to-end gate: against a real endpoint, every
// request after the first must report cached input.
//
// It is configured entirely by environment, so the harness names no host, no
// vendor, and no model:
//
//	HARNESS_LIVE_BASE_URL   API root, e.g. https://example.invalid/v1
//	HARNESS_LIVE_API_KEY    credential
//	HARNESS_LIVE_MODEL      model id
//
// It skips when any is unset, so the default suite stays offline. The prefix is
// deliberately long: content below the provider's minimum cacheable unit is not
// cached at all, so a short prompt would measure nothing.
func TestLiveCacheHitRate(t *testing.T) {
	base := os.Getenv("HARNESS_LIVE_BASE_URL")
	key := os.Getenv("HARNESS_LIVE_API_KEY")
	model := os.Getenv("HARNESS_LIVE_MODEL")
	if base == "" || key == "" || model == "" {
		t.Skip("set HARNESS_LIVE_BASE_URL, HARNESS_LIVE_API_KEY and HARNESS_LIVE_MODEL to run the live cache gate")
	}

	system := strings.Repeat("Follow instructions exactly. ", 60)
	p := &llm.HTTPProvider{BaseURL: base, APIKey: key, Model: model, SessionID: "harness-live-gate"}

	messages := []session.Message{{Role: session.RoleUser, Content: []session.Block{session.Text("Say alpha.")}}}

	var usages []llm.Usage
	for step := 0; step < 3; step++ {
		req := wire.Request{Model: model, System: system, Messages: messages}
		resp, err := p.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		usages = append(usages, resp.Usage)

		messages = append(messages,
			resp.Message,
			session.Message{Role: session.RoleUser, Content: []session.Block{session.Text("Again.")}},
		)
	}

	for i, u := range usages {
		t.Logf("step %d: cached=%d uncached=%d output=%d", i, u.CacheReadTokens, u.UncachedInputTokens, u.OutputTokens)
	}

	// The mechanism must be observable: at least one request after the first has to
	// report cached input, or the append-only discipline is not converting into
	// real provider cache reuse at all.
	//
	// This is deliberately NOT asserted per step, and the reason is worth stating
	// because getting it wrong was the first version of this test. A cold step
	// following a warm one almost always means a failover moved the conversation
	// to a different serving provider, and the cache lives on the provider.
	// Measured once against a live endpoint: step 0 was served by one provider
	// after the preferred one rate-limited, steps 1 and 2 by another, so step 1
	// was cold while step 2 hit 128 tokens. Asserting every step would assert the
	// router's stability, which this package neither controls nor can observe -
	// the response carries no provider identity.
	//
	// The split is therefore: prefix discipline is this harness's contract and is
	// pinned strictly by the offline gates in cache_gate_test.go; provider
	// stability is the gateway's contract and is measured here only as the
	// mechanism working at least once.
	hits := 0
	for i := 1; i < len(usages); i++ {
		if usages[i].CacheReadTokens > 0 {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("no request after the first reported cached input across %d steps; "+
			"append-only prefix reuse is not reaching the provider cache", len(usages)-1)
	}

	// Report the series rate so a regression in the numbers stays visible even
	// while the assertion above passes.
	var series llm.Usage
	for i := 1; i < len(usages); i++ {
		series = series.Add(usages[i])
	}
	t.Logf("series after step 1: hit rate %.3f (cached=%d uncached=%d, %d/%d steps hit)",
		series.HitRate(), series.CacheReadTokens, series.UncachedInputTokens, hits, len(usages)-1)
}

// TestTransportSendsToolDeclarations is the assertion whose absence let a real
// bug ship: the request carried a fully assembled, canonically ordered tool list
// that the transport then dropped on the floor. Nothing in the suite noticed,
// because every other test asserted on the RESPONSE.
//
// The lesson from the model-id bug applies here too - assert what you send, not
// only what you receive.
func TestTransportSendsToolDeclarations(t *testing.T) {
	var captured map[string]any

	srv := capturingServer(t, 200,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{}}`, &captured, nil)
	defer srv.Close()

	req := sampleRequest()
	req.Tools = []wire.ToolSchema{
		{Name: "read", Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "bash", Description: "Run a command.", Parameters: json.RawMessage(`{"type":"object"}`)},
	}

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	raw, ok := captured["tools"].([]any)
	if !ok {
		t.Fatalf("the request carried no tools; the model cannot call what it is not told about")
	}
	if len(raw) != 2 {
		t.Fatalf("tools = %d, want 2", len(raw))
	}

	for i, want := range []string{"read", "bash"} {
		entry, ok := raw[i].(map[string]any)
		if !ok {
			t.Fatalf("tool %d is %T", i, raw[i])
		}
		if entry["type"] != "function" {
			t.Fatalf("tool %d type = %v, want function", i, entry["type"])
		}

		fn, ok := entry["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool %d has no function object", i)
		}
		if fn["name"] != want {
			t.Fatalf("tool %d name = %v, want %v (order is the cached prefix)", i, fn["name"], want)
		}
		if _, present := fn["description"]; !present {
			t.Fatalf("tool %d has no description", i)
		}
		if _, present := fn["parameters"]; !present {
			t.Fatalf("tool %d has no parameters", i)
		}
	}
}

// TestTransportOmitsToolsWhenThereAreNone keeps the field out of the body
// entirely rather than sending an empty array, which some endpoints reject.
func TestTransportOmitsToolsWhenThereAreNone(t *testing.T) {
	var captured map[string]any

	srv := capturingServer(t, 200,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{}}`, &captured, nil)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	if _, err := p.Complete(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, present := captured["tools"]; present {
		t.Fatalf("an empty tool list must be omitted, got %v", captured["tools"])
	}
}

// TestEmptyResponseNamesItsFinishReason guards a condition that is intermittent
// in production: a live route returned a single output token with
// finish_reason "length" and no content on roughly half of tool-bearing
// requests. The harness must retry it, so the error has to say what happened -
// an undiagnosable failure is what made this expensive to find.
func TestEmptyResponseNamesItsFinishReason(t *testing.T) {
	srv := capturingServer(t, 200,
		`{"choices":[{"message":{"content":null},"finish_reason":"length"}],"usage":{"completion_tokens":1}}`,
		nil, nil)
	defer srv.Close()

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}

	_, err := p.Complete(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("an empty response must be an error")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Fatalf("the finish reason must be reported: %v", err)
	}
	if !strings.Contains(err.Error(), "output_tokens=1") {
		t.Fatalf("the output token count must be reported: %v", err)
	}
}

// TestTransportSendsTheOutputBudget asserts the outbound max_tokens, for the
// same reason the tool declarations are asserted: a live route silently clamped
// output to a single token when no budget was sent, and nothing in the suite
// noticed because the tests only looked at responses.
func TestTransportSendsTheOutputBudget(t *testing.T) {
	var captured map[string]any

	srv := capturingServer(t, 200,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`, &captured, nil)
	defer srv.Close()

	req := sampleRequest()
	req.MaxTokens = 4096

	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "default"}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if got, ok := captured["max_tokens"].(float64); !ok || int(got) != 4096 {
		t.Fatalf("max_tokens = %v, want 4096", captured["max_tokens"])
	}
}
