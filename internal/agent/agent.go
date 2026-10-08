// Package agent drives a turn to completion: assemble a request, send it, run
// any tools the model asked for, and repeat until the model stops asking.
//
// Two properties carried from the rest of the harness shape the design:
//
//   - A step's request is assembled ONCE and reused for every retry. Rebuilding
//     it between attempts would risk a byte differing, and a retry that differs
//     by one byte pays a full cache miss on a prompt that was already warm.
//   - Nothing reaches the model that is not in the log first. A tool result is
//     appended as a durable event before the next request derives from it, so
//     the request stays a pure function of the session.
//
// KV Cache effect: prefix reuse requires earlier messages and declarations to
// stay byte-identical under the same provider and model route. Ordinary step
// growth is append-only and preserves reuse.
package agent

import (
	"context"

	"censi/harness/internal/audit"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"censi/harness/internal/assemble"
	"censi/harness/internal/llm"
	"censi/harness/internal/meter"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
	"censi/harness/internal/wire"
)

// Defaults chosen to bound a runaway loop without truncating legitimate work.
const (
	DefaultMaxSteps   = 25
	DefaultMaxRetries = 2
	DefaultRetryDelay = 500 * time.Millisecond
)

// Guard decides whether a tool call may run.
//
// It takes primitives rather than a struct so that the permission implementation
// does not have to import this package, and so a caller can supply a trivial
// closure instead of a type.
type Guard interface {
	// Permit returns an empty reason to allow the call. A non-empty reason
	// denies it, and the reason is handed to the model as the tool result - the
	// model needs to know why, so it can plan around the refusal.
	Permit(ctx context.Context, tool string, args json.RawMessage, readOnly bool, transcript audit.Transcript) (denyReason string, err error)
}

// Config assembles an Agent.
type Config struct {
	// Assembly is the session-stable half of every request.
	Assembly assemble.Config

	Provider llm.Provider
	Tools    *tools.Registry

	// Guard is consulted before every tool call. A nil guard permits
	// everything, which is the right default for tests and headless batch work
	// and the wrong one for anything a person is watching.
	Guard Guard

	// OnDelta receives model text as it arrives, when the provider can stream.
	// It is nil for headless runs, where there is nobody to show it to.
	OnDelta llm.DeltaFunc

	// OnStep is called when a step begins, so a caller can show progress during
	// the long silent stretches between model calls.
	OnStep func(step int)

	// OnToolResult is called after each tool runs, with the full result.
	//
	// It exists so that material meant for a person - a file diff, most of all -
	// can be shown without being put in the model's context. The model already
	// knows what it just did.
	//
	// The whole call is passed, not only the name, because the arguments are what
	// a person needs to see: "bash" says nothing, the command says everything.
	OnToolResult func(call session.Block, result tools.Result)

	// OnUsage is called after each model call with what it actually cost.
	//
	// It exists because some figures are only knowable at the end: reasoning
	// tokens in particular cannot be estimated locally, so the accurate number
	// arrives here rather than through the delta stream.
	OnUsage func(usage llm.Usage)

	// OnSteer is called after a mid-turn direction has been appended to the
	// durable log and before the next model request is assembled.
	OnSteer func(text string)

	// WebSearchUses bounds searches for the whole turn.
	//
	// The provider's caps are per request; this is what makes them per turn.
	// Zero disables searching.
	WebSearchUses int

	// MaxSteps bounds tool-calling steps within a single turn. A model that
	// loops forever is a real failure mode, and the bound is what converts it
	// from a hung session into a reported error.
	MaxSteps int

	// MaxRetries is how many ADDITIONAL attempts follow a failed one. Zero means
	// a single attempt.
	MaxRetries int

	// RetryDelay is the base backoff, doubled per attempt.
	RetryDelay time.Duration
}

// Agent owns one session's turn loop.
type Agent struct {
	cfg      Config
	log      *session.Log
	meter    *meter.Meter
	sequence int
}

// New returns an agent over a log.
func New(cfg Config, log *session.Log) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("agent: a provider is required")
	}
	if cfg.Tools == nil {
		return nil, fmt.Errorf("agent: a tool registry is required")
	}
	if log == nil {
		return nil, fmt.Errorf("agent: a session log is required")
	}

	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultMaxSteps
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = DefaultRetryDelay
	}

	return &Agent{cfg: cfg, log: log, meter: &meter.Meter{}}, nil
}

