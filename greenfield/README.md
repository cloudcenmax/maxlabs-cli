# Greenfield SDE Team protocol v1

`pack.json` supplies both clients with flexible artifact fields, conditional stack
guidance, and legacy scoped rules. Canonical JSON storage is YAML 1.2 compatible;
no workflow server or YAML parser is required. Artifact files are immutable and
hash checked; human-only design, specification and release approvals bind to
artifact/reference fingerprints. Planning changes invalidate affected gates.

## Outcome briefs

New projects default to `execution_mode: flexible`. A task needs a summary and
observable acceptance outcomes. Optional role is implementation (default),
foundation or qa; `depends_on` is an array of task IDs. Files/commands are hints,
not allowlists. Set `serial: true` when work must run alone. Plans require an
acyclic graph, resolved decisions/review blockers, and independent QA following
all implementation/foundation work. Stack and architecture come from the spec.
Laravel-specific guidance applies only when Laravel is selected.

Existing state without execution_mode loads as `scoped`, preserving historical
Laravel exact-file scopes, approved commands, foundation ordering and coverage.
Human `/team mode flexible` or desktop settings migrates it and invalidates
spec/release approval. Models have no mode-switch or approval tool.

## Execution and integration

The dispatcher executes ready waves of up to three workers, eight tasks per
batch, with three attempts per task and forty model steps per child. Shared
setup can precede parallel feature slices; QA follows the combined result.
A cross-client project lease protects state mutations, refreshed every ten
seconds and expiring after two minutes without a heartbeat.

A runner-owned source snapshot repository lives under `.maxlabs/team/execution`.
It excludes .env, credential metadata, symlinks and dependency trees, bounded to
5000 files / 64 MiB. Each task gets its own branch/worktree, normal project tools,
a scrubbed provider-credential environment, approved briefs and previous evidence.
Context is bounded to 96 KB and tool results to 16 KB. Runtime dependencies and
local environment setup must be installed in worktrees; these are not shared.

The `verify` tool records actual command results; a completed JSON report alone
cannot pass a task. Workers choose the real tests and may repair failures.
Successful implementation branches merge serially with Git. Conflicts abort the
merge, preserve both branches/evidence and stop the next wave. Failed attempts
retain checkpoint commits so retries can reconcile with newer integration work.
QA receives the combined diff and implementation evidence, runs its own checks,
and must leave application source unchanged. Semantic correctness and adequate
test coverage still require independent review; a command exit code proves only
that the command succeeded.

Release acceptance verifies the combined source still matches QA's snapshot and
checks every destination change against the original baseline before applying
source. It never deploys or publishes. The original Git checkout remains on its
branch with changes available for review/commit. Worktree branches and immutable
attempt reports remain available for inspection; automatic cleanup is not yet
implemented. This is Git isolation, not an OS sandbox: shell programs retain
normal OS permissions, with ordinary high-impact operation approval checks.

## Design and API evidence

Discovery may casually ask about existing design inspiration; it is optional.
Only exact human-supplied paths can be attached by the planner. Attached roots
are read-only, with canonical path containment, credential and symlink checks.
Desktop provides a collapsible preview panel; CLI opens the OS browser.

Up to three bounded research jobs run concurrently, six per turn. Successful
fetches preserve source URL, retrieval time, content hash and content. Normalize
only needed operations into concise integration briefs, retaining unknowns,
authentication, schemas, errors, pagination and retry/webhook details as relevant.
Truncated sources remain excerpts. Documented schemas and mocks do not prove live
API behavior. Six fetches per agent bound repeated documentation costs.

## Live acceptance-test metering

From a source checkout, for an opt-in live run, `node scripts/greenfield-e2e-proxy.mjs` starts a localhost
relay using the CLI's signed-in account. Point test clients at its printed `/v1`
URL with any placeholder API key, and use isolated session/history stores and a
disposable workspace. Every submitted inference request is billable normally.
The relay does not write prompts or credentials to its report; it records model,
role, duration, HTTP outcome, and provider token receipts for planner/research/
worker/QA calls. Configure `GREENFIELD_E2E_REPORT`, `GREENFIELD_E2E_PROXY_PORT`,
and `GREENFIELD_E2E_MAX_CALLS` as needed. The default request ceiling is 120.

Input buckets are disjoint: uncached input + cache reads + cache writes. Reasoning
is a subset of output and must not be added again. Missing usage is reported as
unknown. Token counts alone do not establish a monetary cost; use actual account
billing or applicable provider prices. Flexible worker attempts store provider token receipts in their run records; latest-attempt totals exclude previous attempts and planner/research usage. Use the relay for complete whole-team metering.
