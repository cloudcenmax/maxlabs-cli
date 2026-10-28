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

	// ApprovalScope names what "always" would cover. It is carried to the
	// interface so a person can see the reach of the decision they are making.
	ApprovalScope string
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
	// unattended; other Bash commands can receive an isolated model review.
	//
	// It is a separate axis from the file sandbox, which decides what a process
	// can do. This decides when a person is asked. Keeping them apart means
	// "run this unattended" never quietly becomes "this can do anything" - the
	// worst an over-eager rule here can produce is a question that did not need
	// asking.
	Auto bool

	// Workspace is the folder the turn is allowed to change. Auto mode uses it
	// to distinguish an ordinary project edit from a write somewhere else.
	Workspace string

	// CommandReviewer is an isolated, tool-free second opinion. It is consulted
	// only when the deterministic auto policy cannot establish a call's safety -
	// destructive commands, opaque shell, or commands outside the workspace.
	CommandReviewer func(context.Context, json.RawMessage, audit.Transcript) audit.ReviewResult

	// ProjectCommand evaluates literal commands against the assigned workspace.
	// It is supplied by the host so setup, delegates and workers share scope.
	ProjectCommand func(command string) bool
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
	effect, reason, _, _ := p.decide(req)

	return effect, reason
}

// decide is Decide with the extra facts the guard needs: whether an isolated
// review should run before a person is troubled, and whether a failed review
// must end as a refusal rather than a question. That review is the second
// opinion for a call the deterministic policy could not establish, never a
// substitute for the safety floor.
func (p Policy) decide(req Request) (Effect, string, bool, bool) {
	// Reading is always safe to run unattended, in every mode. Plan mode is
	// about mutation, not about paralysis.
	if req.ReadOnly {
		return Allow, "reads only", false, false
	}

	if p.PlanMode {
		return Deny, "plan mode refuses anything that changes the workspace", false, false
	}

	// Without auto mode this is the person-watched default: mutation asks.
	if !p.Auto {
		return Ask, "", false, false
	}

	verdict := audit.AssessAuto(req.Tool, req.Arguments, req.Transcript, p.Workspace)

	if verdict.RefusedNow() {
		// A credential bound for something that can reach the network is
		// refused unless an isolated review can establish that the person
		// explicitly asked for this exact action. Otherwise there is no safe
		// question to ask.
		if req.Tool == "bash" && p.CommandReviewer != nil {
			// The review may establish that the person explicitly asked for this
			// exact destination. If it cannot, the answer is refusal: asking
			// "may I send your key?" is a question whose only safe answer is no.
			return Ask, verdict.Explain(), true, true
		}

		return Deny, verdict.Explain(), false, false
	}

	if verdict.Allowed() {
		return Allow, "auto: " + verdict.Explain(), false, false
	}

	// A project-specific rule can allow a command the general policy leaves
	// uncertain - but it cannot reach past the floor above.
	if req.Tool == "bash" && p.ProjectCommand != nil {
		var input struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(req.Arguments, &input) == nil &&
			strings.TrimSpace(input.Command) != "" &&
			p.ProjectCommand(input.Command) {
			return Allow, "audited project command within the assigned workspace", false, false
		}
	}

	reason := verdict.Explain()
	if reason == "" {
		reason = "auto mode could not establish this call's effects"
	}

	return Ask, reason, verdict.Review, verdict.ReviewDecides
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
		OneTimeOnly: oneTimeOnly, RiskReason: riskReason, ApprovalScope: approvalScope,
	}

	g.mu.Lock()
	policy := g.policy
	rememberedAllow := g.always[approvalScope]
	rememberedDeny := g.never[approvalScope]
	g.mu.Unlock()

	// A remembered decision short-circuits the policy, except that plan mode
	// still wins: a session approval given in normal mode must not let a write
	// through while the user believes they are planning. Auto mode uses the same
	// memory - approving a command family once must mean once, not once per
	// step.
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

	effect, reason, review, hard := policy.decide(req)

	// The isolated reviewer is the second opinion for a call the deterministic
	// policy could not establish. It never overrides the floor on its own: a
	// refusal above has already returned without reaching this block.
	if effect == Ask && review && tool == "bash" && policy.CommandReviewer != nil {
		result := policy.CommandReviewer(ctx, args, transcript)
		if err := ctx.Err(); err != nil {
			return "", err
		}

		if result.Allows() {
			effect = Allow
			reason = "independent review approved: effects and scope match the request"
		} else if hard {
			// A refused class of action (credentials bound for the network) is
			// never turned into a prompt. The refusal is the control.
			effect = Deny
			reason = "independent review declined: " + reviewExplanation(result, reason)
		} else {
			reason = "independent review needs confirmation: " + reviewExplanation(result, reason)
			req.RiskReason = reason
			if result.Decision == "stop" || req.OneTimeOnly {
				req.OneTimeOnly = true
			}

			// A headless caller has no channel to ask on. For a stopped review
			// the turn-ending approval request is still the only way to carry the
			// question, which is what a one-shot CLI consumes.
			if g.prompter == nil && result.Decision == "stop" {
				g.record(Outcome{Effect: Deny, Reason: "Cenmax AI command reviewer stopped for explicit user approval"})
				return "", audit.RequireApproval(args, result.ReviewedWorkspace)
			}
		}
	}

	if effect == Ask && hard {
		// The isolated review settles this class of call. Without one, or when
		// it could not establish the action, the safe answer is refusal - not a
		// question. This is what keeps workspace clean-up from interrupting a
		// person while never running an unjustified deletion.
		if reason == "" {
			reason = "auto mode could not establish that this action was requested"
		}
		effect = Deny
		g.record(Outcome{Effect: Deny, Reason: reason})
		return denial(tool, reason), nil
	}

	if effect == Allow {
		// The reason is recorded, not discarded. "Allowed" with no explanation
		// is indistinguishable from a bug in the policy.
		g.record(Outcome{Effect: Allow, Reason: reason})
		return "", nil
	}

	if effect == Deny {
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

	// Make sure the interface can always explain the question.
	if req.RiskReason == "" {
		req.RiskReason = reason
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

// reviewExplanation prefers the reviewer's own risk description when it has
// one, and otherwise falls back to the deterministic reason.
func reviewExplanation(result audit.ReviewResult, fallback string) string {
	if strings.TrimSpace(result.Risk) != "" && result.Risk != "low" {
		return result.Risk + " risk; " + fallback
	}

	return fallback
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
