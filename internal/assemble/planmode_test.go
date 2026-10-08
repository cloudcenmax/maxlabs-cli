package assemble_test

import (
	"strings"
	"testing"

	"censi/harness/internal/assemble"
)

// planConfig builds a configuration with sections on both sides of the plan
// policy order, so the prefix property has something to be true about.
func planConfig(planMode bool) assemble.Config {
	cfg := assemble.Config{
		Provider:      "local",
		Model:         "default",
		Identity:      "You are a coding agent.",
		PersonaPrefix: "You are Worker by Example.",
		Guidance: []assemble.Section{
			{Name: "tool:bash", Order: assemble.OrderGuidance, Text: "Prefer targeted commands."},
		},
	}

	if planMode {
		cfg.Guidance = append(cfg.Guidance, assemble.PlanModeSection(true))
	} else {
		cfg.Guidance = append(cfg.Guidance, assemble.PlanModeSection(false))
	}

	return cfg
}

// TestPlanModePreservesTheCachedPrefixBeforeItsOrder is the reason plan policy
// sits at order 500 rather than first.
//
// Entering plan mode changes the prompt, unavoidably. What must NOT change is
// everything ahead of it: the harness identity and the persona prefix are the
// most-cached bytes in the system, and a mode toggle that discarded them would
// charge a full prefix miss for a change affecting a few hundred trailing
// tokens.
func TestPlanModePreservesTheCachedPrefixBeforeItsOrder(t *testing.T) {
	off := assemble.RenderSystem(planConfig(false))
	on := assemble.RenderSystem(planConfig(true))

	if off == on {
		t.Fatal("plan mode must change the rendered prompt, or it is not doing anything")
	}

	// Everything contributed before order 500 survives byte-for-byte.
	head := "You are a coding agent.\n\nYou are Worker by Example."
	if !strings.HasPrefix(on, head) {
		t.Fatalf("the prefix before order 500 changed:\n got: %q\nwant prefix: %q", on, head)
	}
	if !strings.HasPrefix(off, head) {
		t.Fatalf("the baseline does not start with the expected head: %q", off)
	}

	// And the change is confined to after that head.
	if on[len(head):len(head)+2] != "\n\n" {
		t.Fatalf("unexpected join after the preserved head: %q", on[len(head):len(head)+12])
	}
	if !strings.Contains(on, "PLAN MODE is active") {
		t.Fatalf("the plan section is missing: %q", on)
	}
}

// TestLeavingPlanModeRestoresTheExactPrompt guards a subtle trap: a user who
// toggles plan mode on and back off must not be left with a permanently
// different prefix. An empty section is dropped before joining, so the prompt
// returns to byte-identical rather than carrying a blank gap.
func TestLeavingPlanModeRestoresTheExactPrompt(t *testing.T) {
	never := assemble.RenderSystem(planConfig(false))

	// On, then off again.
	planConfig(true)
	after := assemble.RenderSystem(planConfig(false))

	if never != after {
		t.Fatalf("toggling plan mode off did not restore the prompt:\n      never: %q\nafter toggle: %q", never, after)
	}
}

// TestPlanSectionIsOrderedAfterPersonaAndBeforeToolGuidance pins the placement
// itself, so a future reorder cannot silently move the cache break.
func TestPlanSectionIsOrderedAfterPersonaAndBeforeToolGuidance(t *testing.T) {
	section := assemble.PlanModeSection(true)

	if section.Order != assemble.OrderPlanPolicy {
		t.Fatalf("order = %d, want OrderPlanPolicy (%d)", section.Order, assemble.OrderPlanPolicy)
	}
	if section.Order <= assemble.OrderPersonaPrefix {
		t.Fatal("plan policy must sort after the persona prefix, or toggling it would discard the persona")
	}
	if section.Order >= assemble.OrderGuidance {
		t.Fatal("plan policy must sort before tool guidance, which describes capabilities that do not change with the mode")
	}
}

// TestInactivePlanSectionRendersNothing keeps the inactive state free: a
// session that never uses plan mode must not pay for the feature.
func TestInactivePlanSectionRendersNothing(t *testing.T) {
	if text := assemble.PlanModeSection(false).Text; text != "" {
		t.Fatalf("an inactive plan section must render empty, got %q", text)
	}

	on := assemble.RenderSystem(planConfig(true))
	off := assemble.RenderSystem(planConfig(false))

	if strings.Contains(off, "PLAN MODE") {
		t.Fatalf("plan text leaked into the inactive prompt: %q", off)
	}
	if len(off) >= len(on) {
		t.Fatalf("the inactive prompt should be shorter: on=%d off=%d", len(on), len(off))
	}
}
