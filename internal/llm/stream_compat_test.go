package llm_test

import (
	"censi/harness/internal/llm"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStreamQuotaRetainsOnlyPublicWindowDetails(t *testing.T) {
	for _, window := range []string{"weekly", "h5", "private-routing-detail"} {
		raw, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "quota_exhausted", "type": "rate_limit_error", "window": window, "resets_at": "2026-10-24T04:41:37+00:00", "message": "private diagnostic trace"}})
		srv := sseServer(t, []string{string(raw)}, nil)
		p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
		_, err := p.Stream(context.Background(), sampleRequest(), nil)
		srv.Close()
		var failure *llm.ServiceError
		if !errors.As(err, &failure) || failure.Retryable() || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe quota error %v", err)
		}
		if window != "private-routing-detail" && (failure.Window != window || failure.ResetsAt != "2026-10-24T04:41:37Z" || !strings.Contains(err.Error(), "allowance cannot cover")) {
			t.Fatalf("window details lost: %+v", failure)
		}
	}
}

func TestStreamReportsServiceErrorsInsteadOfEmptyResponses(t *testing.T) {
	for _, code := range []string{"quota_exhausted", "authentication_error", "context_length_exceeded", "invalid_request_error", "service_unavailable", "rate_limit_exceeded", "concurrency_limit_exceeded"} {
		t.Run(code, func(t *testing.T) {
			srv := sseServer(t, []string{`{"error":{"code":"` + code + `","message":"private routing identity and trace"}}`}, nil)
			defer srv.Close()
			p := &llm.HTTPProvider{BaseURL: srv.URL, Model: "worker"}
			_, err := p.Stream(context.Background(), sampleRequest(), nil)
			var failure *llm.ServiceError
			if !errors.As(err, &failure) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "neither text") {
				t.Fatalf("unclassified or leaked service error: %v", err)
			}
			if failure.Retryable() != (code == "service_unavailable" || code == "rate_limit_exceeded" || code == "concurrency_limit_exceeded") {
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
