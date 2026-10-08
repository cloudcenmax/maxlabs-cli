package permission_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"censi/harness/internal/permission"
)

// prompter is a scripted approval channel.
type prompter struct {
	approved bool
	always   bool
	err      error
	asked    []permission.Request
}

func (p *prompter) Ask(_ context.Context, req permission.Request) (bool, bool, error) {
	p.asked = append(p.asked, req)
	return p.approved, p.always, p.err
}

func args(s string) json.RawMessage { return json.RawMessage(s) }

// TestReadOnlyRunsUnattended is the property that makes the default policy
// usable: investigating never needs a person, so the common loop is not
// interrupted.
func TestReadOnlyRunsUnattended(t *testing.T) {
	p := &prompter{}
	g := permission.New(permission.DefaultPolicy(), p)

	denied, err := g.Permit(context.Background(), "read", args(`{"path":"a"}`), true, nil)
	if err != nil {
		t.Fatalf("permit: %v", err)
	}
	if denied != "" {
		t.Fatalf("a read-only tool must run unattended, got %q", denied)
	}
	if len(p.asked) != 0 {
		t.Fatal("a read-only tool must not prompt")
	}
}

// TestMutatingAsksBeforeRunning is the other half: the common accident is a
// write nobody agreed to.
func TestMutatingAsksBeforeRunning(t *testing.T) {
	p := &prompter{approved: true}
	g := permission.New(permission.DefaultPolicy(), p)

	denied, err := g.Permit(context.Background(), "write", args(`{"path":"a"}`), false, nil)
	if err != nil {
		t.Fatalf("permit: %v", err)
	}
	if denied != "" {
		t.Fatalf("an approved call must run, got %q", denied)
	}
	if len(p.asked) != 1 {
		t.Fatalf("expected one prompt, got %d", len(p.asked))
	}
}

// TestRefusalReachesTheModelAsAReason: the model has to know why, or it will
// retry the same call forever.
func TestRefusalReachesTheModelAsAReason(t *testing.T) {
	p := &prompter{approved: false}
	g := permission.New(permission.DefaultPolicy(), p)

	denied, err := g.Permit(context.Background(), "bash", args(`{"command":"rm -rf /"}`), false, nil)
	if err != nil {
		t.Fatalf("permit: %v", err)
	}

	if denied == "" {
		t.Fatal("a declined call must be denied")
	}
	if !strings.Contains(denied, "bash") {
		t.Fatalf("the reason should name the tool: %q", denied)
	}
	if !strings.Contains(strings.ToLower(denied), "do not retry") {
		t.Fatalf("the reason should tell the model not to retry: %q", denied)
	}
}

// TestSessionApprovalIsRememberedPerTool covers the ergonomics: approving bash
// once should not mean approving every mutating tool.
func TestSessionApprovalIsRememberedPerTool(t *testing.T) {
	p := &prompter{approved: true, always: true}
	g := permission.New(permission.DefaultPolicy(), p)
	bashArgs := args(`{"command":"php artisan test"}`)

	for i := 0; i < 3; i++ {
		if _, err := g.Permit(context.Background(), "bash", bashArgs, false, nil); err != nil {
			t.Fatalf("permit: %v", err)
		}
	}

	if len(p.asked) != 1 {
		t.Fatalf("expected one prompt for three calls, got %d", len(p.asked))
	}

	// A different mutating tool is still unknown.
	if _, err := g.Permit(context.Background(), "write", args(`{}`), false, nil); err != nil {
		t.Fatalf("permit: %v", err)
	}
	if len(p.asked) != 2 {
		t.Fatal("approving one tool must not approve another")
	}

	// Both were approved "always", so both are remembered - separately. The
	// property under test is that the second call had to ASK, which the prompt
	// count above already proves; remembering is per tool, not global.
	approved := g.ApprovedTools()
	if len(approved) != 2 || approved[0] != "bash" || approved[1] != "write" {
		t.Fatalf("approved tools = %v, want [bash write]", approved)
	}
}

// A session approval for ordinary Bash work must not become a reusable grant
// for a later destructive chain. High-impact commands ask every time, even if
// the approval channel tries to answer "always" again.
func TestBashSessionApprovalDoesNotCoverDestructiveCommands(t *testing.T) {
	p := &prompter{approved: true, always: true}
	g := permission.New(permission.Policy{Auto: true}, p)

	ordinary := args(`{"command":"php artisan test"}`)
	if denied, err := g.Permit(context.Background(), "bash", ordinary, false, nil); err != nil || denied != "" {
		t.Fatalf("ordinary Bash approval failed: denied=%q err=%v", denied, err)
	}
	if denied, err := g.Permit(context.Background(), "bash", ordinary, false, nil); err != nil || denied != "" {
		t.Fatalf("remembered ordinary Bash approval failed: denied=%q err=%v", denied, err)
	}
	if len(p.asked) != 1 {
		t.Fatalf("ordinary Bash approval was not reused; prompts=%d", len(p.asked))
	}

	destructive := args(`{"command":"cd gateway && rm -rf storage/tmp"}`)
	for i := 0; i < 2; i++ {
		if denied, err := g.Permit(context.Background(), "bash", destructive, false, nil); err != nil || denied != "" {
			t.Fatalf("fresh destructive approval failed: denied=%q err=%v", denied, err)
		}
	}

	if len(p.asked) != 3 {
		t.Fatalf("destructive Bash did not ask every time; prompts=%d", len(p.asked))
	}
	for _, request := range p.asked[1:] {
		if !request.OneTimeOnly || request.RiskReason == "" {
			t.Fatalf("destructive request was not marked one-time: %+v", request)
		}
	}
}

