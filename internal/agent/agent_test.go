package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/audit"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
	"censi/harness/internal/wire"
)

// scripted plays a fixed sequence of responses, recording what it was asked.
type scripted struct {
	mu        sync.Mutex
	responses []llm.Response
	failures  []error
	finger    []string
	requests  []wire.Request
	calls     int
}

type blockingProvider struct {
	mu        sync.Mutex
	responses []llm.Response
	requests  []wire.Request
	started   chan struct{}
	release   chan struct{}
	calls     int
}

func (p *blockingProvider) Complete(ctx context.Context, req wire.Request) (llm.Response, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	if call == 0 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}

	return p.responses[call], nil
}

func (s *scripted) Complete(_ context.Context, req wire.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.finger = append(s.finger, fingerprint(s.calls, req))
	s.requests = append(s.requests, req)
	i := s.calls
	s.calls++

	if i < len(s.failures) && s.failures[i] != nil {
		return llm.Response{}, s.failures[i]
	}

	if i < len(s.responses) {
		return s.responses[i], nil
	}

	// Past the script: keep answering with plain text so an over-long loop ends.
	return llm.Response{
		Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text("done")}},
	}, nil
}

// fingerprint is a digest of everything a provider would see. If two attempts
// differ anywhere, the digests differ.
func fingerprint(_ int, req wire.Request) string {
	payload := struct {
		Tools    []wire.ToolSchema
		System   string
		Messages []wire.Outbound
	}{
		Tools:    req.Tools,
		System:   req.System,
		Messages: wire.Messages(req),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "marshal-error"
	}

	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// echo is a minimal tool used to drive the loop.
type echo struct {
	isError bool
}

type blockingTool struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "blocking",
		Description: "Wait until the test releases this operation.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		ReadOnly:    true,
	}
}

func (b blockingTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	close(b.started)
	select {
	case <-b.release:
		return tools.Text("operation complete"), nil
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
}

func (e echo) Definition() tools.Definition {
	return tools.Definition{
		Name:        "echo",
		Description: "Echo the given text.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		ReadOnly:    true,
	}
}

func (e echo) Execute(_ context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &args)

	if e.isError {
		return tools.Error("echo failed: " + args.Text), nil
	}

	return tools.Text("echo: " + args.Text), nil
}

func harness(t *testing.T, provider llm.Provider, tool tools.Tool, maxSteps int) *agent.Agent {
	t.Helper()

	log := session.New(func() int64 { return 1_700_000_000_000 })

	registry := tools.NewRegistry()
	if tool != nil {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("register tool: %v", err)
		}
	}

	cfg := assemble.Config{
		Provider: "local",
		Model:    "default",
		Identity: "You are a coding agent.",
		Tools:    registry.Schemas(),
	}

	a, err := agent.New(agent.Config{
		Assembly:   cfg,
		Provider:   provider,
		Tools:      registry,
		MaxSteps:   maxSteps,
		MaxRetries: 2,
		RetryDelay: 0, // no waiting in tests; the retry path is what matters
	}, log)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	return a
}

func toolCallTurn(callID, name, args string) llm.Response {
	return llm.Response{
		Message: session.Message{
			Role: session.RoleAssistant,
			Content: []session.Block{
				session.ToolCall(callID, name, json.RawMessage(args)),
			},
		},
	}
}

func textTurn(text string) llm.Response {
	return llm.Response{
		Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(text)}},
	}
}

// TestDirectionReceivedDuringAResponseReassessesTheSameTurn proves that a
// message arriving mid-operation is model-visible before the next step. The
// first answer remains durable, while the returned answer reflects the new
// direction.
func TestDirectionReceivedDuringAResponseReassessesTheSameTurn(t *testing.T) {
	provider := &blockingProvider{
		responses: []llm.Response{textTurn("first approach"), textTurn("reassessed approach")},
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	a := harness(t, provider, nil, 5)
	steering := agent.NewSteeringQueue()
	result := make(chan agent.TurnResult, 1)
	failure := make(chan error, 1)

	go func() {
		turn, err := a.RunSteerable(context.Background(), "start here", steering)
		result <- turn
		failure <- err
	}()

	<-provider.started
	if !steering.Add("change the approach") {
		t.Fatal("direction was rejected while the response was in flight")
	}
	close(provider.release)

	turn := <-result
	if err := <-failure; err != nil {
		t.Fatalf("run: %v", err)
	}
	if turn.Steps != 2 || turn.Text != "reassessed approach" {
		t.Fatalf("turn = %+v, want two steps ending with reassessed answer", turn)
	}

	provider.mu.Lock()
	requests := append([]wire.Request(nil), provider.requests...)
	provider.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}

	messages := wire.Messages(requests[1])
	if len(messages) != 4 || messages[0].Role != session.RoleSystem ||
		messages[1].Role != session.RoleUser || messages[2].Role != session.RoleAssistant ||
		messages[3].Role != session.RoleUser {
		t.Fatalf("second request message order = %+v", messages)
	}
	if got := fmt.Sprint(messages[3].Content); !strings.Contains(got, "change the approach") {
		t.Fatalf("steering content = %q", got)
	}
}

