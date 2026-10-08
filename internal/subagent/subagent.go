// Package subagent runs a delegated task in its own context and returns only the
// result.
//
// KV Cache effect: **authoritative, and the entire reason this package looks the
// way it does.** A subagent that shares the parent's S0-S2 prefix costs almost
// nothing to start, because the provider has already cached those bytes and the
// child's first request reads them at the cached rate. A subagent that changes
// its identity or tool set discards that prefix and pays a full cold write.
//
// So the child inherits the harness's identity, guidance and tool declarations
// BYTE-IDENTICALLY, and differs from the parent only from S3 onward - the
// instruction snapshot, which lives in history by design, and its own
// transcript. Giving a subagent its own persona would be the obvious thing to
// do and would cost the whole prefix on every delegation.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/llm"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
)

// ToolName is what the parent calls to delegate.
const ToolName = "task"

// DefaultMaxSteps bounds a delegated task.
//
// Smaller than a top-level turn but not by much. A subagent's transcript is
// invisible while it runs, so a tight ceiling turns "still working" into
// "silently stopped"; 40 is enough for a real investigation and still bounds a
// task that was under-specified.
const DefaultMaxSteps = 40

// Config assembles a delegation tool.
type Config struct {
	// Provider performs the child's model calls.
	Provider llm.Provider

	// Registry is the parent's tool set. The child gets the same declarations,
	// which is what keeps S0 identical.
	Registry *tools.Registry

	// Assembly is the parent's request configuration.
	//
	// Inherited for the parts that should not vary - identity, guidance, token
	// cap - and overridden for the model and reasoning effort below.
	Assembly assemble.Config

	// Model and Thinking are this tool's defaults, used when a delegation does
	// not name its own. Empty falls back to the parent's.
	Model    string
	Thinking string

	// Guard gates the child's tool calls. A subagent must not be a way around a
	// permission the user set.
	Guard agent.Guard

	// MaxSteps bounds the child.
	MaxSteps int

	// MaxDepth caps recursion. A subagent that can spawn subagents can spawn
	// them without bound, which is both a cost and a debugging hazard.
	MaxDepth int

	// Depth is this tool's own depth. Zero means it is the top level.
	Depth int

	// OnStep, when set, is called as the child works, so a shell can show that
	// something is happening.
	OnStep func(description string, step int)
}

// Tool delegates a task to a fresh context.
type Tool struct {
	cfg Config
}

// New returns the delegation tool.
func New(cfg Config) (*Tool, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("subagent: a provider is required")
	}
	if cfg.Registry == nil {
		return nil, fmt.Errorf("subagent: a tool registry is required")
	}

	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultMaxSteps
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 1
	}

	return &Tool{cfg: cfg}, nil
}

// SetAssembly replaces the inherited configuration.
//
// It exists because of a construction cycle. The delegation tool must be
// registered before the parent's tool list is complete, but it must inherit that
// COMPLETE list - otherwise its children declare one tool fewer than the parent
// and their S0 differs, which is precisely the cold prefix this package is built
// to avoid. So the caller registers the tool first and completes the wiring
// here.
func (t *Tool) SetAssembly(cfg assemble.Config) {
	t.cfg.Assembly = cfg
}

