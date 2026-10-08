# MaxLabs CLI

The cache-optimised MaxLabs coding agent for your terminal.

## Install and run

MaxLabs CLI is implemented in TypeScript and distributed as a normal npm
package. It contains no native binary, installer, or platform-specific `.exe`.
Node.js 22.13 or newer is required.

```bash
npm install --global @maxlabs/cli
maxlabs
```

Run it without a global installation with:

```bash
npx @maxlabs/cli
```

OAuth is attempted first. Personal and work sessions are stored separately in
`~/.maxlabs/oauth.json` with user-only permissions. Use `/account` to see the
available contexts, `/account add` to connect another context through browser
consent, and `/account 2` (or its name/ID) to switch. Switching clears the
conversation so personal context cannot cross into a work request. `/logout`
clears every saved OAuth context. Set `MAXLABS_API_KEY` and `MAXLABS_BASE_URL`
when an API-key fallback is needed. Common options include:

Production defaults to `https://console.maxlabs.cenmax.in` for OAuth and
`https://api.maxlabs.cenmax.in/v1` for API requests. Both remain overridable
with `MAXLABS_AUTH_URL` and `MAXLABS_BASE_URL` for development and testing.

```bash
maxlabs --workspace . --model auto --auto
maxlabs --prompt "review this change" --web-search auto
```

While a turn is running, type another instruction and press Enter. MaxLabs
queues it and applies it after the current model request or tool operation reaches
a safe boundary. Use `/model`, `/auto`, `/plan`, `/act`, `/think`, `/web`, and
`/help` inside the interactive shell.

## The idea

An agent's cost is dominated by re-sending a growing prefix to the model on
every step. Provider-side prefix caches make that re-send dramatically cheaper,
but only for the part of the request that is byte-identical to a previous
request, compared from the first token.

The tempting approach is to build an agent and then add "prefix discipline".
MaxLabs CLI inverts that. Its one obligation is:

> **Every model request must be reconstructable from an append-only session
> log.**

Prefix-cache stability is then a *consequence*, not a thing to maintain: an
append-only log projected by a per-node pure function yields requests that
extend their predecessors whenever the request envelope is unchanged. Stability
is emergent, not managed.

Two rules follow, and they are load-bearing:

1. **Enforce by unrepresentability, not by validation.** Given a choice between
   detecting a cache-hostile request and making one impossible to construct,
   choose the latter. A request that is merely detected still ships.
2. **Make the change channel the log.** Plugins cannot rewrite a built request.
   If something must change what the model sees, it appends an event.

## Segment layout

The request is assembled from ordered segments, most-stable first. This ordering
*is* the cache strategy; anything that changes every step is pushed to the tail,
where its invalidation cost is bounded to itself.

| Segment | Content | Stability |
|---|---|---|
| S0 | Tool declarations | Immutable; canonically ordered |
| S1 | Agent identity + persona prefix | Session-immutable |
| S2 | Static guidance + mode policy | Session-immutable |
| S3 | Workspace instructions — carried **in history** | Append-only |
| S4 | Conversation history | Append-only |
| S5 | Compaction marker | Session-stable, below S1 |
| S6 | Runtime context | Volatile, always last |

S3 is in history rather than in the system prompt deliberately: a mid-session
`AGENTS.md` edit then costs an append instead of a head rewrite. Some providers
treat a cached system instruction as immutable, which makes this mandatory
rather than stylistic.

## Layout

```
internal/session   append-only event log, surface projection, generations
internal/assemble  the only component permitted to build a request
internal/wire      deterministic encoding, prefix units, request hashing
internal/tools     tool registry, filesystem and shell tools, bounded output
internal/agent     the turn/step loop: tool dispatch, series, retries
internal/compact   pressure-driven compaction and the tool-result pruner
internal/permission tool gating: allow, ask, deny, and plan mode
internal/tui       the interactive terminal shell
internal/llm       transport boundary and an offline cache model
internal/meter     provider-reported usage accounting
```

`assemble` is the only package allowed to construct a request. Nothing else may
add, reorder, or format bytes sent to a provider. That structural rule is what
makes the properties below testable.