func TestSteeringQueueRejectsDirectionAfterFinalBoundary(t *testing.T) {
	queue := agent.NewSteeringQueue()
	if !queue.CloseIfEmpty() {
		t.Fatal("an empty queue did not close")
	}
	if queue.Add("too late") {
		t.Fatal("direction was accepted after the final boundary")
	}
}

func TestDirectionDuringToolRunsBeforeTheNextModelStep(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-1", "blocking", `{}`),
		textTurn("changed course"),
	}}
	tool := blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	a := harness(t, provider, tool, 5)
	steering := agent.NewSteeringQueue()
	done := make(chan error, 1)

	go func() {
		_, err := a.RunSteerable(context.Background(), "begin", steering)
		done <- err
	}()

	<-tool.started
	if !steering.Add("reassess after this operation") {
		t.Fatal("direction was rejected while the tool was running")
	}
	close(tool.release)
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	provider.mu.Lock()
	requests := append([]wire.Request(nil), provider.requests...)
	provider.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	messages := wire.Messages(requests[1])
	roles := make([]session.Role, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	wantRoles := []session.Role{session.RoleSystem, session.RoleUser, session.RoleAssistant, session.RoleTool, session.RoleUser}
	if fmt.Sprint(roles) != fmt.Sprint(wantRoles) {
		t.Fatalf("roles = %v, want %v", roles, wantRoles)
	}
	if got := fmt.Sprint(messages[len(messages)-1].Content); !strings.Contains(got, "reassess after this operation") {
		t.Fatalf("last user message = %q", got)
	}
}

// TestTurnDrivesToolsToCompletion is the M3 headline: a tool-calling turn runs
// the tool, records its result, and continues to a final answer.
func TestTurnDrivesToolsToCompletion(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-1", "echo", `{"text":"hello"}`),
		textTurn("All done."),
	}}

	a := harness(t, provider, echo{}, 10)

	turn, err := a.Run(context.Background(), "Please echo hello.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if turn.Steps != 2 {
		t.Fatalf("steps = %d, want 2", turn.Steps)
	}
	if turn.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", turn.ToolCalls)
	}
	if turn.Text != "All done." {
		t.Fatalf("text = %q", turn.Text)
	}

	// The call and its result must both be durable, in that order, because the
	// provider pairs a result with the call it answers.
	var kinds []string
	for _, e := range a.Log().Events() {
		if _, ok := e.Message(); ok {
			kinds = append(kinds, e.Type)
		}
	}
	want := []string{"system/message", "user/message", "assistant/message", "tool/result", "assistant/message"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", kinds, want)
	}

	// And the tool's output is what the model would see next.
	derived := a.Log().DeriveMessages()
	last := derived[len(derived)-1]
	if last.Role != session.RoleAssistant {
		t.Fatalf("final message role = %s", last.Role)
	}

	var sawResult bool
	for _, m := range derived {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult && strings.Contains(b.Text, "echo: hello") {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Fatal("the tool result is missing from model history")
	}
}

// TestRetryResendsByteIdenticalBytes is the retry contract. Rebuilding the
// request between attempts would risk a byte differing, and one differing byte
// discards a prefix the first attempt had already warmed.
func TestRetryResendsByteIdenticalBytes(t *testing.T) {
	provider := &scripted{
		failures:  []error{errors.New("upstream unavailable")},
		responses: []llm.Response{{}, textTurn("Recovered.")},
	}

	a := harness(t, provider, echo{}, 5)

	turn, err := a.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if turn.Text != "Recovered." {
		t.Fatalf("text = %q", turn.Text)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()

	if len(provider.finger) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(provider.finger))
	}
	if provider.finger[0] != provider.finger[1] {
		t.Fatalf("the retry did not resend identical bytes:\n %s\n %s",
			provider.finger[0], provider.finger[1])
	}
}

