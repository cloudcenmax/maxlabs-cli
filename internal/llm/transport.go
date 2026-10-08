package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// HTTPProvider calls a chat-completions endpoint - the request shape nearly
// every provider implements.
//
// It is configured entirely by its fields, so the harness names no host and no
// vendor: whoever mounts it supplies the base URL, credential, and model.
//
// KV Cache effect: none. Transport reports what the provider cached; it never
// assembles a request and never changes a prefix.
type HTTPProvider struct {
	// BaseURL is the API root. "/chat/completions" is appended.
	BaseURL string

	APIKey string

	// OAuthBaseURL and OAuth provide the preferred authenticated endpoint.
	// When OAuth has no usable session, BaseURL/APIKey remain the fallback.
	OAuthBaseURL string
	OAuth        AccessTokenSource

	// Model is the caller-facing model id.
	Model string

	// SessionID pins the conversation to one serving provider for the life of
	// the session. Some routers only keep a conversation on one provider once
	// they observe a cache hit; sending a stable key makes them do so from the
	// first request instead. Omitting it is legal but measurably worse.
	SessionID string

	// Client is optional; a default with a sane timeout is used otherwise.
	Client *http.Client

	// MaxErrorBodyBytes bounds how much of a failed response is retained for
	// diagnosis. A provider error page should not become a memory event.
	MaxErrorBodyBytes int
}

// AccessTokenSource returns a current OAuth access token, refreshing it when
// necessary. Implemented by both the CLI and desktop sessions.
type AccessTokenSource interface {
	AccessToken(context.Context) (string, error)
}

// defaultTimeout bounds a single model request. Agent steps are long, so this is
// generous; it exists to stop a hung connection pinning a session forever.
const defaultTimeout = 5 * time.Minute

const defaultMaxErrorBody = 4 << 10

func (p *HTTPProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: defaultTimeout}
}

// chatRequest is the outbound body. Field order is not significant to the wire,
// but the message encoding is: see wire.Outbound.
type chatRequest struct {
	Model     string              `json:"model"`
	Messages  []wire.Outbound     `json:"messages"`
	Tools     []wire.OutboundTool `json:"tools,omitempty"`
	MaxTokens int                 `json:"max_tokens,omitempty"`

	// WebSearch is the portable flag the gateway interprets. Sent as its own
	// field rather than folded into the model name, so the model string stays a
	// model string.
	WebSearch string `json:"web_search,omitempty"`

	// WebSearchUses is how many searches remain this turn. Omitted when the
	// caller has no opinion, so the gateway's default applies.
	WebSearchUses int  `json:"web_search_uses,omitempty"`
	Stream        bool `json:"stream"`

	// StreamOptions asks for usage on the final frame of a stream.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`

	// Reasoning carries the effort hint, when one is set.
	Reasoning *reasoningParam `json:"reasoning,omitempty"`
}

// reasoningParam is the request shape for a reasoning hint.
//
// Only "effort" is sent. The obvious alternative - disabling reasoning outright
// - is rejected by at least one live route with a 400, so the ladder starts at
// the lowest effort the route accepts rather than at "off".
type reasoningParam struct {
	Effort string `json:"effort"`
}

// reasoningEffort maps a chosen level onto what goes on the wire.
//
// The empty string returns nil so the field is omitted entirely: sending an
// effort of "" is not the same as sending nothing, and would ask the endpoint
// for an effort level it does not have.
func reasoningEffort(level string) *reasoningParam {
	if level == "" || level == "default" || level == "auto" {
		return nil
	}

	return &reasoningParam{Effort: level}
}

// chatResponse is the subset of the completion envelope this harness consumes.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// newRequest builds an authenticated request. Complete and Stream share it so
// the headers - especially the session key that pins provider affinity - cannot
// drift between the buffered and streamed paths.
func (p *HTTPProvider) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	return p.NewEndpointRequest(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(body))
}