// SetHooks installs the streaming and progress callbacks.
//
// They are separate from Config because the shell is built after the agent -
// the agent's callbacks write to the shell's status line - so one of the two has
// to be completed after both exist.
func (a *Agent) SetHooks(onDelta llm.DeltaFunc, onStep func(step int)) {
	a.cfg.OnDelta = onDelta
	a.cfg.OnStep = onStep
}

// SetToolResultHook installs the per-tool-result callback.
func (a *Agent) SetToolResultHook(onResult func(call session.Block, result tools.Result)) {
	a.cfg.OnToolResult = onResult
}

// SetUsageHook installs the per-call usage callback.
func (a *Agent) SetUsageHook(onUsage func(llm.Usage)) {
	a.cfg.OnUsage = onUsage
}

// SetSteerHook installs the safe-boundary direction callback.
func (a *Agent) SetSteerHook(onSteer func(string)) {
	a.cfg.OnSteer = onSteer
}

// SetAssembly replaces the session-stable request configuration.
//
// Plan mode needs this: entering or leaving it changes a prompt section, and the
// change must take effect on the next request without discarding the session.
// The caller is responsible for not doing this while a turn is running.
func (a *Agent) SetAssembly(cfg assemble.Config) {
	a.cfg.Assembly = cfg
}

// SetWebSearch changes the portable Gateway search mode and its per-turn
// budget without rebuilding the agent.
func (a *Agent) SetWebSearch(mode string, uses int) {
	a.cfg.Assembly.WebSearch = mode
	a.cfg.WebSearchUses = uses
}

// Assembly returns the current request configuration.
func (a *Agent) Assembly() assemble.Config {
	return a.cfg.Assembly
}

// Log exposes the session log.
func (a *Agent) Log() *session.Log { return a.log }

// Meter exposes the session's usage accounting.
func (a *Agent) Meter() *meter.Meter { return a.meter }

// TurnResult summarises a completed turn.
type TurnResult struct {
	// Steps is how many model requests the turn took.
	Steps int

	// ToolCalls is how many tool invocations ran.
	ToolCalls int

	// Text is the model's final text, which is what the caller shows.
	Text string

	// Usage is the whole turn's cumulative accounting.
	Usage llm.Usage

	// Series is the request-series identifier this turn ran under.
	Series string

	// WebSearches is how many searches the turn ran, across every step.
	WebSearches int

	// Warnings are things worth telling the reader that are not failures.
	//
	// A model that emits a tool call as text produces a turn that succeeded and
	// reads as gibberish, which is the worst of both: nothing is broken enough to
	// report itself, and the output makes no sense.
	Warnings []string

	// Cancelled reports that the user interrupted the turn, so the caller can
	// distinguish an interruption from a fault.
	Cancelled bool
}

// ErrStepLimit is returned when a turn exceeds MaxSteps.
var ErrStepLimit = errors.New("agent: the turn exceeded its step limit")

// Run drives one turn: the user message, then steps until the model stops
// calling tools.
func (a *Agent) Run(ctx context.Context, userText string) (TurnResult, error) {
	return a.run(ctx, userText, nil)
}

// RunSteerable drives a turn that can accept new user direction between model
// and tool operations. A direction never interrupts an operation already in
// flight; it is appended before the next request is assembled.
func (a *Agent) RunSteerable(ctx context.Context, userText string, steering *SteeringQueue) (TurnResult, error) {
	return a.run(ctx, userText, steering)
}