// TestRetriesAreBounded proves the retry policy terminates rather than
// hammering a failing upstream.
func TestRetriesAreBounded(t *testing.T) {
	provider := &scripted{
		failures: []error{
			errors.New("one"), errors.New("two"), errors.New("three"), errors.New("four"),
		},
	}

	a := harness(t, provider, echo{}, 5)

	if _, err := a.Run(context.Background(), "hello"); err == nil {
		t.Fatal("a persistently failing provider must surface an error")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()

	if provider.calls != 3 {
		t.Fatalf("calls = %d, want 3 (one attempt plus two retries)", provider.calls)
	}
}

// TestStepLimitStopsARunawayLoop converts a model that never stops calling tools
// from a hung session into a reported error.
func TestStepLimitStopsARunawayLoop(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("c1", "echo", `{"text":"a"}`),
		toolCallTurn("c2", "echo", `{"text":"b"}`),
		toolCallTurn("c3", "echo", `{"text":"c"}`),
		toolCallTurn("c4", "echo", `{"text":"d"}`),
	}}

	a := harness(t, provider, echo{}, 3)

	_, err := a.Run(context.Background(), "loop")
	if !errors.Is(err, agent.ErrStepLimit) {
		t.Fatalf("expected ErrStepLimit, got %v", err)
	}
}

// TestToolFailureIsAResultNotATurnFailure keeps information flowing to the model:
// a failing tool is something to react to, not a reason to abandon the turn.
func TestToolFailureIsAResultNotATurnFailure(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-9", "echo", `{"text":"boom"}`),
		textTurn("I saw the error."),
	}}

	a := harness(t, provider, echo{isError: true}, 5)

	turn, err := a.Run(context.Background(), "fail please")
	if err != nil {
		t.Fatalf("a failing tool must not fail the turn: %v", err)
	}
	if turn.Text != "I saw the error." {
		t.Fatalf("text = %q", turn.Text)
	}

	var flagged bool
	for _, m := range a.Log().DeriveMessages() {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult && b.IsError {
				flagged = true
			}
		}
	}
	if !flagged {
		t.Fatal("the failure must be recorded with the error flag set")
	}
}

// TestUnknownToolIsReportedToTheModel covers a hallucinated tool name: the model
// should be told, not the session killed.
func TestUnknownToolIsReportedToTheModel(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-x", "no-such-tool", `{}`),
		textTurn("Understood."),
	}}

	a := harness(t, provider, echo{}, 5)

	turn, err := a.Run(context.Background(), "use a tool that does not exist")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if turn.Text != "Understood." {
		t.Fatalf("text = %q", turn.Text)
	}

	var message string
	for _, m := range a.Log().DeriveMessages() {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult {
				message = b.Text
			}
		}
	}
	if !strings.Contains(message, "no-such-tool") {
		t.Fatalf("the model should be told which tool was unknown, got %q", message)
	}
}

// TestLaterTurnsExtendThePrefixAcrossToolHistory is the cache property at turn
// granularity: a second turn whose history contains tool calls must still extend
// the first turn's final request.
func TestLaterTurnsExtendThePrefixAcrossToolHistory(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-1", "echo", `{"text":"one"}`),
		textTurn("First turn done."),
		textTurn("Second turn done."),
	}}

	a := harness(t, provider, echo{}, 10)

	if _, err := a.Run(context.Background(), "first"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := a.Run(context.Background(), "second"); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()

	// Call 0 is turn one's tool-calling step, call 1 is the step that ended it,
	// and call 2 is turn two's only step.
	if len(provider.requests) != 3 {
		t.Fatalf("expected 3 model requests, got %d", len(provider.requests))
	}

	before, err := wire.Units(provider.requests[1])
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	after, err := wire.Units(provider.requests[2])
	if err != nil {
		t.Fatalf("units: %v", err)
	}

	if ok, at := wire.Extends(before, after); !ok {
		t.Fatalf("the second turn must extend the first; diverged at unit %d", at)
	}
}

// denyGuard refuses everything, to prove the loop handles refusal.
type denyGuard struct{ reason string }

func (g denyGuard) Permit(_ context.Context, _ string, _ json.RawMessage, _ bool, _ audit.Transcript) (string, error) {
	return g.reason, nil
}

