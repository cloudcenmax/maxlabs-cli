// Package harness is the cache-optimised coding harness.
//
// # Design
//
// The harness is built around one obligation: every model request must be
// reconstructable from an append-only session log. Prefix-cache stability is a
// consequence of that, not a separate thing to maintain - an append-only log
// projected by a per-node pure function yields requests that extend their
// predecessors whenever the request envelope is unchanged.
//
// # Layout
//
//	internal/session   append-only event log, surface projection, generations
//	internal/assemble  the only component permitted to build a request
//	internal/wire      deterministic encoding, prefix units, request hashing
//	internal/llm       transport boundary and an offline cache model
//
// # Provider neutrality
//
// The harness names no inference provider and no vendor. Cache behaviour is
// expressed as capabilities, never as a brand: a provider either caches
// implicitly, requires explicit breakpoints, or does not cache. Anything
// provider-specific belongs in the caller or in the gateway that routes to the
// provider, not here.
//
// This constraint is enforced by TestNoVendorNames, so it cannot regress by
// accident.
package harness
