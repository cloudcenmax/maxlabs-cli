package tui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/diff"
	"censi/harness/internal/llm"
	"censi/harness/internal/permission"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
	"censi/harness/internal/tui"
	"censi/harness/internal/wire"
)

// scripted answers with a fixed sequence, repeating the last response.
type scripted struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
}

type catalogScripted struct {
	mu       sync.Mutex
	models   []llm.ModelCard
	requests []string
}

func (c *catalogScripted) Complete(_ context.Context, request wire.Request) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.requests = append(c.requests, request.Model)

	return text("ok"), nil
}

func (c *catalogScripted) Models(context.Context) ([]llm.ModelCard, error) {
	return append([]llm.ModelCard(nil), c.models...), nil
}

func (s *scripted) Complete(context.Context, wire.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.calls
	s.calls++

	if i < len(s.responses) {
		return s.responses[i], nil
	}
	if len(s.responses) == 0 {
		return text("ok"), nil
	}

	return s.responses[len(s.responses)-1], nil
}

func text(s string) llm.Response {
	return llm.Response{
		Message: session.Message{Role: session.RoleAssistant, Content: []session.Block{session.Text(s)}},
	}
}

func requestContainsUserText(request wire.Request, want string) bool {
	for _, message := range wire.Messages(request) {
		if message.Role == session.RoleUser && strings.Contains(fmt.Sprint(message.Content), want) {
			return true
		}
	}

	return false
}

func callTool(id, name string) llm.Response {
	return llm.Response{
		Message: session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.ToolCall(id, name, json.RawMessage(`{"path":"x"}`))},
		},
	}
}

// mutating is a tool that changes the workspace, so it must ask.
type mutating struct{ ran *bool }

func (m mutating) Definition() tools.Definition {
	return tools.Definition{
		Name:        "write",
		Description: "Write a file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		ReadOnly:    false,
	}
}

func (m mutating) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	*m.ran = true
	return tools.Text("wrote it"), nil
}

// reader is the tool readers use; read-only, so it never prompts.
type reader struct{ ran *bool }

func (r reader) Definition() tools.Definition {
	return tools.Definition{
		Name:        "read",
		Description: "Read a file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		ReadOnly:    true,
	}
}

func (r reader) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	*r.ran = true
	return tools.Text("file contents"), nil
}

type harness struct {
	shell *tui.Shell
	out   *bytes.Buffer
	wrote bool
	read  bool
	guard *permission.Guard
	agent *agent.Agent
}

func newHarness(t *testing.T, input string, provider llm.Provider) *harness {
	return newHarnessReader(t, strings.NewReader(input), provider)
}