## The two properties

**Prefix extension.** Within a request series, every request's unit sequence
must extend its predecessor's. This single property catches essentially every
cache regression: prompt interpolation, tool reordering, message rewriting, and
truncation drift.

Note that this is *not* a byte-prefix check over the whole JSON body. Providers
frame the envelope, so the previous body ends in `]}` and the next one does not
— a naive byte comparison would fail even for a perfectly append-only request.
`wire.Units` decomposes the request so the property can be checked directly.

**Determinism.** The same log and configuration reconstruct the same request
bytes, so cache behaviour is auditable and resume is safe.

## Testing

```
go test ./...
```

Two tiers, deliberately separated:

- **Offline** (always runs). `llm.PrefixCacheStub` models prefix-only matching
  from token 0 and the block-size floor below which nothing is cached, so the
  cache-hit gate runs without a network. Mocks establish append-extension.
- **Live** (not yet present). Sufficiency requires a real provider cache hit.
  Mocks can only overstate hits, never understate them, so the offline gate is
  necessary but not sufficient.

Several tests are **mutation tests**: they assert that a deliberate mistake
*collapses* the hit rate or *breaks* prefix extension. If they ever stop failing,
the corresponding gate has stopped measuring anything.

## Provider neutrality

MaxLabs CLI names no inference provider and no vendor. Cache behaviour is
expressed as a capability — implicit caching, explicit breakpoints, or none —
never as a brand.

Provider-specific behaviour belongs in the caller or in the gateway that routes
to the provider. This is enforced mechanically by `TestNoVendorNames`, which
scans every source file and fails the build.

## Status

Milestone 6: delegation.

Working: everything through M5, plus subagents. The model can delegate a
self-contained task to a child agent with its own context and receive only the
answer, so a long search does not fill the parent's window with detail.

Not yet: nothing on the original ladder.

## Subagents and the cache

The child inherits the parent's identity, guidance and tool declarations
**byte-identically** and differs only from S3 onward. That is deliberate. Giving
a subagent its own persona is the obvious thing to do and would change S1 - the
most heavily cached bytes in the request - so every delegation would pay a cold
prefix write on a prompt that was otherwise identical.

The delegation tool therefore stays visible inside a child even though a child at
the depth limit may not use it: removing it would change S0, the tool block, and
S0 comes first. One refused call is cheaper than a cold prefix.

A subagent is capped at **40 steps**, separately from the 100 a top-level turn
gets. A child's transcript is invisible while it runs, so a tight ceiling turns
"still working" into "silently stopped"; 40 is enough for a real investigation
and still bounds a task that was under-specified. It is a constant, not a flag -
`subagent.DefaultMaxSteps`.


## Terminal rendering

Model output is markdown, and the shell renders it rather than printing it raw:
headings become bold, bullets become `•`, emphasis and inline code become ANSI,
and code fences are indented and dimmed with their contents left untouched.

Rendering is line-based so it survives streaming - a construct split across two
deltas is never printed half-formed. It applies only when stdout is a terminal;
piped and redirected output passes through byte-accurate, because a redirect
should not silently rewrite the model's answer.

## Modes and keys

Plan mode has a keyboard switch rather than only a command, because a mode with
no visible exit is a trap:

| key | effect |
|---|---|
| `shift+tab` | switch between act and plan mode, from anywhere including mid-prompt |
| `up` / `down` | previous and next prompt |
| `left` / `right`, `home` / `end`, `ctrl+a` / `ctrl+e` | move within the line |
| `backspace`, `delete`, `ctrl+u` | edit the line |
| `ctrl+c` | stop the running turn, or exit when idle |
| `enter` | submit |

The active mode is stated at the **end** of the status line, where it is read
last and remembered, and plan mode says how to leave it:

    ⠹ step 3 · cache 68% · 12.4k in · PLAN MODE · shift+tab to exit
    ⠹ step 3 · cache 68% · 12.4k in · act · shift+tab · ctrl+c

