package llm_test

import (
	"censi/harness/internal/llm"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStreamReportsServiceErrorsInsteadOfEmptyResponses(t *testing.T) {
	for _, code := range []string{"quota_exhausted", "authentication_error", "context_length_exceeded", "invalid_request_error", "service_unavailable"} {
		t.Run(code, func(t *testing.T) {
			srv := sseServer(t, []string{`{"error":{"code":"` + code + `","message":"private routing identity and trace"}}`}, nil)
			defer srv.Close()
			p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
			_, err := p.Stream(context.Background(), sampleRequest(), nil)
			var failure *llm.ServiceError
			if !errors.As(err, &failure) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "neither text") {
				t.Fatalf("unclassified or leaked service error: %v", err)
			}
			if failure.Retryable() != (code == "service_unavailable") {
				t.Fatalf("incorrect retry classification: %v", failure)
			}
		})
	}
}

func TestStreamAcceptsBufferedCompletionInsideSSE(t *testing.T) {
	srv := sseServer(t, []string{`{"choices":[{"message":{"content":"MaxLabs response","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":3}}`, `{"choices":[],"usage":{"completion_tokens":4}}`}, nil)
	defer srv.Close()
	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
	result, err := p.Stream(context.Background(), sampleRequest(), nil)
	if err != nil || len(result.Message.ToolCalls()) != 1 || result.Message.Content[0].Text != "MaxLabs response" || result.Usage.OutputTokens != 4 {
		t.Fatalf("buffered SSE completion lost: %+v %v", result, err)
	}
}

func TestStreamFailureAfterToolFragmentsDoesNotReturnExecutableCalls(t *testing.T) {
	srv := sseServer(t, []string{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"write","arguments":"{}"}}]}}]}`, `{"error":{"code":"service_unavailable"}}`}, nil)
	defer srv.Close()
	p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
	result, err := p.Stream(context.Background(), sampleRequest(), nil)
	if err == nil || len(result.Message.ToolCalls()) != 0 {
		t.Fatal("failed stream exposed executable tool calls", result, err)
	}
}
