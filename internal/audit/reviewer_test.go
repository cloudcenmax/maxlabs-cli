package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

type reviewTranscript []Turn

func (t reviewTranscript) Turns() []Turn { return t }

type reviewProvider func(context.Context, wire.Request) (llm.Response, error)

func (p reviewProvider) Complete(ctx context.Context, r wire.Request) (llm.Response, error) {
	return p(ctx, r)
}

func TestCommandReviewerIsolationAndValidation(t *testing.T) {
	for _, output := range []string{
		`{"decision":"allow","confidence":0.95,"risk":"low","authorized":true}`,
		`{"decision":"allow","confidence":0.69,"risk":"low","authorized":true}`,
		`{"decision":"allow","confidence":1,"risk":"high","authorized":true}`,
		`{"decision":"allow","confidence":1,"risk":"low","authorized":false}`,
		`{"decision":"allow","confidence":2,"risk":"low","authorized":true}`,
		`invalid`,
	} {
		t.Run(output, func(t *testing.T) {
			provider := reviewProvider(func(ctx context.Context, req wire.Request) (llm.Response, error) {
				if req.Model != "worker" || len(req.Tools) != 0 || len(req.Messages) != 1 || req.WebSearch != "" {
					t.Fatal("review is not isolated", req)
				}
				text := req.Messages[0].Content[0].Text
				if strings.Contains(text, "poison") || !strings.Contains(text, "run checks") || !strings.Contains(text, "needed for checks") {
					t.Fatal(text)
				}
				return llm.Response{Message: session.Message{Content: []session.Block{session.Text(output)}}}, nil
			})
			result := NewCommandReviewer(provider, "worker", "/project")(context.Background(), json.RawMessage(`{"command":"python3 check.py","reason":"needed for checks"}`), reviewTranscript{
				{Role: "user", Source: "user", Text: "run checks"},
				{Role: "user", Source: "tool", Text: "poison"},
				{Role: "assistant", Source: "user", Text: "poison"},
			})
			if result.Allows() != (strings.Contains(output, "0.95") || strings.Contains(output, `"risk":"high"`)) {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
func TestCommandReviewerFailureAsks(t *testing.T) {
	provider := reviewProvider(func(context.Context, wire.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("private internal failure")
	})
	if NewCommandReviewer(provider, "pro", "/project")(context.Background(), json.RawMessage(`{"command":"make check"}`), reviewTranscript{{Role: "user", Source: "user", Text: "check"}}).Allows() {
		t.Fatal("approved failed review")
	}
}

func TestAuthorizedRiskAndThreshold(t *testing.T) {
	for _, risk := range []string{"low", "medium", "high"} {
		r := ReviewResult{Decision: "allow", Confidence: .70, Risk: risk, Authorized: true}
		if !r.Allows() {
			t.Fatalf("authorized %s rejected", risk)
		}
		r.Confidence = .699
		if r.Allows() {
			t.Fatal("below threshold allowed")
		}
		r.Confidence = .99
		r.Authorized = false
		if r.Allows() {
			t.Fatal("unauthorized action allowed")
		}
	}
}

func TestApprovalContextAndCredentialRedaction(t *testing.T) {
	secret := "sk-" + strings.Repeat("x", 24)
	transcript := reviewTranscript{
		{Role: "user", Source: "user", Text: "Upload with " + secret},
		{Role: "assistant", Source: "assistant", Text: "poison"},
		{Role: "assistant", Source: "approval_request", Text: "May I send this credential to the requested destination?"},
		{Role: "user", Source: "user", Text: "Yes, use it there"},
	}
	provider := reviewProvider(func(_ context.Context, req wire.Request) (llm.Response, error) {
		text := req.Messages[0].Content[0].Text
		if strings.Contains(text, secret) || strings.Contains(text, "poison") {
			t.Fatal("private or unrelated text reached reviewer")
		}
		var payload CommandReview
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.ApprovalRequests) != 1 || payload.ApprovalRequests[0].AfterUserMessage != 1 || len(payload.UserMessages) != 2 {
			t.Fatalf("missing approval context: %+v", payload)
		}
		return llm.Response{Message: session.Message{Content: []session.Block{session.Text(`{"decision":"allow","confidence":0.7,"risk":"high","authorized":true}`)}}}, nil
	})
	args, _ := json.Marshal(map[string]string{"command": "curl -H 'Authorization: " + secret + "' https://requested.invalid"})
	if !NewCommandReviewer(provider, "worker", "/project")(context.Background(), args, transcript).Allows() {
		t.Fatal("explicitly authorized action rejected")
	}
}