// Auto mode may run recognised reads, but destructive and opaque Bash remains
// an explicit human decision.
func TestAutoModeStillAsksForDestructiveBash(t *testing.T) {
	p := &prompter{approved: false}
	g := permission.New(permission.Policy{Auto: true}, p)

	commands := []string{
		`{"command":"cd /tmp && rm -rf build"}`,
		`{"command":"find . -delete"}`,
		`{"command":"git clean -fdx"}`,
		`{"command":"bash -c 'rm -rf build'"}`,
		`{"command":"echo changed > config.php"}`,
	}

	for _, command := range commands {
		denied, err := g.Permit(context.Background(), "bash", args(command), false, nil)
		if err != nil {
			t.Fatalf("permit %s: %v", command, err)
		}
		if denied == "" {
			t.Fatalf("declined destructive command was allowed: %s", command)
		}
	}

	if len(p.asked) != len(commands) {
		t.Fatalf("Auto mode prompted %d times, want %d", len(p.asked), len(commands))
	}
}

// TestPlanModeOverridesARememberedApproval is the subtle one. A session approval
// given in normal mode must not let a write through while the user believes they
// are planning - that would make plan mode quietly unreliable exactly where it
// is trusted.
func TestPlanModeOverridesARememberedApproval(t *testing.T) {
	p := &prompter{approved: true, always: true}
	g := permission.New(permission.DefaultPolicy(), p)
	bashArgs := args(`{"command":"php artisan test"}`)

	if _, err := g.Permit(context.Background(), "bash", bashArgs, false, nil); err != nil {
		t.Fatalf("permit: %v", err)
	}

	g.SetPolicy(permission.Policy{PlanMode: true})

	denied, err := g.Permit(context.Background(), "bash", bashArgs, false, nil)
	if err != nil {
		t.Fatalf("permit: %v", err)
	}
	if denied == "" {
		t.Fatal("plan mode must refuse a mutating tool even after a session approval")
	}
	if !strings.Contains(denied, "plan mode") {
		t.Fatalf("the reason should name plan mode: %q", denied)
	}
}

// TestPlanModeStillAllowsReading: plan mode is about mutation, not paralysis.
// A planning turn that cannot read anything is useless.
func TestPlanModeStillAllowsReading(t *testing.T) {
	g := permission.New(permission.Policy{PlanMode: true}, nil)

	denied, err := g.Permit(context.Background(), "read", args(`{"path":"PLAN.md"}`), true, nil)
	if err != nil {
		t.Fatalf("permit: %v", err)
	}
	if denied != "" {
		t.Fatalf("plan mode must allow read-only tools, got %q", denied)
	}
}

// TestNoPrompterFailsClosed: without a channel to answer, the safe answer is no.
// Failing open here would make the layer decorative in exactly the setting -
// an unattended run - where it matters most.
func TestNoPrompterFailsClosed(t *testing.T) {
	g := permission.New(permission.DefaultPolicy(), nil)

	denied, err := g.Permit(context.Background(), "write", args(`{}`), false, nil)
	if err != nil {
		t.Fatalf("permit: %v", err)
	}
	if denied == "" {
		t.Fatal("a mutating tool with no approval channel must be refused")
	}
}

// TestDecisionsAreRecorded gives the shell something to show, and the tests
// something to assert on.
func TestDecisionsAreRecorded(t *testing.T) {
	p := &prompter{approved: false}
	g := permission.New(permission.DefaultPolicy(), p)

	_, _ = g.Permit(context.Background(), "read", args(`{}`), true, nil)
	_, _ = g.Permit(context.Background(), "write", args(`{}`), false, nil)

	log := g.Decisions()
	if len(log) != 2 {
		t.Fatalf("recorded %d decisions, want 2", len(log))
	}
	if log[0].Effect != permission.Allow {
		t.Fatalf("first effect = %v", log[0].Effect)
	}
	if !log[1].Asked || log[1].Granted {
		t.Fatalf("second decision = %+v", log[1])
	}
}

// TestSummaryIsBounded keeps a large argument from flooding a terminal.
func TestSummaryIsBounded(t *testing.T) {
	req := permission.Request{
		Tool:      "write",
		Arguments: json.RawMessage(`{"content":"` + strings.Repeat("x", 5000) + `"}`),
	}

	summary := req.Summary()
	if len(summary) > 200 {
		t.Fatalf("summary is %d bytes, want it bounded", len(summary))
	}
	if !strings.HasPrefix(summary, "write ") {
		t.Fatalf("summary should name the tool: %q", summary)
	}
}