func (a *Agent) run(ctx context.Context, userText string, steering *SteeringQueue) (TurnResult, error) {
	a.sequence++
	if steering != nil {
		defer steering.Close()
	}

	// The system head is reserved BEFORE the user message is admitted.
	//
	// Order matters and is not cosmetic: the first surface node is the system
	// prompt, and node 0 is the one position a replacement may never shadow. If a
	// user message landed there first, the prompt would be pushed to node 1 and
	// lose that structural protection - so the head is claimed even for an empty
	// rendering, which then costs no wire bytes.
	if err := assemble.SyncSystem(a.log, assemble.RenderSystem(a.cfg.Assembly)); err != nil {
		return TurnResult{}, err
	}

	if strings.TrimSpace(userText) != "" {
		if _, err := a.log.Append(session.EventUserMessage, session.Message{
			Role:    session.RoleUser,
			Content: []session.Block{session.Text(userText)},
			Source:  "user",
		}, session.AppendOp()); err != nil {
			return TurnResult{}, fmt.Errorf("agent: recording the user message: %w", err)
		}
	}

	turn := TurnResult{Series: a.seriesID()}

	// The search budget for THIS turn, not for each request.
	//
	// A provider's caps apply per request and a turn is many requests, so
	// without this "ten searches" would mean ten searches on every step - which
	// on a long turn is not a budget at all.
	searchesLeft := a.cfg.WebSearchUses

	for step := 0; step < a.cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			// A cancelled turn is not a failure of the harness: the caller
			// asked for it. Return what was produced rather than an error the
			// shell would print as a fault.
			turn.Cancelled = true
			return turn, ctx.Err()
		}

		if a.cfg.OnStep != nil {
			a.cfg.OnStep(step + 1)
		}

		// Assembled once. Every retry below re-sends this exact value.
		assembly := a.cfg.Assembly
		assembly.WebSearchUses = searchesLeft

		request, err := assemble.Assemble(a.log, assembly)
		if err != nil {
			return turn, err
		}

		response, err := a.send(ctx, request)

		if err == nil {
			// What the endpoint actually ran, not what it was allowed. A request
			// that searched twice has spent two of the turn's budget.
			spent := response.Usage.WebSearches
			if spent > 0 {
				searchesLeft -= spent

				if searchesLeft < 0 {
					searchesLeft = 0
				}

				turn.WebSearches += spent
			}
		}

		if err == nil && a.cfg.OnUsage != nil {
			a.cfg.OnUsage(response.Usage)
		}
		if err != nil {
			// A cancelled send is not a fault: the caller asked for it, and the
			// flag is what lets a shell say "interrupted" instead of printing an
			// error for something the user did on purpose.
			if ctx.Err() != nil {
				turn.Cancelled = true
			}
			return turn, err
		}

		turn.Steps++
		turn.Usage = turn.Usage.Add(response.Usage)
		a.meter.Record(a.sequence, step+1, response.Usage)

		calls := response.Message.ToolCalls()
		if len(calls) == 0 {
			// The response is durable before steering is checked. If direction
			// arrived while the model was answering, this response is the end of
			// that operation rather than the end of the whole turn.
			if _, err := a.log.Append(session.EventAssistantMessage, response.Message, session.AppendOp()); err != nil {
				return turn, fmt.Errorf("agent: recording the assistant message: %w", err)
			}

			turn.Text = textOf(response.Message)

			if LooksLikeLeakedToolCall(turn.Text) {
				turn.Warnings = append(turn.Warnings, "the model wrote out a tool call as text instead of calling a tool, so nothing was run")
			}

			steered, err := a.applySteering(steering)
			if err != nil {
				return turn, err
			}
			if steered {
				turn.Text = ""
				continue
			}

			if steering == nil || steering.CloseIfEmpty() {
				return turn, nil
			}

			// A direction raced with the final boundary. CloseIfEmpty kept the
			// queue open, so apply it rather than returning and losing it.
			steered, err = a.applySteering(steering)
			if err != nil {
				return turn, err
			}
			if steered {
				turn.Text = ""
				continue
			}

			return turn, nil
		}

		// The assistant turn that requested the tools is logged before the
		// results, because the provider pairs a result with the call it answers
		// and the call must already exist in history.
		if _, err := a.log.Append(session.EventAssistantMessage, response.Message, session.AppendOp()); err != nil {
			return turn, fmt.Errorf("agent: recording the tool-calling turn: %w", err)
		}

		for _, call := range calls {
			result, execErr := a.executeTool(ctx, call)
			if execErr == nil && a.cfg.OnToolResult != nil {
				a.cfg.OnToolResult(call, result)
			}
			if execErr != nil {
				if ctx.Err() != nil {
					turn.Cancelled = true
				}
				return turn, execErr
			}
			turn.ToolCalls++

			if _, err := a.log.Append(session.EventToolResult, session.Message{
				Role:    session.RoleTool,
				Content: []session.Block{session.ToolResult(call.CallID, result.IsError, resultText(result))},
				Source:  "tool:" + call.Name,
			}, session.AppendOp()); err != nil {
				return turn, fmt.Errorf("agent: recording a tool result: %w", err)
			}
		}

		if _, err := a.applySteering(steering); err != nil {
			return turn, err
		}
	}

	return turn, ErrStepLimit
}