func newHarnessReader(t *testing.T, input io.Reader, provider llm.Provider) *harness {
	t.Helper()

	h := &harness{out: &bytes.Buffer{}}

	registry := tools.NewRegistry()
	if err := registry.Register(mutating{ran: &h.wrote}); err != nil {
		t.Fatalf("register write: %v", err)
	}
	if err := registry.Register(reader{ran: &h.read}); err != nil {
		t.Fatalf("register read: %v", err)
	}

	base := assemble.Config{
		Provider:      "local",
		Model:         "default",
		Identity:      "You are a coding agent.",
		PersonaPrefix: "You are Worker by Example.",
		Tools:         registry.Schemas(),
		MaxTokens:     4096,
	}

	log := session.New(func() int64 { return 1_700_000_000_000 })
	h.guard = permission.New(permission.DefaultPolicy(), nil)

	a, err := agent.New(agent.Config{
		Assembly: base,
		Provider: provider,
		Tools:    registry,
		Guard:    h.guard,
		MaxSteps: 5,
	}, log)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	h.agent = a

	shell, err := tui.New(tui.Config{
		In:        input,
		Out:       h.out,
		Agent:     a,
		Guard:     h.guard,
		Base:      base,
		Workspace: "/tmp/workspace",
		Catalog: func() llm.ModelCatalog {
			catalog, _ := provider.(llm.ModelCatalog)

			return catalog
		}(),
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	h.guard.SetPrompter(shell)
	h.shell = shell

	return h
}

type steeringStream struct {
	mu       sync.Mutex
	calls    int
	requests []wire.Request
	started  chan struct{}
	release  chan struct{}
}

func (s *steeringStream) Complete(ctx context.Context, request wire.Request) (llm.Response, error) {
	return s.Stream(ctx, request, nil)
}

func (s *steeringStream) Stream(ctx context.Context, request wire.Request, onDelta llm.DeltaFunc) (llm.Response, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.requests = append(s.requests, request)
	s.mu.Unlock()

	if onDelta != nil {
		onDelta(llm.Delta{Text: "streaming answer"})
	}

	if call == 1 {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}

	return text("streaming answer"), nil
}

// newHarnessWith builds a shell whose registry holds one chosen tool.
func newHarnessWith(t *testing.T, input string, provider llm.Provider, tool tools.Tool) *harness {
	t.Helper()

	h := &harness{out: &bytes.Buffer{}}

	registry := tools.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	base := assemble.Config{
		Provider: "local", Model: "default", Identity: "You are a coding agent.",
		Tools: registry.Schemas(), MaxTokens: 4096,
	}

	log := session.New(func() int64 { return 1_700_000_000_000 })
	h.guard = permission.New(permission.DefaultPolicy(), nil)

	a, err := agent.New(agent.Config{
		Assembly: base, Provider: provider, Tools: registry, Guard: h.guard, MaxSteps: 5,
	}, log)
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	h.agent = a

	shell, err := tui.New(tui.Config{
		In: strings.NewReader(input), Out: h.out, Agent: a, Guard: h.guard,
		Base: base, Workspace: "/tmp/workspace",
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	h.guard.SetPrompter(shell)
	h.shell = shell

	return h
}

// TestShellDrivesATurn is the M5 headline in its simplest form: a real
// conversation through the shell.
func TestShellDrivesATurn(t *testing.T) {
	h := newHarness(t, "hello\n/exit\n", &scripted{responses: []llm.Response{text("Hi there.")}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(h.out.String(), "Hi there.") {
		t.Fatalf("the answer is missing from the transcript:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "1 step(s)") {
		t.Fatalf("the step summary is missing:\n%s", h.out.String())
	}
}

// TestPlanModeBlocksAMutatingTool is the M5 integration property: the mode
// reaches the tools gate, and the refusal reaches the model.
func TestPlanModeBlocksAMutatingTool(t *testing.T) {
	h := newHarness(t, "/plan\nwrite a file\n/exit\n", &scripted{responses: []llm.Response{
		callTool("c1", "write"),
		text("I cannot write while planning."),
	}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !h.shell.PlanMode() {
		t.Fatal("/plan did not enter plan mode")
	}
	if h.wrote {
		t.Fatal("plan mode must prevent a mutating tool from running")
	}

	// The model must be told why, or it will retry.
	var refusal string
	for _, m := range h.agent.Log().DeriveMessages() {
		for _, b := range m.Content {
			if b.Type == session.BlockToolResult && b.IsError {
				refusal = b.Text
			}
		}
	}
	if !strings.Contains(refusal, "plan mode") {
		t.Fatalf("the model was not told about plan mode: %q", refusal)
	}
}

// TestPlanModeStillAllowsReading: a planning turn that cannot investigate is
// useless, so read-only tools must pass through.
func TestPlanModeStillAllowsReading(t *testing.T) {
	h := newHarness(t, "/plan\nread the plan\n/exit\n", &scripted{responses: []llm.Response{
		callTool("c1", "read"),
		text("Here is what I found."),
	}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !h.read {
		t.Fatal("plan mode must allow a read-only tool to run unattended")
	}
}

// TestDecliningAnApprovalStopsTheTool covers the human gate outside plan mode.
func TestDecliningAnApprovalStopsTheTool(t *testing.T) {
	h := newHarness(t, "write a file\nn\n/exit\n", &scripted{responses: []llm.Response{
		callTool("c1", "write"),
		text("Understood."),
	}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if h.wrote {
		t.Fatal("a declined tool must not run")
	}
	// The prompt must announce itself as a decision, name the tool, and spell the
	// options out. A terse "[y/n/a]" reads like shell chrome and gets scrolled
	// past, which is what made this confusing in the first place.
	out := h.out.String()
	for _, want := range []string{"approval needed", "write", "allow this once", "always allow", "deny"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the approval prompt is missing %q:\n%s", want, out)
		}
	}
}

// TestApprovingAnApprovalRunsTheTool is the counter-case, so the decline test
// cannot pass by the tool simply never working.
func TestApprovingAnApprovalRunsTheTool(t *testing.T) {
	h := newHarness(t, "write a file\ny\n/exit\n", &scripted{responses: []llm.Response{
		callTool("c1", "write"),
		text("Done."),
	}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !h.wrote {
		t.Fatal("an approved tool must run")
	}
}

// TestAnEmptyAnswerIsARefusal: guessing "yes" from a stray newline is not a
// mistake worth being able to make.
func TestAnEmptyAnswerIsARefusal(t *testing.T) {
	h := newHarness(t, "write a file\n\n/exit\n", &scripted{responses: []llm.Response{
		callTool("c1", "write"),
		text("Understood."),
	}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if h.wrote {
		t.Fatal("an empty answer must refuse, not approve")
	}
}

// TestLeavingPlanModeRestoresThePrompt proves the shell keeps the assembly
// configuration and the gate in step, and that toggling back is clean.
func TestLeavingPlanModeRestoresThePrompt(t *testing.T) {
	before := h0(t)

	h := newHarness(t, "/plan\n/act\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if h.shell.PlanMode() {
		t.Fatal("/act did not leave plan mode")
	}

	after := assemble.RenderSystem(h.agent.Assembly())
	if before != after {
		t.Fatalf("toggling plan mode on and off changed the prompt:\n before: %q\n  after: %q", before, after)
	}
}

// h0 renders the baseline prompt with plan mode off.
func h0(t *testing.T) string {
	t.Helper()

	h := newHarness(t, "", &scripted{})

	return assemble.RenderSystem(h.agent.Assembly())
}

// TestUnknownCommandIsReported keeps a typo from being sent as a prompt.
func TestUnknownCommandIsReported(t *testing.T) {
	h := newHarness(t, "/nonsense\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(h.out.String(), "unknown command") {
		t.Fatalf("a bad command should be reported:\n%s", h.out.String())
	}
}

// TestToolsCommandListsTheRoster covers the introspection command.
func TestToolsCommandListsTheRoster(t *testing.T) {
	h := newHarness(t, "/tools\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()
	if !strings.Contains(out, "read") || !strings.Contains(out, "write") {
		t.Fatalf("the tool roster is incomplete:\n%s", out)
	}
}

// TestEOFEndsTheSessionCleanly: a piped session must not look like a crash.
func TestEOFEndsTheSessionCleanly(t *testing.T) {
	h := newHarness(t, "hello\n", &scripted{responses: []llm.Response{text("hi")}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("EOF should end the session cleanly, got %v", err)
	}
}

// TestApprovalAcceptsTheWordsItShows: the prompt spells the options out, so
// rejecting the spelling it just displayed would be perverse.
func TestApprovalAcceptsTheWordsItShows(t *testing.T) {
	for _, word := range []string{"allow", "yes", "always"} {
		t.Run(word, func(t *testing.T) {
			h := newHarness(t, "write a file\n"+word+"\n/exit\n", &scripted{responses: []llm.Response{
				callTool("c1", "write"),
				text("Done."),
			}})

			if err := h.shell.Run(context.Background()); err != nil {
				t.Fatalf("run: %v", err)
			}
			if !h.wrote {
				t.Fatalf("%q was shown as an option but did not approve", word)
			}
		})
	}

	for _, word := range []string{"deny", "no"} {
		t.Run(word, func(t *testing.T) {
			h := newHarness(t, "write a file\n"+word+"\n/exit\n", &scripted{responses: []llm.Response{
				callTool("c1", "write"),
				text("Understood."),
			}})

			if err := h.shell.Run(context.Background()); err != nil {
				t.Fatalf("run: %v", err)
			}
			if h.wrote {
				t.Fatalf("%q must refuse", word)
			}
		})
	}
}

// TestApprovalPromptStopsTheSpinner is the visible half of the fix: while the
// harness waits on a person, it must not claim to be working.
func TestApprovalPromptStopsTheSpinner(t *testing.T) {
	h := newHarness(t, "write a file\ndeny\n/exit\n", &scripted{responses: []llm.Response{
		callTool("c1", "write"),
		text("Understood."),
	}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if h.out.Len() == 0 {
		t.Fatal("no output at all")
	}

	// The transcript is not a terminal here, so the important assertion is that
	// the prompt was reached and answered - the spinner suppression itself is
	// covered against the status type directly.
	if !strings.Contains(h.out.String(), "approval needed") {
		t.Fatal("the prompt never appeared")
	}
}

// streaming answers through Stream, emitting deltas, and completes without a
// trailing newline - which is exactly the shape that exposed the ordering bug.
type streaming struct {
	text   string
	called bool
}

func (s *streaming) Complete(context.Context, wire.Request) (llm.Response, error) {
	s.called = true

	return text(s.text), nil
}

func (s *streaming) Stream(_ context.Context, _ wire.Request, onDelta llm.DeltaFunc) (llm.Response, error) {
	s.called = true

	// Emit in pieces, as a real stream would.
	for _, chunk := range []string{s.text[:len(s.text)/2], s.text[len(s.text)/2:]} {
		if onDelta != nil {
			onDelta(llm.Delta{Text: chunk})
		}
	}

	return text(s.text), nil
}

// TestStreamedTailComesBeforeTheSummary is the bug where one answer looked like
// two.
//
// The renderer holds an unterminated final line so a construct split across
// deltas is not printed half-formed. The summary used to print before that line
// was released, so the tail of the answer appeared BELOW the step summary.
func TestStreamedTailComesBeforeTheSummary(t *testing.T) {
	provider := &streaming{text: "the answer ends here"}

	h := newHarness(t, "do it\n/exit\n", provider)

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()

	answerAt := strings.Index(out, "the answer ends here")
	summaryAt := strings.Index(out, "step(s)")

	if answerAt < 0 {
		t.Fatalf("the answer is missing:\n%s", out)
	}
	if summaryAt < 0 {
		t.Fatalf("the summary is missing:\n%s", out)
	}
	if answerAt > summaryAt {
		t.Fatalf("the tail of the answer was printed after the summary:\n%s", out)
	}

	// And it must not be split: the whole answer appears before the summary.
	if strings.Count(out, "the answer ends here") != 1 {
		t.Fatalf("the answer appears more than once:\n%s", out)
	}
}

// TestPromptIsShownWhenWaitingForInput: the shell owns the idle prompt, because
// the editor cannot know a turn has ended while it is blocked on a read.
func TestPromptIsShownWhenWaitingForInput(t *testing.T) {
	// Input ends at EOF rather than with /exit. A piped /exit is consumed DURING
	// the turn and replayed from the queue, and the queue path deliberately
	// skips the prompt - so it would test the wrong thing.
	h := newHarness(t, "hello\n", &scripted{responses: []llm.Response{text("hi")}})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()

	if !strings.Contains(out, "> ") {
		t.Fatalf("no prompt was printed:\n%q", out)
	}

	// It appears after the turn's summary, where the user is about to type.
	summaryAt := strings.Index(out, "step(s)")
	promptAt := strings.LastIndex(out, "> ")

	if summaryAt < 0 || promptAt < 0 {
		t.Fatalf("setup: summary=%d prompt=%d", summaryAt, promptAt)
	}
	if promptAt < summaryAt {
		t.Fatalf("the prompt was printed before the turn finished:\n%s", out)
	}
}

// TestMessageSubmittedDuringATurnSteersAtTheNextBoundary covers interactive
// steering. Direction typed during a model operation is applied before the
// agent takes its next step, without corrupting the append-only history.
func TestMessageSubmittedDuringATurnSteersAtTheNextBoundary(t *testing.T) {
	provider := &steeringStream{started: make(chan struct{}), release: make(chan struct{})}
	input, writer := io.Pipe()
	h := newHarnessReader(t, input, provider)
	done := make(chan error, 1)

	go func() {
		done <- h.shell.Run(context.Background())
	}()

	if _, err := fmt.Fprintln(writer, "first request"); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	<-provider.started
	if _, err := fmt.Fprintln(writer, "steer with this"); err != nil {
		t.Fatalf("write follow-up: %v", err)
	}
	close(provider.release)
	if err := writer.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	provider.mu.Lock()
	calls := provider.calls
	requests := append([]wire.Request(nil), provider.requests...)
	provider.mu.Unlock()

	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2 so direction triggers reassessment", calls)
	}
	if !strings.Contains(h.out.String(), "direction received; applying after the current operation") ||
		!strings.Contains(h.out.String(), "direction applied; reassessing") {
		t.Fatal("the user is not told when direction is received and applied")
	}
	if len(requests) != 2 || !requestContainsUserText(requests[1], "steer with this") {
		t.Fatal("the second model request does not contain the steering direction")
	}
}

// TestThinkCommandSetsTheLevel covers the user-facing control.
func TestThinkCommandSetsTheLevel(t *testing.T) {
	h := newHarness(t, "/think low\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.agent.Assembly().ReasoningEffort; got != "low" {
		t.Fatalf("assembly effort = %q, want low", got)
	}
	if !strings.Contains(h.out.String(), "thinking set to low") {
		t.Fatalf("no confirmation was shown:\n%s", h.out.String())
	}
}

// TestThinkDefaultClearsTheSetting: "default" means say nothing and let the
// endpoint decide, which is not the same as sending an empty effort.
func TestThinkDefaultClearsTheSetting(t *testing.T) {
	h := newHarness(t, "/think default\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.agent.Assembly().ReasoningEffort; got != "" {
		t.Fatalf("assembly effort = %q, want empty", got)
	}
}

// TestThinkRejectsAnUnknownLevel keeps a typo from silently doing nothing.
func TestThinkRejectsAnUnknownLevel(t *testing.T) {
	h := newHarness(t, "/think slightly\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()
	if !strings.Contains(out, "unknown thinking level") {
		t.Fatalf("a bad level was accepted silently:\n%s", out)
	}

	// And it must not have been applied.
	if got := h.agent.Assembly().ReasoningEffort; got != "" {
		t.Fatalf("a bad level changed the setting to %q", got)
	}
}

// TestThinkWithNoArgumentReportsTheLevel is the discoverability path.
func TestThinkWithNoArgumentReportsTheLevel(t *testing.T) {
	h := newHarness(t, "/think\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()
	if !strings.Contains(out, "minimal, low, medium, high, default") {
		t.Fatalf("the available levels were not listed:\n%s", out)
	}
}

func TestModelCommandSelectsAnEnabledGatewayModel(t *testing.T) {
	provider := &catalogScripted{models: []llm.ModelCard{
		{ID: "worker", Description: "Fast everyday work"},
		{ID: "pro", Description: "Deeper reasoning"},
	}}
	h := newHarness(t, "/model\n2\nhello\n/exit\n", provider)

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.agent.Assembly().Model; got != "pro" {
		t.Fatalf("assembly model = %q, want pro", got)
	}
	if len(provider.requests) != 1 || provider.requests[0] != "pro" {
		t.Fatalf("request models = %v, want [pro]", provider.requests)
	}

	out := h.out.String()
	for _, expected := range []string{"available models", "worker", "pro", "model switched to pro"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("model picker is missing %q:\n%s", expected, out)
		}
	}
}

func TestModelCommandRejectsAModelOutsideTheGatewayCatalog(t *testing.T) {
	provider := &catalogScripted{models: []llm.ModelCard{{ID: "worker"}, {ID: "pro"}}}
	h := newHarness(t, "/model upstream-secret-name\n/exit\n", provider)

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.agent.Assembly().Model; got != "default" {
		t.Fatalf("an unavailable model changed the assembly to %q", got)
	}
	if !strings.Contains(h.out.String(), "unknown or disabled model") {
		t.Fatalf("the unavailable model was not explained:\n%s", h.out.String())
	}
}

func TestAutoCommandEnablesTheAuditedPolicy(t *testing.T) {
	h := newHarness(t, "/auto\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	policy := h.guard.Policy()
	if !policy.Auto || policy.PlanMode {
		t.Fatalf("policy = %+v, want auto without plan mode", policy)
	}
	if !strings.Contains(h.out.String(), "risky work still asks") {
		t.Fatalf("auto mode's safety boundary was not shown:\n%s", h.out.String())
	}
}

func TestPlanModeReturnsToAutoWhenToggledOff(t *testing.T) {
	h := newHarness(t, "/auto\n/plan\n/toggle\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	policy := h.guard.Policy()
	if !policy.Auto || policy.PlanMode {
		t.Fatalf("policy = %+v, want resumed auto mode", policy)
	}
}

func TestWebCommandChangesTheGatewaySearchMode(t *testing.T) {
	h := newHarness(t, "/web auto\n/exit\n", &scripted{})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := h.agent.Assembly().WebSearch; got != "auto" {
		t.Fatalf("web search = %q, want auto", got)
	}
	if !strings.Contains(h.out.String(), "web search: auto") {
		t.Fatalf("search mode was not reported:\n%s", h.out.String())
	}
}

// TestThinkingLevelDoesNotChangeThePrompt: the setting is a request parameter,
// so changing it mid-session must not invalidate the cached prefix.
func TestThinkingLevelDoesNotChangeThePrompt(t *testing.T) {
	h := newHarness(t, "", &scripted{})

	before := assemble.RenderSystem(h.agent.Assembly())

	h.shell.SetThinking("high")

	after := assemble.RenderSystem(h.agent.Assembly())

	if before != after {
		t.Fatalf("changing the thinking level changed the prompt:\n before: %q\n  after: %q", before, after)
	}
}

// diffing is a tool that edits a file, so the human-facing diff path can be
// exercised end to end.
type diffing struct{ ran *bool }

func (d diffing) Definition() tools.Definition {
	return tools.Definition{
		Name:        "edit",
		Description: "Edit a file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
}

func (d diffing) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	*d.ran = true

	result := tools.Text("Edited src/thing.php.")
	result.DiffTarget = "src/thing.php"
	result.Diff = []diff.Line{
		{Kind: diff.Same, Text: "if (!$book) {"},
		{Kind: diff.Del, Text: "    throw new Exception('no');"},
		{Kind: diff.Add, Text: "    throw new BookMissing($id);"},
		{Kind: diff.Same, Text: "}"},
	}

	return result, nil
}

// TestFileDiffIsShownToThePerson is the point of the addon: a file changes and
// the only evidence should not be the model's own description of it.
func TestFileDiffIsShownToThePerson(t *testing.T) {
	var ran bool

	// "y" answers the approval prompt: edit changes the workspace, so it asks.
	h := newHarnessWith(t, "edit it\ny\n",
		&scripted{responses: []llm.Response{
			llm.Response{Message: session.Message{Role: session.RoleAssistant,
				Content: []session.Block{session.ToolCall("c1", "edit", json.RawMessage(`{"path":"x"}`))}}},
			text("Done."),
		}},
		diffing{ran: &ran})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()

	for _, want := range []string{
		"src/thing.php",
		"throw new Exception('no');",
		"throw new BookMissing($id);",
		"+1", "-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the diff is missing %q:\n%s", want, out)
		}
	}
}

// TestDiffIsNotSentToTheModel: the model already knows what it changed, and a
// diff of its own edit would waste its context.
func TestDiffIsNotSentToTheModel(t *testing.T) {
	var ran bool

	h := newHarnessWith(t, "edit it\ny\n", &scripted{responses: []llm.Response{
		{Message: session.Message{Role: session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c1", "edit", json.RawMessage(`{"path":"x"}`))}}},
		text("Done."),
	}}, diffing{ran: &ran})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, message := range h.agent.Log().DeriveMessages() {
		for _, block := range message.Content {
			if block.Type != session.BlockToolResult {
				continue
			}
			if strings.Contains(block.Text, "BookMissing") {
				t.Fatalf("the diff leaked into the model's context: %q", block.Text)
			}
		}
	}
}

// TestUnchangedWriteShowsNoDiff keeps a no-op from printing an empty heading.
func TestUnchangedWriteShowsNoDiff(t *testing.T) {
	var ran bool

	h := newHarnessWith(t, "edit it\ny\n", &scripted{responses: []llm.Response{
		{Message: session.Message{Role: session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c1", "edit", json.RawMessage(`{"path":"x"}`))}}},
		text("Done."),
	}}, sameFile{ran: &ran})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(h.out.String(), "✎") {
		t.Fatalf("an empty diff printed a heading:\n%s", h.out.String())
	}
}

// sameFile reports a diff that changes nothing.
type sameFile struct{ ran *bool }

func (s sameFile) Definition() tools.Definition {
	return tools.Definition{Name: "edit", Description: "Edit.", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (s sameFile) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	*s.ran = true

	result := tools.Text("Edited nothing.")
	result.DiffTarget = "src/same.php"
	result.Diff = []diff.Line{{Kind: diff.Same, Text: "unchanged"}}

	return result, nil
}

// bigRewrite produces a diff where every line changed, as a whole-file write
// does. There is no unchanged run to elide, so only the cap keeps it short.
type bigRewrite struct{ ran *bool }

func (b bigRewrite) Definition() tools.Definition {
	return tools.Definition{Name: "write", Description: "Write.", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (b bigRewrite) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	*b.ran = true

	result := tools.Text("Wrote src/big.php (4000 bytes).")
	result.DiffTarget = "src/big.php"

	var lines []diff.Line
	for i := 0; i < 200; i++ {
		lines = append(lines, diff.Line{Kind: diff.Del, Text: fmt.Sprintf("old line %d", i)})
		lines = append(lines, diff.Line{Kind: diff.Add, Text: fmt.Sprintf("new line %d", i)})
	}
	result.Diff = lines

	return result, nil
}

// TestWholeFileRewriteIsCapped is the fix for a rewrite printing the entire
// file: when nothing is unchanged there is no run to elide, so the only thing
// that keeps the output to the edited region is a hard cap.
func TestWholeFileRewriteIsCapped(t *testing.T) {
	var ran bool

	h := newHarnessWith(t, "rewrite it\ny\n", &scripted{responses: []llm.Response{
		{Message: session.Message{Role: session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c1", "write", json.RawMessage(`{"path":"x"}`))}}},
		text("Done."),
	}}, bigRewrite{ran: &ran})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()

	// The first few lines are shown.
	if !strings.Contains(out, "old line 0") {
		t.Fatalf("nothing from the diff was shown:\n%s", out)
	}

	// But not lines from deep in the file.
	for _, hidden := range []string{"old line 150", "new line 150", "old line 90"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("the whole file was printed; %q should have been elided", hidden)
		}
	}

	// And what was withheld is stated rather than silently dropped.
	if !strings.Contains(out, "more line(s)") {
		t.Fatalf("the elision was not reported:\n%s", out)
	}
}

// TestSmallEditStillShowsInFull: the cap must not truncate an ordinary edit.
func TestSmallEditStillShowsInFull(t *testing.T) {
	var ran bool

	h := newHarnessWith(t, "edit it\ny\n", &scripted{responses: []llm.Response{
		{Message: session.Message{Role: session.RoleAssistant,
			Content: []session.Block{session.ToolCall("c1", "edit", json.RawMessage(`{"path":"x"}`))}}},
		text("Done."),
	}}, diffing{ran: &ran})

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()
	if strings.Contains(out, "more line(s)") {
		t.Fatalf("a four-line diff was truncated:\n%s", out)
	}
	for _, want := range []string{"throw new Exception('no');", "throw new BookMissing($id);"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

// TestStepLimitIsReportedAsAStopNotAFault: reaching the ceiling is a normal way
// for a long turn to end. Reporting it as an error alarms the user and buries
// the answer already produced.
func TestStepLimitIsReportedAsAStopNotAFault(t *testing.T) {
	provider := &alwaysTools{}

	h := newHarness(t, "keep going\n", provider)

	if err := h.shell.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := h.out.String()

	if strings.Contains(out, "turn ended:") {
		t.Fatalf("the step limit was reported as a fault:\n%s", out)
	}
	if !strings.Contains(out, "stopped at the step limit") {
		t.Fatalf("the stop was not explained:\n%s", out)
	}
	if !strings.Contains(out, "-max-steps") {
		t.Fatalf("the way forward was not given:\n%s", out)
	}
}

// alwaysTools answers every step with another tool call, so the loop runs until
// the ceiling stops it.
type alwaysTools struct{ n int }

func (a *alwaysTools) Complete(_ context.Context, _ wire.Request) (llm.Response, error) {
	a.n++

	return llm.Response{Message: session.Message{
		Role:    session.RoleAssistant,
		Content: []session.Block{session.ToolCall("c"+fmt.Sprint(a.n), "read", json.RawMessage(`{"path":"x"}`))},
	}}, nil
}