Reaching the arrow keys and Shift+Tab at all needs raw terminal mode, since the
line discipline otherwise swallows them and hands over whole lines. `ISIG` is
deliberately left enabled: with it off, Ctrl+C stops generating SIGINT and
arrives as a byte, so a wedged read could no longer be interrupted from the
keyboard. Ctrl+C therefore still reaches the signal handler, which stops the turn
if one is running and exits if not.

On a platform with no raw-mode implementation the shell falls back to line-based
input. History and Shift+Tab are unavailable; everything else works.

## File diffs

When the model writes or edits a file, the change is shown as a diff:

    ✎ src/LendingService.php  +5 -1
      ⋮
        if (!$this->books->exists($bookId)) {
      -     throw new RuntimeException('missing book');
      +     throw new BookMissing($bookId);
        }
      +
      + if (!$this->users->exists($userId)) {
      +     throw new UserMissing($userId);
      + }
        $loan = $this->loans->create($bookId, $userId);

Red for removals, green for additions, dim for context, `⋮` where unchanged
runs were elided. Two lines of context survive around each change.

The output is capped at 20 lines. A whole-file rewrite changes every line, so
there is no unchanged run to elide and the diff would otherwise be the entire
file; what was withheld is stated rather than silently dropped:

    ⋮ 380 more line(s)

Two properties are deliberate. The diff goes to the **person, not the model**:
the model is told "Edited src/LendingService.php." because a diff of its own
edit is context it already has, while the person watching has no other way to
see what happened to their files. And a write that changes nothing prints no
heading at all.

The diff is exact for files up to four million comparison cells and falls back to
trimming the common head and tail beyond that, so a small edit in a very large
file still reports the change rather than allocating a table to find it.

## Approvals

Read-only tools run unattended. Anything that can change the workspace stops and
asks, and the prompt is built to be unmistakable:

    ⏸  approval needed  the agent is waiting for your decision

        write PLAN.md ...

        y  allow this once
        a  always allow write for this session
        n  deny  (the agent is told why, and will plan around it)

    choice>

What you asked and what the model answered are styled differently, so a
transcript stays readable when scanned:

    ❯ read the plan and summarise it

      Here is the summary. Availability is derived, not stored.

The prompt is drawn as a band: the brand pink as the background with black on
top, both stated explicitly rather than inherited. That is the point - contrast
becomes a property of this code instead of the user's theme.

| against | contrast |
|---|---|
| black text on the band | **7.8:1** (WCAG AAA) |
| the band vs a black terminal | 7.8:1 |
| the band vs a dark grey terminal | 6.2:1 |
| the band vs a white terminal | 2.7:1 |

The text contrast is the one that matters, and it is identical on every theme
because neither colour is inherited. The pink is a saturated mid-tone rather than
a dark or light band, which is what lets one colour serve both: it pops hard on
dark and still reads as a clear coloured block on light.

The line you are typing is always visible. Raw mode turns off the terminal's own
echo, so the shell has to draw input itself: on its own row while idle, and at
the end of the status line while an agent is working. Without that, typing is
invisible until Enter.

Two details matter more than they look. The **spinner stops** while the question
is on screen: a spinner claims something is progressing, and redrawing it several
times a second wipes the question away - which is what made approvals unreadable.
And the words are accepted as well as the letters, because a prompt that spells
out "always allow" and then rejects the word would be perverse.



A turn can run for a minute or more. There are two ways out, and both are
checked between steps rather than only at the start:

- `stop`, `wait`, `cancel`, `halt`, `abort` - matched exactly and case
  insensitively, so "stop the loop in my code" stays a prompt.
- `ctrl+c` - cancels the turn in flight; pressed again while idle it exits.

An interrupted turn is reported as interrupted, not as a failure. The
distinction matters: a shell that prints an error for something the user asked
for teaches them not to press the key.

## Thinking effort

Reasoning is capped explicitly, because leaving it to the route is not neutral:
on at least one live route the default is to reason until the output budget is
gone, return nothing usable, and take over a minute doing it.

