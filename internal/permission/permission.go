// Package permission decides whether a tool call may run.
//
// The design is a small policy machine rather than a security boundary. It
// stops a model from changing a workspace without a person agreeing to it - the
// common accident - and it is written on the assumption that an OS sandbox sits
// underneath for the adversarial case.
//
// Three effects exist, and the distinction between the last two is the point:
//
//	allow  the call runs
//	ask    a person decides, and the answer may be remembered
//	deny   the call does not run, and the model is told why so it can plan around it
//
// KV Cache effect: none directly. Plan mode does change the system prompt, but
// only from order 500 onward - see PromptSection - so the identity and persona
// ahead of it stay cached.
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"censi/harness/internal/audit"
	"sync"
)

// Effect is a policy's answer for one call.
type Effect string

const (
	// Allow runs the call.
	Allow Effect = "allow"

	// Ask defers to a person.
	Ask Effect = "ask"

	// Deny refuses the call.
	Deny Effect = "deny"
)

// Request describes one tool call awaiting a decision.
type Request struct {
	Tool      string
	Arguments json.RawMessage

	// OneTimeOnly means a remembered session approval must not apply to this
	// call and an approval for this call must not be remembered. It is used for
	// destructive or opaque Bash commands whose exact text needs review.
	OneTimeOnly bool
	RiskReason  string

	// ReadOnly is the tool's own classification. It is what makes the default
	// policy useful: reading is safe to do unattended, writing is not.
	ReadOnly bool

	// Transcript is the conversation the auditor reads for itself.
	//
	// It is the record, not a summary. A caller that supplies "the intent" can
	// supply a false one, and the agent is a caller - so the auditor applies its
	// own provenance rule to the turns it is given, and keeps only those a
	// person authored.
	Transcript audit.Transcript
}

// ArgumentsPreview renders the call's arguments for a prompt, bounded so a large
// argument cannot flood a terminal.
//
// It is bounded by rune count rather than bytes: slicing a UTF-8 string at a
// byte offset can cut a character in half and print mojibake, and a path with a
// non-ASCII character in it is not exotic.
func (r Request) ArgumentsPreview() string {
	args := strings.TrimSpace(string(r.Arguments))
	if args == "" || args == "{}" {
		return ""
	}

	args = strings.Join(strings.Fields(args), " ")

	const limit = 160
	if runes := []rune(args); len(runes) > limit {
		args = string(runes[:limit]) + "..."
	}

	return args
}

// Summary renders the call for a prompt.
func (r Request) Summary() string {
	args := r.ArgumentsPreview()
	if args == "" {
		return r.Tool
	}

	return r.Tool + " " + args
}

// Policy maps a request to an effect.
type Policy struct {
	// PlanMode refuses anything that can change the workspace, so a planning
	// turn cannot leave a trace even if the model decides to act.
	PlanMode bool

	// Auto consults the auditor before asking, so recognised read-only work runs
	// unattended and everything else still stops.
	//
	// It is a separate axis from the file sandbox, which decides what a process
	// can do. This decides when a person is asked. Keeping them apart means
	// "run this unattended" never quietly becomes "this can do anything" - the
	// worst an over-eager rule here can produce is a question that did not need
	// asking.
	Auto bool
}

// DefaultPolicy returns the policy a person-watched session starts with:
// read-only tools run unattended, everything else asks.
func DefaultPolicy() Policy { return Policy{} }

// Effect decides one call.
func (p Policy) Effect(req Request) Effect {
	effect, _ := p.Decide(req)

	return effect
}

// Decide decides one call and says why.
//
// The reason travels with the effect so a person can see the judgement rather
// than only its outcome. An approval nobody can explain is one nobody can
// correct.
func (p Policy) Decide(req Request) (Effect, string) {
	// Reading is always safe to run unattended, in every mode. Plan mode is
	// about mutation, not about paralysis.
	if req.ReadOnly {
		return Allow, "reads only"
	}

	if p.PlanMode {
		return Deny, "plan mode refuses anything that changes the workspace"
	}

	if p.Auto {
		// The auditor sees the call and nothing else. It cannot be told that
		// something was already agreed, which is the whole reason it is not the
		// model: a context that has been talked into something talks its
		// auditor into it too.
		decision := audit.Assess(req.Tool, req.Arguments, req.Transcript)

		if decision.Allowed() {
			return Allow, "audited: " + decision.Explain()
		}

		// A refusal is not a question. Offering it as one gives a person a
		// choice whose only safe answer is no, and a tired reader eventually
		// says yes - so the refusal is the control, not the prompt.
		if decision.RefusedNow() {
			return Deny, decision.Explain()
		}

		// A risky verdict is not a refusal. It means a person decides, which is
		// what happens without auto mode at all.
		return Ask, "needs review: " + decision.Explain()
	}

	return Ask, ""
}

// Prompter asks a person to decide. It is an interface so the shell can supply
// one and tests can supply another.
type Prompter interface {
	// Ask returns the decision and whether it should be remembered for the rest
	// of the session.
	Ask(ctx context.Context, req Request) (approved bool, always bool, err error)
}

