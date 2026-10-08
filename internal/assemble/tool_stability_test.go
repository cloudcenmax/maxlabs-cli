package assemble_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"censi/harness/internal/assemble"
	"censi/harness/internal/tools"
)

// This file pins segment S0's stability contract and, just as importantly,
// documents where it cannot be met.
//
// There is a genuine tension between two properties a tool list wants:
//
//   - Stable across PROCESS STARTS, so two runs of the same build present the
//     same bytes. That wants a canonical order derived from the set.
//   - Stable across SET CHANGES, so adding a tool appends rather than shifting
//     everything after it. That wants an append-only order.
//
// Sorting by name gives the first and not the second: a new tool that sorts
// before an existing one is inserted in the middle, and every schema after it
// moves. An explicit order gives both, provided new tools are added at the end.
// The tests below pin both behaviours so the trade-off is visible rather than
// discovered in production.

// registryOf builds a registry containing one stub tool per name.
func registryOf(t *testing.T, names ...string) *tools.Registry {
	t.Helper()

	r := tools.NewRegistry()
	for _, name := range names {
		err := r.Register(toolStub{def: tools.Definition{
			Name:        name,
			Description: "The " + name + " tool.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			ReadOnly:    true,
		}})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	return r
}

type toolStub struct{ def tools.Definition }

func (s toolStub) Definition() tools.Definition { return s.def }

func (s toolStub) Execute(_ context.Context, _ json.RawMessage) (tools.Result, error) {
	return tools.Text("ok"), nil
}

// s0 renders segment S0 exactly as the wire layer would, so these assertions are
// about the bytes a provider caches rather than about an intermediate value.
func s0(t *testing.T, r *tools.Registry, order []string) []string {
	t.Helper()

	canonical, err := assemble.CanonicalizeTools(r.Schemas(), order)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	out := make([]string, 0, len(canonical))
	for _, schema := range canonical {
		b, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out = append(out, string(b))
	}

	return out
}

func isPrefix(prev, next []string) bool {
	if len(prev) > len(next) {
		return false
	}
	for i := range prev {
		if prev[i] != next[i] {
			return false
		}
	}
	return true
}

// TestS0IsIndependentOfRegistrationOrder is the property that keeps two runs of
// the same build presenting identical bytes.
func TestS0IsIndependentOfRegistrationOrder(t *testing.T) {
	a := s0(t, registryOf(t, "write", "bash", "read", "grep"), nil)
	b := s0(t, registryOf(t, "grep", "read", "write", "bash"), nil)

	if len(a) != len(b) {
		t.Fatalf("different lengths: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("S0 depends on registration order at index %d:\n %s\n %s", i, a[i], b[i])
		}
	}
}

// TestS0AppendsWhenOrderIsExplicit is the cache-safe growth path: an explicitly
// ordered tool list extends when a tool is added at the end.
func TestS0AppendsWhenOrderIsExplicit(t *testing.T) {
	before := s0(t, registryOf(t, "read", "write"), []string{"read", "write"})
	after := s0(t, registryOf(t, "read", "write", "bash"), []string{"read", "write", "bash"})

	if !isPrefix(before, after) {
		t.Fatalf("appending a tool to an explicit order must extend S0:\n before=%v\n after=%v", before, after)
	}
}

// TestS0BreaksWhenANewToolSortsEarlier documents the hazard rather than hiding
// it. Name ordering is stable across starts but NOT across set changes, so a
// tool that sorts into the middle invalidates every schema after it.
//
// This is not a bug to fix in CanonicalizeTools - it is the inherent cost of
// deriving order from names, and the reason an explicit full order is the right
// choice for a long-lived session.
func TestS0BreaksWhenANewToolSortsEarlier(t *testing.T) {
	before := s0(t, registryOf(t, "bash", "read"), nil)
	after := s0(t, registryOf(t, "bash", "edit", "read"), nil)

	if isPrefix(before, after) {
		t.Fatal("a tool sorting into the middle should break the prefix; " +
			"if this now passes, the ordering rule changed and the comment above is stale")
	}

	// The failure is exactly at the inserted position: everything before it
	// survives, everything after it moves by one.
	if before[0] != after[0] {
		t.Fatal("the first schema should have survived; the hazard is insertion, not a total reshuffle")
	}
	if before[1] == after[1] {
		t.Fatal("expected the second schema to have shifted")
	}
}

// TestExplicitOrderPutsUnlistedToolsLast proves the rest tail is appended rather
// than interleaved, which is what makes adding a tool to a partially-ordered
// registry a tail append.
func TestExplicitOrderPutsUnlistedToolsLast(t *testing.T) {
	got := s0(t, registryOf(t, "bash", "read", "grep", "write"), []string{"read", "write"})

	names := make([]string, 0, len(got))
	for _, s := range got {
		var decoded struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(s), &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		names = append(names, decoded.Name)
	}

	want := []string{"read", "write", "bash", "grep"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
}

// TestSchemaBytesCarryNoAbsolutePaths guards a subtle cache hazard: an absolute
// path baked into a declaration differs per machine, so two developers running
// the same build would present different bytes for the same logical tool set.
func TestSchemaBytesCarryNoAbsolutePaths(t *testing.T) {
	canonical, err := assemble.CanonicalizeTools(registryOf(t, "read", "grep").Schemas(), nil)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	for _, schema := range canonical {
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(encoded) == "" {
			t.Fatal("empty schema")
		}
		if strings.Contains(string(encoded), root) {
			t.Fatalf("schema %s embeds a machine-specific path: %s", schema.Name, encoded)
		}
	}
}
