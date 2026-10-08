package wire_test

import (
	"testing"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// TestWebSearchDoesNotDisturbThePrefix: the flag changes what comes back, not
// what was asked. If it reached the hash, switching grounding on mid-conversation
// would invalidate the cached prefix and cost more than the search.
func TestWebSearchDoesNotDisturbThePrefix(t *testing.T) {
	base := request(msg(session.RoleUser, "hello"))

	grounded := base
	grounded.WebSearch = "always"

	left, err := wire.Hash(base)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	right, err := wire.Hash(grounded)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if left != right {
		t.Fatal("turning grounding on changed the request hash, so the cached prefix would be lost")
	}

	unitsLeft, err := wire.Units(base)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}

	unitsRight, err := wire.Units(grounded)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}

	if len(unitsLeft) != len(unitsRight) {
		t.Fatalf("grounding changed the unit count: %d vs %d", len(unitsLeft), len(unitsRight))
	}

	for i := range unitsLeft {
		if string(unitsLeft[i].Bytes) != string(unitsRight[i].Bytes) {
			t.Fatalf("unit %d differs with grounding on", i)
		}
	}
}

// And the prompt itself is unchanged by it, which is what makes the above true.
func TestWebSearchIsNotSentAsPromptContent(t *testing.T) {
	base := request(msg(session.RoleUser, "hello"))

	grounded := base
	grounded.WebSearch = "always"

	plain, err := wire.Prompt(base)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	withFlag, err := wire.Prompt(grounded)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if string(plain) != string(withFlag) {
		t.Fatal("the flag leaked into the prompt bytes")
	}
}

// TestTheSearchBudgetTravelledDoesNotDisturbThePrefix: how many searches remain
// changes what comes back, not what was asked, so it must not reach the hash.
// Otherwise a turn's cache would be invalidated every time a search ran.
func TestTheSearchBudgetTravelledDoesNotDisturbThePrefix(t *testing.T) {
	base := request(msg(session.RoleUser, "hello"))
	base.WebSearch = "auto"
	base.WebSearchUses = 5

	spent := base
	spent.WebSearchUses = 2

	left, err := wire.Hash(base)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	right, err := wire.Hash(spent)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if left != right {
		t.Fatal("the remaining budget changed the request hash, so a search would invalidate the cache")
	}
}