// Outcome records what happened to one call, for reporting and tests.
type Outcome struct {
	Effect  Effect
	Reason  string
	Asked   bool
	Granted bool
}

// Guard implements agent.Guard.
//
// It remembers decisions per tool name for the life of the session: a person who
// has approved `bash` once should not be asked again on every step. That memory
// is deliberately per tool rather than global - approving one mutating tool must
// not silently approve the rest.
type Guard struct {
	mu       sync.Mutex
	policy   Policy
	prompter Prompter

	always map[string]bool
	never  map[string]bool

	// log records every decision, so a caller can show what was asked and
	// answer for what.
	log []Outcome
}

// New returns a guard.
func New(policy Policy, prompter Prompter) *Guard {
	return &Guard{
		policy:   policy,
		prompter: prompter,
		always:   map[string]bool{},
		never:    map[string]bool{},
	}
}

// SetPrompter attaches the channel that answers Ask.
//
// It is separate from New because the shell needs the guard to build itself -
// the guard asks through the shell - so one of the two has to be completed
// after both exist.
func (g *Guard) SetPrompter(prompter Prompter) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.prompter = prompter
}

// SetPolicy replaces the policy, which is how plan mode is entered and left.
func (g *Guard) SetPolicy(policy Policy) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.policy = policy
}

// Policy returns the current policy.
func (g *Guard) Policy() Policy {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.policy
}

// Permit implements agent.Guard.
//
// A non-empty return value is a denial reason that reaches the model.
func (g *Guard) Permit(ctx context.Context, tool string, args json.RawMessage, readOnly bool, transcript audit.Transcript) (string, error) {
	oneTimeOnly, riskReason := audit.RequiresFreshApproval(tool, args)
	approvalScope := audit.ApprovalScope(tool, args)
	req := Request{
		Tool: tool, Arguments: args, ReadOnly: readOnly, Transcript: transcript,
		OneTimeOnly: oneTimeOnly, RiskReason: riskReason,
	}

	g.mu.Lock()
	policy := g.policy
	rememberedAllow := g.always[approvalScope]
	rememberedDeny := g.never[approvalScope]
	g.mu.Unlock()

	// A remembered decision short-circuits the policy, except that plan mode
	// still wins: a session approval given in normal mode must not let a write
	// through while the user believes they are planning.
	if !policy.PlanMode {
		if rememberedAllow && !req.OneTimeOnly {
			g.record(Outcome{Effect: Allow, Reason: "approved earlier in this session"})
			return "", nil
		}
		if rememberedDeny {
			g.record(Outcome{Effect: Deny, Reason: "declined earlier in this session"})
			return denial(tool, "you declined it earlier in this session"), nil
		}
	}

	effect, reason := policy.Decide(req)

	switch effect {
	case Allow:
		// The reason is recorded, not discarded. "Allowed" with no explanation
		// is indistinguishable from a bug in the policy.
		g.record(Outcome{Effect: Allow, Reason: reason})
		return "", nil

	case Deny:
		if reason == "" {
			reason = "policy refused this tool call"
		}
		g.record(Outcome{Effect: Deny, Reason: reason})
		return denial(tool, reason), nil
	}

	if g.prompter == nil {
		// Nothing can answer, so the safe answer is no. Failing open here would
		// make the whole layer decorative in exactly the setting - an
		// unattended run - where it matters most.
		reason := "no approval channel is available"
		g.record(Outcome{Effect: Deny, Reason: reason})
		return denial(tool, reason), nil
	}

	approved, always, err := g.prompter.Ask(ctx, req)
	if err != nil {
		return "", err
	}

	g.mu.Lock()
	if always && !req.OneTimeOnly {
		if approved {
			g.always[approvalScope] = true
		} else {
			g.never[approvalScope] = true
		}
	}
	g.mu.Unlock()

	g.record(Outcome{Effect: Ask, Asked: true, Granted: approved})

	if approved {
		return "", nil
	}

	return denial(tool, "the user declined this call"), nil
}

// Decisions returns a copy of every decision made, in order.
func (g *Guard) Decisions() []Outcome {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]Outcome(nil), g.log...)
}

// ApprovedTools returns the tool names approved for the session.
func (g *Guard) ApprovedTools() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	names := make([]string, 0, len(g.always))
	seen := map[string]bool{}
	for name := range g.always {
		if before, _, found := strings.Cut(name, ":"); found {
			name = before
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

func (g *Guard) record(o Outcome) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.log = append(g.log, o)
}

// denial renders a refusal for the model.
//
// It is phrased as information rather than as a failure, because the model's
// next move should be to plan around the restriction, not to retry the call.
func denial(tool, reason string) string {
	return fmt.Sprintf(
		"Permission denied: %s was not run because %s. "+
			"Do not retry it; continue with what you can do, or explain what you need.",
		tool, reason)
}