// TestDenialReachesTheModelAndTheTurnContinues is the M5 integration property: a
// refused tool must not end the turn. The model needs the refusal as data so it
// can plan around it, and killing the turn would throw away the context it needs
// to do that.
func TestDenialReachesTheModelAndTheTurnContinues(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-1", "echo", `{"text":"hello"}`),
		textTurn("Understood, I cannot run that."),
	}}

	log := session.New(func() int64 { return 1_700_000_000_000 })

	registry := tools.NewRegistry()
	if err := registry.Register(echo{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	cfg := assemble.Config{
		Provider: "local",
		Model:    "default",
		Identity: "You are a coding agent.",
		Tools:    registry.Schemas(),
	}

	a, err := agent.New(agent.Config{
		Assembly: cfg,
		Provider: provider,
		Tools:    registry,
		Guard:    denyGuard{reason: "Permission denied: echo was not run because the user declined it."},
		MaxSteps: 5,
	}, log)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	turn, err := a.Run(context.Background(), "please echo")
	if err != nil {
		t.Fatalf("a denied tool must not fail the turn: %v", err)
	}
	if turn.Text != "Understood, I cannot run that." {
		t.Fatalf("text = %q", turn.Text)
	}

	// The refusal must be in history, flagged as an error, and must say why.
	var refusal string
	var flagged bool
	for _, m := range log.DeriveMessages() {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult && b.IsError {
				flagged = true
				refusal = b.Text
			}
		}
	}

	if !flagged {
		t.Fatal("the refusal must be recorded as an error result")
	}
	if !strings.Contains(refusal, "Permission denied") {
		t.Fatalf("the model must be told why: %q", refusal)
	}

	// And the tool must not have run: the echo tool returns "echo: hello".
	if strings.Contains(refusal, "echo: hello") {
		t.Fatal("the tool ran despite being denied")
	}
}

// passThroughGuard permits everything, as the default must for headless work.
type passThroughGuard struct{}

func (passThroughGuard) Permit(context.Context, string, json.RawMessage, bool, audit.Transcript) (string, error) {
	return "", nil
}

// TestPermittingGuardRunsTheTool is the counter-case, so the denial test cannot
// pass by the tool simply never working.
func TestPermittingGuardRunsTheTool(t *testing.T) {
	provider := &scripted{responses: []llm.Response{
		toolCallTurn("call-1", "echo", `{"text":"hello"}`),
		textTurn("done"),
	}}

	log := session.New(func() int64 { return 1_700_000_000_000 })

	registry := tools.NewRegistry()
	if err := registry.Register(echo{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	a, err := agent.New(agent.Config{
		Assembly: assemble.Config{Provider: "local", Model: "default", Identity: "x", Tools: registry.Schemas()},
		Provider: provider,
		Tools:    registry,
		Guard:    passThroughGuard{},
		MaxSteps: 5,
	}, log)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	if _, err := a.Run(context.Background(), "please echo"); err != nil {
		t.Fatalf("run: %v", err)
	}

	var output string
	for _, m := range log.DeriveMessages() {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult {
				output = b.Text
			}
		}
	}
	if !strings.Contains(output, "echo: hello") {
		t.Fatalf("the permitted tool should have run, got %q", output)
	}
}

// blocking hangs until its context is cancelled, standing in for a slow model.
type blocking struct{ started chan struct{} }

func (b *blocking) Complete(ctx context.Context, _ wire.Request) (llm.Response, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}

	<-ctx.Done()

	return llm.Response{}, ctx.Err()
}

// TestCancellationStopsTheTurnAndSaysSo is the interruption contract: a turn the
// user stopped must be reported as stopped, not as a fault, or the shell will
// print an error for something the user asked for.
func TestCancellationStopsTheTurnAndSaysSo(t *testing.T) {
	provider := &blocking{started: make(chan struct{}, 1)}
	a := harness(t, provider, echo{}, 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-provider.started
		cancel()
	}()

	turn, err := a.Run(ctx, "do something slow")
	if err == nil {
		t.Fatal("a cancelled turn must return an error so the caller can tell")
	}
	if !turn.Cancelled {
		t.Fatal("the turn must be marked cancelled, so a shell can say interrupted rather than failed")
	}
}

// cancelsAfterAnswering cancels the turn's context from inside the first model
// call, after returning a tool call. That makes the per-step cancellation check
// deterministic: the tool runs, the loop comes back around, and the check at the
// top of the next step is what must catch it.
type cancelsAfterAnswering struct {
	cancel context.CancelFunc
	calls  int
}

func (p *cancelsAfterAnswering) Complete(_ context.Context, _ wire.Request) (llm.Response, error) {
	p.calls++
	if p.calls == 1 {
		defer p.cancel()
		return toolCallTurn("c1", "echo", `{"text":"a"}`), nil
	}

	// Reached only if the loop ignored the cancellation.
	return textTurn("should not have been called"), nil
}

// TestCancellationIsCheckedPerStep: an interrupt between steps must stop the
// loop rather than starting another model call.
func TestCancellationIsCheckedPerStep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := &cancelsAfterAnswering{cancel: cancel}
	a := harness(t, provider, echo{}, 10)

	turn, err := a.Run(ctx, "loop")
	if err == nil {
		t.Fatal("expected the cancelled turn to report an error")
	}
	if !turn.Cancelled {
		t.Fatal("the turn must be marked cancelled")
	}
	if provider.calls != 1 {
		t.Fatalf("the loop started %d model calls after cancellation, want 1", provider.calls)
	}
}