| level | what it sends |
|---|---|
| `minimal` | `{"effort":"minimal"}` |
| `low` | `{"effort":"low"}` |
| `medium` | `{"effort":"medium"}` — the default |
| `high` | `{"effort":"high"}` |
| `default` | nothing at all, letting the route decide |

Set it with `--thinking <level>`, or change it mid-session with `/think <level>`.
The current level shows in the status line as `think:med`, near the front so a
narrow terminal truncates the counters rather than the setting that explains why
the model is slow.

There is no "off". Disabling reasoning outright is rejected with a 400 by a live
route, so the ladder starts at the lowest effort that route accepts rather than
at a value it does not have.

**Changing the level costs nothing.** Effort is a sampling parameter and does not
alter the token sequence the provider caches, so the same prompt reads the same
cached prefix at any level. A test asserts the prompt hash is identical across
levels, because if it ever were not, every change would discard the whole cached
prefix for a setting that changes no bytes the model reads.

## Thinking

When the model reasons, a single dim line above the status line reports how much
of it there has been:

    ✻ thinking · 1.2k tokens
    ⠹ think:med · step 2 · cache 71% · 8.2k in · act · ctrl+c to stop

The reasoning itself is **not** shown. A chain of thought is long, changes every
line, and would scroll the answer off the screen; what someone waiting actually
wants is to know how much longer, and a rising count answers that without
competing for attention with the answer.

The line **collapses by itself** the moment the answer starts, leaving one dim
record of what it cost:

The live figure is prefixed `~` and is honestly approximate. Measured against
live streams, reasoning ran from **2.6 to 5.4 characters per token** - a 2x
spread - so no local constant is right and a precise-looking number would be a
lie. It exists to show progress.

The accurate figure comes from the endpoint's own usage block, which arrives at
the end of the stream. That is when the summary is written, quoting what the
thinking actually cost rather than repeating the guess:

    ✻ thought for 4.2s · 140 tokens

A route that reports no reasoning tokens produces no summary rather than
"0 tokens".

The region is always exactly one row, which keeps the erase count predictable: a
region taller than the terminal could not be erased correctly, since erasing
means stepping back over a known number of rows.

Two things are deliberately awkward and worth keeping that way. Reasoning and
content arrive through **one** callback rather than two, because the transition
between them has to be observed in order and two callbacks would race to decide
which came first. And both spellings of the reasoning field are read, because
which arrives is a property of the route rather than of anything the caller
controls.

Piped and one-shot runs do not show reasoning at all. It is distinguished by
being dimmed, and dimming is lost through a pipe - at which point it would read
as part of the answer in a captured log.

## Width

The terminal's column count is read at startup and again on every resize. It is
not cosmetic. The status line is pinned by moving the cursor up one row, so a
status line wider than the terminal wraps onto two rows - after which every
redraw is off by one and the transcript comes apart. The status line is therefore
truncated to the width, measured in visible columns rather than bytes, because
escape sequences occupy none.

Streamed text is wrapped to the same width at word boundaries, so the terminal
never has to wrap it. The alternative - breaking on a character count - puts hard
newlines in the middle of prose and makes a long answer look ragged.

The input line scrolls horizontally rather than wrapping, for the same reason:
the redraw clears a single row, so a wrapped input line would leave its overflow
behind on every keystroke.

## Step limit

A turn is capped at **100 tool-calling steps**. The ceiling is shown in the
status line as `step 12/100`, so it is visible while it approaches rather than
only at the moment it is hit. Change it with `--max-steps`.

Reaching it is a **stop, not a fault**:

    [stopped at the step limit: 100 step(s), 74 tool call(s).
     send another message to carry on, or raise it with --max-steps]

It used to end with `turn ended: agent: the turn exceeded its step limit`, which
both alarms the user and buries the answer the turn had already produced.

## The status line

While a turn runs, a single line is pinned to the bottom showing the mode, the
step, the cache hit rate and the input token count, with a spinner in the brand
pink. It is written with plain ANSI cursor moves rather than a TUI framework -
three escape sequences cover the whole thing.

Set `Ansi` false (the runner does this automatically when stdout is not a
terminal) and every escape is suppressed, so a piped or redirected run produces
clean text.
