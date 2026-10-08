// Package llm is the transport boundary: one interface, and for M0 a stub that
// models provider prefix-cache semantics so the cache gates can run offline.
//
// KV Cache effect: this package reports provider cache metrics; it never
// assembles a request and never invalidates a prefix.
package llm

import (
	"context"
	"strings"
	"sync"

	"censi/harness/internal/session"
	"censi/harness/internal/wire"
)

// Usage is per-call token accounting.
//
// The buckets are DISJOINT. UncachedInputTokens is uncached input only; cached
// input is reported separately. Billed input is the sum of the three input
// buckets. Collapsing them would inflate apparent miss cost by up to 50x on a
// warm prefix, which is the whole quantity this harness exists to minimise.
type Usage struct {
	UncachedInputTokens int
	CacheReadTokens     int
	CacheWriteTokens    int
	OutputTokens        int

	// ReasoningTokens is how much of OutputTokens was spent thinking.
	//
	// It is reported by the endpoint and is the ONLY accurate source for that
	// figure: a local estimate has to infer tokens from characters, and measured
	// against live streams that ratio ranged from 2.6 to 5.4 characters per
	// token, so no constant is right. A live progress indicator can only be an
	// approximation; this is the number to quote.
	ReasoningTokens int

	// WebSearches is how many searches the endpoint ran for this request.
	//
	// Reported by the endpoint rather than inferred, and the reason a per-turn
	// search budget can be enforced at all: the caps a provider accepts are per
	// REQUEST, and an agent turn is many requests. Without a count to subtract
	// from, "ten searches a turn" would mean ten searches every step.
	WebSearches int
}

// BilledInputTokens is the disjoint-bucket sum.
func (u Usage) BilledInputTokens() int {
	return u.UncachedInputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// HitRate is the fraction of billed input served from cache. Zero when nothing
// was billed.
func (u Usage) HitRate() float64 {
	billed := u.BilledInputTokens()
	if billed == 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(billed)
}

// Add accumulates another attempt's usage into this one.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		UncachedInputTokens: u.UncachedInputTokens + o.UncachedInputTokens,
		CacheReadTokens:     u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens:    u.CacheWriteTokens + o.CacheWriteTokens,
		OutputTokens:        u.OutputTokens + o.OutputTokens,
	}
}

// Response is one settled model call.
type Response struct {
	Message session.Message
	Usage   Usage
}

// Provider performs one model call. Implementations must not mutate the
// request: it is the frozen product of assembly, and a mutation here would
// silently break the reconstruction invariant.
type Provider interface {
	Complete(ctx context.Context, req wire.Request) (Response, error)
}

// DefaultCacheBlockTokens is the storage granularity below which content is not
// cached at all.
//
// It is a parameter of the cache *model*, not a universal constant: providers
// differ, and a caller modelling a specific route overrides it through
// PrefixCacheStub.BlockTokens. Nothing else in the harness may depend on this
// value.
const DefaultCacheBlockTokens = 64

// PrefixCacheStub is an offline stand-in for a provider prefix cache.
//
// It models the two properties that actually determine our cost:
//
//  1. Matching is PREFIX-ONLY, from the first token. A difference in the middle
//     never hits, so perturbing any early byte discards the entire common
//     suffix.
//  2. A prefix shorter than the block granularity is not cached at all. A
//     previous request's full prompt persists as a cached unit at its request
//     boundary, so a complete match is served whole; the granularity is a
//     floor, not a truncation of a longer match.
//
// The match is computed against every previously seen request, which is the
// optimistic bound: a real provider may evict, so this stub can only overstate
// cache hits, never understate them. A gate that passes here is necessary but
// not sufficient; a live-provider test supplies sufficiency.
type PrefixCacheStub struct {
	// BlockTokens overrides DefaultCacheBlockTokens.
	BlockTokens int

	mu      sync.Mutex
	seen    [][]string
	replies []string
	n       int
}

// blockTokens resolves the effective granularity.
func (s *PrefixCacheStub) blockTokens() int {
	if s.BlockTokens > 0 {
		return s.BlockTokens
	}
	return DefaultCacheBlockTokens
}

// NewPrefixCacheStub builds a stub that answers with the supplied replies in
// order, repeating the last one once exhausted.
func NewPrefixCacheStub(replies ...string) *PrefixCacheStub {
	if len(replies) == 0 {
		replies = []string{"ok"}
	}
	return &PrefixCacheStub{replies: replies}
}

// Tokenize is the stub's token model: whitespace-separated words. It is not a
// real tokenizer, and it does not need to be - the gate cares about the shape of
// prefix matching, not about exact counts.
func Tokenize(prompt []byte) []string {
	return strings.Fields(string(prompt))
}

// Complete implements Provider.
func (s *PrefixCacheStub) Complete(_ context.Context, req wire.Request) (Response, error) {
	prompt, err := wire.Prompt(req)
	if err != nil {
		return Response{}, err
	}
	tokens := Tokenize(prompt)

	s.mu.Lock()
	best := 0
	for _, prev := range s.seen {
		n := commonPrefixLen(prev, tokens)
		if n > best {
			best = n
		}
	}
	// A cached unit is persisted at each request boundary (end of user input,
	// end of model output), so a previous request's full prompt is itself a
	// cached unit and a complete match is served whole. The 64-token rule is a
	// FLOOR below which nothing is cached at all, not a truncation of a longer
	// match.
	cached := 0
	if best >= s.blockTokens() {
		cached = best
	}
	s.seen = append(s.seen, tokens)

	reply := s.replies[len(s.replies)-1]
	if s.n < len(s.replies) {
		reply = s.replies[s.n]
	}
	s.n++
	s.mu.Unlock()

	return Response{
		Message: session.Message{
			Role:    session.RoleAssistant,
			Content: []session.Block{session.Text(reply)},
		},
		Usage: Usage{
			UncachedInputTokens: len(tokens) - cached,
			CacheReadTokens:     cached,
			CacheWriteTokens:    0,
			OutputTokens:        len(strings.Fields(reply)),
		},
	}, nil
}

func commonPrefixLen(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
