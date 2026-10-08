package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// Delta is one piece of a streamed response.
//
// Reasoning and content are separate fields rather than one stream with a flag,
// because they are shown differently and end differently: reasoning is
// scaffolding that gets collapsed once the answer starts, and it must never be
// mistaken for the answer itself.
type Delta struct {
	// Text is answer content.
	Text string

	// Reasoning is the model's thinking, when the endpoint exposes it.
	Reasoning string
}

// DeltaFunc receives deltas as they arrive.
//
// It is called from the goroutine reading the response body, so an
// implementation that touches shared state must synchronise, and it must not
// block: a slow consumer stalls the stream.
type DeltaFunc func(Delta)

// Streamer is implemented by providers that can emit output incrementally.
//
// It is separate from Provider rather than folded into it so that a stubbed or
// buffered provider stays a one-method type. The agent uses Stream when it is
// available and a sink is configured, and falls back to Complete otherwise.
type Streamer interface {
	Stream(ctx context.Context, req wire.Request, onDelta DeltaFunc) (Response, error)
}

// streamChunk is one SSE frame from a streaming completion.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`

			// Two spellings of the same field are in use across endpoints. Both
			// are read because which one arrives is a property of the route, not
			// of anything the caller controls - and a capability expressed as
			// "read both" costs nothing, where a brand check would be wrong the
			// moment a route changes.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// accumulatingCall reassembles one tool call from its streamed fragments.
//
// A tool call arrives in pieces: the first fragment carries the id and name,
// later fragments carry successive slices of the argument JSON. Concatenating
// the argument slices is the only correct reconstruction - parsing each
// fragment alone would fail on all but the last.
type accumulatingCall struct {
	id   string
	name string
	args strings.Builder
}

// Stream performs a model call and emits text as it arrives.
func (p *HTTPProvider) Stream(ctx context.Context, req wire.Request, onDelta DeltaFunc) (Response, error) {
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
		Stream:        true,
		// Without this the usage block is omitted from a streamed response, and
		// the cache accounting - the reason this harness exists - disappears.
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return Response{}, fmt.Errorf("llm: encoding request: %w", err)
	}

	httpReq, err := p.newRequest(ctx, body)
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client().Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		limit := p.MaxErrorBodyBytes
		if limit <= 0 {
			limit = defaultMaxErrorBody
		}
		excerpt := make([]byte, limit)
		n, _ := resp.Body.Read(excerpt)
		return Response{}, fmt.Errorf("llm: upstream returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(excerpt[:n])))
	}

	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		buffered, err := p.consumeBufferedResponse(resp.Body)
		if err != nil {
			return Response{}, err
		}
		if onDelta != nil {
			for _, block := range buffered.Message.Content {
				if block.Type == session.BlockText && block.Text != "" {
					onDelta(Delta{Text: block.Text})
				}
			}
		}

		return buffered, nil
	}

	return p.consumeStream(resp, onDelta)
}

// consumeStream parses SSE frames and assembles the final response.
func (p *HTTPProvider) consumeStream(resp *http.Response, onDelta DeltaFunc) (Response, error) {
	var (
		text   strings.Builder
		calls  = map[int]*accumulatingCall{}
		order  []int
		finish string
		usage  map[string]any
		sawAny bool
	)

	scanner := bufio.NewScanner(resp.Body)

	// Frames can be larger than the scanner's 64KiB default; a single large tool
	// argument is enough to hit it.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A frame that cannot be parsed is skipped rather than fatal: a
			// keep-alive or a provider-specific comment should not end the turn.
			continue
		}

		sawAny = true

		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}

			reasoning := choice.Delta.Reasoning
			if reasoning == "" {
				reasoning = choice.Delta.ReasoningContent
			}
			if reasoning != "" && onDelta != nil {
				onDelta(Delta{Reasoning: reasoning})
			}

			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				if onDelta != nil {
					onDelta(Delta{Text: choice.Delta.Content})
				}
			}

			for _, fragment := range choice.Delta.ToolCalls {
				call, ok := calls[fragment.Index]
				if !ok {
					call = &accumulatingCall{}
					calls[fragment.Index] = call
					order = append(order, fragment.Index)
				}
				if fragment.ID != "" {
					call.id = fragment.ID
				}
				if fragment.Function.Name != "" {
					call.name = fragment.Function.Name
				}
				call.args.WriteString(fragment.Function.Arguments)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// A truncated stream is a real failure: the assembled message would be
		// incomplete, and logging a partial turn as complete would desynchronise
		// the log from what the model actually produced.
		return Response{}, fmt.Errorf("llm: reading the stream: %w", err)
	}

	if !sawAny {
		return Response{}, fmt.Errorf("llm: the stream contained no usable frames")
	}

	blocks := make([]session.Block, 0, len(order)+1)
	if text.Len() > 0 {
		blocks = append(blocks, session.Text(text.String()))
	}

	for _, index := range order {
		call := calls[index]
		if call.name == "" {
			continue
		}

		args := strings.TrimSpace(call.args.String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return Response{}, fmt.Errorf("llm: tool call %q carried invalid argument JSON", call.name)
		}

		blocks = append(blocks, session.ToolCall(call.id, call.name, json.RawMessage(args)))
	}

	if len(blocks) == 0 {
		return Response{}, fmt.Errorf(
			"llm: the stream contained neither text nor a tool call (finish_reason=%q)", finish)
	}

	return Response{
		Message: session.Message{Role: session.RoleAssistant, Content: blocks},
		Usage:   ParseUsage(usage),
	}, nil
}

// streamOptions asks the endpoint to report usage on the final frame.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}
