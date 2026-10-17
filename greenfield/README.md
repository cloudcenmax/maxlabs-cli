# Greenfield SDE Team contract v1

`pack.json` is consumed by both the npm CLI and the Go desktop harness. Its
required fields, Laravel instructions, and permitted check prefixes form one
versioned protocol. `fixtures.json` exercises the same contracts in both runtime
test suites. The Go adapter is in this package; the TypeScript adapter is
`src/greenfield.ts`. Neither requires a YAML parser or an external workflow server.

## Artifacts and approvals

Artifacts are immutable files with SHA-256 checksums, indexed in `state.json`.
Saving a changed artifact retains earlier files. The manifest contains current
artifact references, human approval fingerprints, task results, input file
fingerprints, and explicitly attached reference directories. Treat planning state
as trusted local project data, not a secure multi-user approval service.

Only human client controls can approve design, spec, and release. The model tool
has no approval operation. Design approval covers the brief and prototype; spec
approval covers the brief, design, spec, tasks, integrations, planning reviews,
and reference attachments. Source evidence does not invalidate an approved plan.
Release acceptance requires all tasks, including independent QA, to pass.

The readiness compiler checks required artifact fields, unresolved decisions,
unique acceptance IDs with expected behavior, requirement coverage, task paths,
command prefixes, dependency existence/cycles, foundation ordering, and QA
ordering/read-only scope. Semantic correctness still requires review and tests.
Task roles are `foundation`, `implementation`, and `qa`. Bootstrap is an optional
foundation-only string such as `"13.*"`; use the version agreed during planning.

## Execution

The planner can inspect, save artifacts, read attached references, fetch public
reference documentation, delegate bounded research, and run ready tasks. It
cannot bypass the runner with direct edits, shell execution, or ordinary
unscoped delegation. Researchers cannot implement. Workers get fresh sessions
with the approved task and contracts; prototype HTML and raw evidence are omitted
from their initial context. Context over 96 KB is rejected for refinement.

Workers return `{"outcome":"completed","summary":"...","blockers":[]}` or a
blocked report. A completed report alone cannot pass a task: the runner executes
every approved check and records its output. File writes are limited to exact
paths, verified against canonical workspace containment. Commands are restricted
to known verification/setup prefixes and cannot contain shell composition or
expansion. Commands and project scripts retain normal OS permissions.

A cross-process directory lease serializes mutations and execution across CLI
and desktop. It is refreshed every ten seconds and expires after two minutes
without a heartbeat. Interrupted runs remain visible and can be retried; input
changes from an abruptly terminated worker require explicit spec reapproval.
Retries stop after three attempts; a human can review and reapprove to reset
failed attempts. Application workers are sequential so shared databases, routes,
and contracts cannot race. Research is parallel, with serialized permission
prompts and artifact persistence.

## Reference and integration research

Reference roots can be attached through client controls or the planner’s
`attach_reference` action when the exact literal path occurs in a human-authored
chat turn. The planner guard rejects paths sourced only from assistant/tool text;
researchers and workers cannot attach folders. Design inspiration is optional. Reads
resolve canonical paths, reject traversal and symlink escapes, omit dependency
folders from directory listings, and refuse environment/Git metadata. References
are read-only and are not executable instructions. The approved design captures
the selected reference's intended tokens, components, and behavior.

Successful research fetches automatically create source evidence artifacts.
The planner normalizes required API methods into integration contracts, keeping
source URLs, API versions, authentication, errors, retries, unknowns, and evidence
status explicit. `documented`, `schema_validated`, and `sandbox_tested` describe
different evidence; neither a source summary nor a mock is a live sandbox test.
No credentials belong in contracts. Source snapshots are not refreshed silently;
the planner decides when research is needed and a changed integration contract
requires spec reapproval.

## Beta limits

Only Laravel is supported. Storage is JSON, not arbitrary YAML syntax. The task
compiler validates and dispatches model-authored plans rather than synthesizing
Laravel code deterministically. Deployment remains a separately authorized
operation. Browser and visual QA depend on configured project checks and human
inspection. Provider calls and real external integrations need live acceptance
testing beyond the offline harness suite.

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
billing or applicable provider prices. Normal client turn meters currently show
the coordinator's usage; use the relay when measuring the whole team.

Planners and each researcher have a six-fetch documentation ceiling. The fixed
`team environment` action observes installed tool versions without exposing an
arbitrary shell. `team artifact` reads contracts without loading prototype HTML
and limits evidence excerpts; complete snapshots remain on disk. Workflow policy
is sent once in the system policy rather than duplicated in user history.


Approved batches dispatch ready tasks deterministically, up to eight, without
coordinator inference. The first failure stops the batch. Successful delivery
creates a runner-authored release with check evidence and the approved setup
instructions, and still requires client release acceptance. Retry packets include
previous failure summaries; check logs and immutable attempt reports preserve
details across reapproval. Worker report formatting gets one tool-free repair.
The model receives at most 16 KB per tool result; evidence hooks retain the
original result. Retrieve targeted source ranges when output is marked truncated.

Task `read` paths identify fingerprinted inputs, not a read sandbox. Workers can
inspect other code inside the workspace; exact path enforcement applies to their
file mutations. New direction stops deterministic execution at a task boundary
so affected contracts can be reviewed. QA receives check evidence before review.