// applySteering appends every pending direction in arrival order. It is called
// only at protocol-safe boundaries: after a complete model response, or after
// every tool result required by that response has been recorded.
func (a *Agent) applySteering(steering *SteeringQueue) (bool, error) {
	if steering == nil {
		return false, nil
	}

	applied := false
	for _, text := range steering.Drain() {
		if _, err := a.log.Append(session.EventUserMessage, session.Message{
			Role:    session.RoleUser,
			Content: []session.Block{session.Text(text)},
			Source:  "user",
		}, session.AppendOp()); err != nil {
			return applied, fmt.Errorf("agent: recording steering direction: %w", err)
		}

		applied = true
		if a.cfg.OnSteer != nil {
			a.cfg.OnSteer(text)
		}
	}

	return applied, nil
}

// send attempts the request, retrying the SAME frozen value.
//
// The request is not rebuilt between attempts. Rebuilding would mean re-deriving
// from the log, and any difference at all - a re-rendered prompt, a re-read
// file, a re-sampled clock - would invalidate the prefix the first attempt had
// already warmed at the provider.
func (a *Agent) send(ctx context.Context, request wire.Request) (llm.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= a.cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return llm.Response{}, err
		}

		response, err := a.complete(ctx, request)
		if err == nil {
			return response, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			return llm.Response{}, ctx.Err()
		}

		if attempt < a.cfg.MaxRetries {
			delay := a.cfg.RetryDelay * time.Duration(1<<attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			}
		}
	}

	return llm.Response{}, fmt.Errorf("agent: request failed after %d attempt(s): %w", a.cfg.MaxRetries+1, lastErr)
}

// complete performs one attempt, streaming when the provider supports it.
//
// Streaming matters only for latency: the request bytes are identical either
// way, so the cached prefix is unaffected. Nothing about the cache strategy
// depends on this path.
func (a *Agent) complete(ctx context.Context, request wire.Request) (llm.Response, error) {
	if sink := a.cfg.OnDelta; sink != nil {
		if streamer, ok := a.cfg.Provider.(llm.Streamer); ok {
			return streamer.Stream(ctx, request, sink)
		}
	}

	return a.cfg.Provider.Complete(ctx, request)
}

// executeTool runs one tool call.
//
// A tool that reports failure returns an error RESULT rather than a Go error:
// the model is the one that needs to see it, and failing the turn would hide
// information the model could have acted on. Only a harness-level fault - an
// unknown tool, a cancelled context - becomes a Go error.
func (a *Agent) executeTool(ctx context.Context, call session.Block) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}

	if a.cfg.Guard != nil {
		readOnly := false
		if tool, ok := a.cfg.Tools.Get(call.Name); ok {
			readOnly = tool.Definition().ReadOnly
		}

		// The transcript is handed over rather than a summary of it. The auditor
		// decides which turns count, so nothing here can hand it a flattering
		// version of what the user asked for.
		denied, err := a.cfg.Guard.Permit(ctx, call.Name, call.Arguments, readOnly, a.transcript())
		if err != nil {
			return tools.Result{}, err
		}
		if denied != "" {
			// A refusal is an ERR RESULT, not a failed turn: the model is the
			// one that needs to know, and killing the turn would throw away the
			// context it needs to plan around the restriction.
			return tools.Error(denied), nil
		}
	}

	return a.cfg.Tools.Execute(ctx, call.Name, call.Arguments)
}

// seriesID returns the current request series.
//
// A series is a run of requests whose envelope is unchanged, so the provider can
// reuse one cached prefix across it. The identifier is derived from the
// content generation rather than maintained by hand: a replacement changes the
// generation, and the series changes with it, without any caller having to
// remember to say so.
func (a *Agent) seriesID() string {
	return fmt.Sprintf("series-%d", a.log.ContentGeneration())
}

// textOf concatenates a message's text blocks.
func textOf(m session.Message) string {
	parts := make([]string, 0, len(m.Content))
	for _, b := range m.Content {
		if b.Type == session.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

// resultText concatenates a tool result's text blocks.
func resultText(r tools.Result) string {
	parts := make([]string, 0, len(r.Content))
	for _, b := range r.Content {
		if b.Type == session.BlockText {
			parts = append(parts, b.Text)
		}
	}

	if len(parts) == 0 {
		return "(no output)"
	}

	return strings.Join(parts, "\n")
}