// Definition describes the tool to the parent model.
func (t *Tool) Definition() tools.Definition {
	return tools.Definition{
		Name: ToolName,
		Description: "Delegate a self-contained task to a separate agent with its own " +
			"context. Use it for work that involves a lot of searching or reading whose " +
			"detail you do not need afterwards: only the final answer comes back, so your " +
			"own context stays small. The subagent cannot ask you questions.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"description": {"type": "string", "description": "A three to five word label for the task."},
				"prompt": {"type": "string", "description": "The complete, self-contained task. The subagent cannot see this conversation, so include everything it needs."},
				"model": {"type": "string", "description": "Model for the subagent, named by the operator. Omit to use yours. Only use a name the operator has given you - you cannot list what is available, and an invented name fails."},
				"thinking": {"type": "string", "enum": ["minimal", "low", "medium", "high", "default"], "description": "Reasoning effort for the subagent. Omit to use yours."}
			},
			"required": ["description", "prompt"]
		}`),
		// A delegation changes nothing itself, but the agent it starts can. It is
		// classified as read-only because the child's own calls are gated
		// individually by the same guard - treating the spawn as a mutation would
		// prompt twice for one action.
		ReadOnly: true,
	}
}

// args is the delegation request.
type args struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`

	// Model and Thinking let a delegation be cheaper than its parent.
	//
	// A subagent does narrow, well-specified work and reports back, which is
	// exactly the shape a smaller model handles. The parent is usually on the
	// larger one because it is the thing holding the conversation.
	//
	// Inheriting both was the old behaviour, justified by keeping the child's S0
	// identical to the parent's so its FIRST request could reuse the parent's
	// cache. That traded the feature away for a one-time saving of a few
	// thousand tokens - and a child required to match its parent in every
	// respect is one that did not need to exist.
	//
	// What the child keeps is the property that actually matters: its own prefix
	// is stable across its own steps, so from its second request onward it is
	// cached. A conversation has a cache; several conversations do not share one.
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

// firstNonEmpty returns the first value that is set.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// Execute runs the delegated task.
func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in args
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.Result{}, fmt.Errorf("subagent: invalid arguments: %w", err)
	}

	if in.Prompt == "" {
		return tools.Error("task requires a prompt describing what the subagent should do"), nil
	}

	if t.cfg.Depth >= t.cfg.MaxDepth {
		return tools.Error(fmt.Sprintf(
			"subagents cannot delegate further (depth limit %d). Do this work directly.",
			t.cfg.MaxDepth)), nil
	}

	log := session.New(nil)

	// The child gets the parent's registry, including the delegation tool, and
	// the depth limit is enforced at call time instead.
	//
	// Removing the tool would be tidier - a child that may not delegate has no
	// use for it - and is now affordable: the child no longer needs an S0
	// identical to its parent's, since its model and effort may already differ.
	// It is left in place because the depth check is what actually enforces the
	// limit, and a tool that returns a clear refusal is easier to debug than a
	// tool the model cannot see and keeps trying to call.
	registry := t.cfg.Registry

	// The child assembles its own request.
	//
	// It inherits the tool list, because a subagent that could not read a file
	// would be useless, but its model and reasoning effort are its own. Its
	// prefix is therefore its own too - stable across its own steps, which is
	// what makes its second request onward cheap.
	assembly := t.cfg.Assembly
	assembly.Model = firstNonEmpty(in.Model, t.cfg.Model, t.cfg.Assembly.Model)
	assembly.ReasoningEffort = firstNonEmpty(in.Thinking, t.cfg.Thinking, t.cfg.Assembly.ReasoningEffort)

	child, err := agent.New(agent.Config{
		Assembly:      assembly,
		Provider:      t.cfg.Provider,
		Tools:         registry,
		Guard:         t.cfg.Guard,
		MaxSteps:      t.cfg.MaxSteps,
		MaxRetries:    2,
		WebSearchUses: assembly.WebSearchUses,
	}, log)
	if err != nil {
		return tools.Result{}, fmt.Errorf("subagent: building the child agent: %w", err)
	}

	description := in.Description
	if description == "" {
		description = "subagent"
	}

	if t.cfg.OnStep != nil {
		child.SetHooks(nil, func(step int) { t.cfg.OnStep(description, step) })
	}

	turn, runErr := child.Run(ctx, in.Prompt)

	text := turn.Text
	if text == "" && runErr != nil {
		// A child that failed before saying anything still has to report
		// something the parent can act on.
		text = fmt.Sprintf("The subagent stopped after %d step(s) without an answer: %v",
			turn.Steps, runErr)
	}

	report := fmt.Sprintf("%s\n\n[subagent: %d step(s), %d tool call(s), cache hit %.0f%%]",
		text, turn.Steps, turn.ToolCalls, child.Meter().SeriesHitRate()*100)

	return tools.Text(report), nil
}

// compile-time proof that the tool plugs into the registry.
var _ tools.Tool = (*Tool)(nil)