// NewEndpointRequest builds a request using OAuth first and the configured API
// key endpoint as a fallback. Model listing and completions share this path so
// they cannot accidentally authenticate as different accounts.
func (p *HTTPProvider) NewEndpointRequest(
	ctx context.Context,
	method string,
	path string,
	body io.Reader,
) (*http.Request, error) {
	baseURL := p.BaseURL
	bearer := p.APIKey

	if p.OAuth != nil && p.OAuthBaseURL != "" {
		if token, err := p.OAuth.AccessToken(ctx); err == nil && token != "" {
			baseURL = p.OAuthBaseURL
			bearer = token
		}
	}

	if baseURL == "" {
		return nil, fmt.Errorf("llm: BaseURL is required")
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")

	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("llm: building request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	if p.SessionID != "" {
		httpReq.Header.Set("X-Session-Id", p.SessionID)
	}

	return httpReq, nil
}

// modelFor decides which model name to send.
//
// The request's model wins. It is the assembled one, so a caller that chose a
// model has its choice honoured; the provider's own field is only the fallback
// for callers that never set one.
//
// Sending the provider's field unconditionally was a real bug: selecting a model
// changed the assembly and the request series, but the request that reached the
// gateway still named the model the provider was built with - so the interface
// offered a switch that did nothing.
func (p *HTTPProvider) modelFor(req wire.Request) string {
	if req.Model != "" {
		return req.Model
	}

	return p.Model
}

// Complete performs one model call.
func (p *HTTPProvider) Complete(ctx context.Context, req wire.Request) (Response, error) {
	if p.BaseURL == "" && p.OAuthBaseURL == "" {
		return Response{}, fmt.Errorf("llm: BaseURL is required")
	}

	body, err := json.Marshal(chatRequest{
		Model:         p.modelFor(req),
		Messages:      wire.Messages(req),
		Tools:         wire.ToolDeclarations(req.Tools),
		MaxTokens:     req.MaxTokens,
		WebSearch:     req.WebSearch,
		WebSearchUses: req.WebSearchUses,
		Reasoning:     reasoningEffort(req.ReasoningEffort),
		Stream:        false,
	})
	if err != nil {
		return Response{}, fmt.Errorf("llm: encoding request: %w", err)
	}

	httpReq, err := p.newRequest(ctx, body)
	if err != nil {
		return Response{}, err
	}

	resp, err := p.client().Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limit := p.MaxErrorBodyBytes
	if limit <= 0 {
		limit = defaultMaxErrorBody
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
		// The credential is never echoed: only the provider's own text is kept.
		return Response{}, fmt.Errorf("llm: upstream returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}

	return p.consumeBufferedResponse(resp.Body)
}

// consumeBufferedResponse parses one ordinary completion envelope. It is
// shared by Complete and by Stream's compatibility path for gateways that
// accept a streaming request but buffer the provider response before replying.
func (p *HTTPProvider) consumeBufferedResponse(body io.Reader) (Response, error) {
	var decoded chatResponse
	if err := json.NewDecoder(body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("llm: decoding response: %w", err)
	}

	if len(decoded.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: response contained no choices")
	}

	text, err := decodeContent(decoded.Choices[0].Message.Content)
	if err != nil {
		return Response{}, err
	}

	// Text and tool calls keep their original order relative to one another:
	// a model may narrate, then call a tool, then narrate again, and the
	// provider caches that sequence.
	blocks := make([]session.Block, 0, len(decoded.Choices[0].Message.ToolCalls)+1)
	if text != "" {
		blocks = append(blocks, session.Text(text))
	}
	for _, call := range decoded.Choices[0].Message.ToolCalls {
		if call.Function.Name == "" {
			continue
		}

		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			// A provider that emits an unparseable argument string has produced
			// an unusable call. Surface it rather than inventing a value the
			// model never asked for.
			return Response{}, fmt.Errorf("llm: tool call %q carried invalid argument JSON", call.Function.Name)
		}

		blocks = append(blocks, session.ToolCall(call.ID, call.Function.Name, json.RawMessage(args)))
	}

	if len(blocks) == 0 {
		// Neither text nor a tool call means there is nothing to continue with.
		// Reporting this as an empty assistant turn would append a message the
		// model never produced and desynchronise the log from the provider.
		//
		// This condition is INTERMITTENT in practice, not exceptional: measured
		// against a live route, roughly half of tool-bearing requests returned a
		// single output token with finish_reason "length" and no content, while
		// identical requests succeeded. It is therefore a condition to retry, not
		// to fail on, and the finish reason is named so the retry is diagnosable
		// after the fact.
		return Response{}, fmt.Errorf(
			"llm: the response contained neither text nor a tool call (finish_reason=%q, output_tokens=%d)",
			decoded.Choices[0].FinishReason, ParseUsage(decoded.Usage).OutputTokens)
	}

	return Response{
		Message: session.Message{Role: session.RoleAssistant, Content: blocks},
		Usage:   ParseUsage(decoded.Usage),
	}, nil
}

// decodeContent accepts either a plain string or a block array. Being lenient
// here costs nothing and avoids failing a whole step over a shape difference.
func decodeContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}

	// A tool-call-only turn conventionally carries a null content field.
	if strings.TrimSpace(string(raw)) == "null" {
		return "", nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("llm: unrecognised message content: %w", err)
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}

	return strings.Join(parts, ""), nil
}

// Usage field names, grouped by the shape they appear in.
//
// These are matched by presence rather than by which provider is believed to be
// on the other end. A router may serve the same model through several upstreams
// in one session, so branching on an assumed provider identity would misparse
// exactly when it matters.
var (
	readFields = []string{
		"prompt_cache_hit_tokens", // flat hit counter
		"cached_tokens",           // nested under prompt_tokens_details
		"cache_read_input_tokens", // alternate name
	}
	writeFields = []string{
		"cache_write_tokens",          // nested under prompt_tokens_details
		"cache_creation_input_tokens", // alternate name
	}
	totalInputFields = []string{"prompt_tokens", "input_tokens"}
	outputFields     = []string{"completion_tokens", "output_tokens"}
	reasoningFields  = []string{"reasoning_tokens", "completion_tokens_details.reasoning_tokens"}
	missFields       = []string{"prompt_cache_miss_tokens"}

	// A provider that runs server-side tools reports how many it ran. Named
	// both flat and nested: the nested form is what the usage block uses, and
	// flatten lifts the details map without descending into it.
	searchFields = []string{"web_search_requests", "server_tool_use.web_search_requests"}
)

// ParseUsage normalises a provider usage block into DISJOINT buckets.
//
// The buckets must not overlap: UncachedInputTokens is uncached input only, and
// billed input is the sum of the three input buckets. Collapsing them would
// overstate miss cost by the cache discount - the exact quantity this harness
// exists to minimise - so the arithmetic is done once, here.
func ParseUsage(raw map[string]any) Usage {
	if raw == nil {
		return Usage{}
	}

	flat := flatten(raw)

	var u Usage
	u.OutputTokens = firstInt(flat, outputFields)
	u.ReasoningTokens = firstInt(flat, reasoningFields)
	u.WebSearches = firstInt(flat, searchFields)

	hit, hitFound := firstIntOK(flat, readFields)
	miss, missFound := firstIntOK(flat, missFields)
	write := firstInt(flat, writeFields)
	total, totalFound := firstIntOK(flat, totalInputFields)

	switch {
	case hitFound || missFound:
		u.CacheReadTokens = hit
		u.CacheWriteTokens = write
		if totalFound {
			// A single prompt total that already includes cached tokens: the
			// uncached remainder is what is left after removing them.
			u.UncachedInputTokens = clampNonNegative(total - hit - write)
			return u
		}
		// No total: the miss counter is the uncached figure.
		u.UncachedInputTokens = miss
		return u

	case totalFound:
		// A total with no cache detail at all: everything was uncached.
		u.CacheWriteTokens = write
		u.UncachedInputTokens = clampNonNegative(total - write)
		return u
	}

	return u
}

// flatten returns the usage object's own keys plus those of its
// `prompt_tokens_details` child, so nested and flat shapes are read uniformly.
// A flat key wins, since it is the more specific of the two.
func flatten(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw)+4)

	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		for k, v := range details {
			out[k] = v
		}
	}

	// Server-tool usage is one level down, and is the only place a provider
	// says how many searches it ran.
	if tools, ok := raw["server_tool_use"].(map[string]any); ok {
		for k, v := range tools {
			out["server_tool_use."+k] = v
		}
	}
	for k, v := range raw {
		out[k] = v
	}

	return out
}

func firstInt(values map[string]any, keys []string) int {
	v, _ := firstIntOK(values, keys)
	return v
}

func firstIntOK(values map[string]any, keys []string) (int, bool) {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		if n, ok := asInt(raw); ok {
			return n, true
		}
	}
	return 0, false
}

// asInt accepts the numeric encodings a JSON decoder may produce.
func asInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		if v != math.Trunc(v) || v < 0 {
			return 0, false
		}
		return int(v), true
	case int:
		if v < 0 {
			return 0, false
		}
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil && n >= 0 {
			return int(n), true
		}
	}
	return 0, false
}

func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
