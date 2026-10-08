// Package tui is the interactive terminal shell.
//
// It is deliberately thin. Everything that decides what the model sees lives in
// the assembler, everything that decides whether a tool may run lives in the
// permission package, and this package only connects a terminal to both. A shell
// that owned policy would be a second place for that policy to drift.
//
// Input handling is the one genuinely subtle part. A turn runs for tens of
// seconds during which the user may want to interrupt it, and an approval prompt
// may appear inside that same window. Both need the keyboard. The resolution is
// a single long-lived reader goroutine feeding one channel, with the turn loop
// routing each line: to a waiting approval, to the interrupt, or to the next
// prompt. Two readers on one terminal would steal each other's keystrokes.
//
// KV Cache effect: none. The shell changes configuration between turns and
// never assembles or alters a request.
package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/diff"
	"censi/harness/internal/llm"
	"censi/harness/internal/permission"
	"censi/harness/internal/session"
	"censi/harness/internal/tools"
)

// ErrExit is returned when the user asks to leave.
var ErrExit = errors.New("tui: exit requested")

// stopWords interrupt a running turn. They are matched case-insensitively and
// exactly, so "stop the loop in my code" stays a prompt while "stop" tells the
// shell to interrupt.
var stopWords = map[string]bool{
	"stop": true, "wait": true, "cancel": true, "halt": true, "abort": true,
}

// isStop reports whether a line asks to interrupt.
func isStop(line string) bool {
	return stopWords[strings.ToLower(strings.TrimSpace(line))]
}

// isApprovalAnswer recognises the vocabulary printed by the permission prompt.
// Piped input can arrive a few microseconds before the tool reaches Ask; keeping
// these short answers in the shell queue lets Ask claim them without allowing a
// model to consume a permission decision as ordinary steering.
func isApprovalAnswer(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes", "allow", "ok", "a", "always", "all", "n", "no", "deny":
		return true
	default:
		return false
	}
}

// Shell is an interactive session over one agent.
type Shell struct {
	in  *bufio.Reader
	out io.Writer

	agent *agent.Agent
	guard *permission.Guard

	// base is the assembly configuration with plan mode OFF. Toggling plan mode
	// derives from this rather than mutating it, so the two states cannot drift.
	base assemble.Config

	planMode  bool
	autoMode  bool
	workspace string
	catalog   llm.ModelCatalog

	// onModelChange keeps other model-aware components, such as delegation,
	// aligned with the parent agent after an in-session switch.
	onModelChange func(assemble.Config)

	// thinking is the reasoning effort level, for display and for /think.
	thinking string

	status *status

	// lines carries every keystroke line, from one reader goroutine.
	lines chan string

	// approvalCh receives a line when an approval prompt is waiting for it, so
	// an answer is never mistaken for an interrupt.
	approvalCh      chan string
	approvalMu      sync.Mutex
	approvalPending bool

	// queued holds lines typed while the agent was busy but which were not
	// interruptions.
	queuedMu sync.Mutex
	queued   []string

	cancelMu sync.Mutex
	cancel   context.CancelFunc

	// streamed records whether any delta reached the screen this turn, so the
	// final text is not printed a second time.
	streamed atomic.Bool

	// maxSteps is the per-turn ceiling, shown in the status line.
	maxSteps int

	// thinkingStarted is when reasoning began this step, for the cost summary.
	thinkingStarted time.Time
	thinkingMu      sync.Mutex

	// editor reads keystrokes when the terminal allows it.
	editor *lineEditor

	// busy gates the editor's redrawing: while a turn runs, the status line owns
	// the bottom of the screen.
	busy atomic.Bool

	// approvalActive is true while a question is on screen. The editor's redraw
	// gate is the same flag that silences it during a turn, so this is what tells
	// it the row is free again.
	approvalActive atomic.Bool

	// modelSelectionActive gives the model picker its own input prompt while it
	// waits for a number or model id.
	modelSelectionActive atomic.Bool

	exitOnce sync.Once
	exitCh   chan struct{}
}

// Config assembles a Shell.
type Config struct {
	In    io.Reader
	Out   io.Writer
	Agent *agent.Agent
	Guard *permission.Guard

	// Base is the session's assembly configuration with plan mode off.
	Base assemble.Config

	// Workspace is shown in the banner. It is display only; the tools already
	// hold the real confinement.
	Workspace string

	// Ansi enables the spinner and the pinned status line. It must be false for
	// piped or redirected output, where escape sequences are noise.
	Ansi bool

	// Thinking is the reasoning effort level.
	Thinking string

	// Auto starts the shell in audited autonomous mode.
	Auto bool

	// MaxSteps is the per-turn ceiling, for display only.
	MaxSteps int

	// Catalog supplies the enabled public models shown by /model.
	Catalog llm.ModelCatalog

	// OnModelChange updates components that inherit the parent's assembly.
	OnModelChange func(assemble.Config)
}

// New returns a shell.
func New(cfg Config) (*Shell, error) {
	if cfg.Agent == nil {
		return nil, fmt.Errorf("tui: an agent is required")
	}
	if cfg.In == nil || cfg.Out == nil {
		return nil, fmt.Errorf("tui: input and output are required")
	}

	s := &Shell{
		in:            bufio.NewReader(cfg.In),
		out:           cfg.Out,
		agent:         cfg.Agent,
		guard:         cfg.Guard,
		base:          cfg.Base,
		workspace:     cfg.Workspace,
		catalog:       cfg.Catalog,
		onModelChange: cfg.OnModelChange,
		thinking:      cfg.Thinking,
		autoMode:      cfg.Auto,
		maxSteps:      cfg.MaxSteps,
		status:        newStatus(cfg.Out, cfg.Ansi),
		lines:         make(chan string, 8),
		approvalCh:    make(chan string, 1),
		exitCh:        make(chan struct{}),
	}

	file, _ := cfg.In.(*os.File)
	if !cfg.Ansi {
		// Raw mode without a reference terminal is not worth attempting.
		file = nil
	}
	s.editor = newLineEditor(s.in, cfg.Out, file, &s.busy, s.promptText, cfg.Ansi)
	s.editor.onInput = s.status.setInput
	s.editor.renderSubmitted = func(text string) string {
		// A blank line after the prompt keeps a long answer from running into
		// what was asked for.
		return s.status.echoPrompt(text) + "\n\n"
	}

	// The agent streams into the status line and reports each step, so the
	// counters move during the silent stretches between model calls.
	s.agent.SetHooks(s.onDelta, s.onStep)
	s.agent.SetUsageHook(s.onUsage)
	s.agent.SetToolResultHook(s.onToolResult)
	s.agent.SetSteerHook(s.onSteer)

	return s, nil
}

// PlanMode reports whether plan mode is on.
func (s *Shell) PlanMode() bool { return s.planMode }

// onDelta streams model output and remembers that it was shown. It runs on the
// transport's goroutine.
//
// The first piece of content collapses the thinking region, which is why the two
// arrive through one callback: the transition has to be observed in order, and
// two callbacks would race to decide which came first.
func (s *Shell) onDelta(delta llm.Delta) {
	if delta.Reasoning != "" {
		if s.streamed.Load() {
			// Content has already started; late reasoning is not a region that
			// can be opened again.
			return
		}

		if s.thinkingStarted.IsZero() {
			s.thinkingStarted = time.Now()
		}

		s.status.beginThinking()
		s.status.pushThinking(delta.Reasoning)

		return
	}

	if delta.Text == "" {
		return
	}

	if !s.streamed.Load() {
		// The answer is starting: collapse the scaffolding. The accurate cost is
		// reported later, from the usage block.
		s.status.endThinking()
		s.streamed.Store(true)
	}

	s.status.write(delta.Text)
}

// onSteer separates the completed operation's output from the response that
// follows the new direction. The turn remains live and keeps its tool history.
func (s *Shell) onSteer(string) {
	s.status.flushStream()
	s.streamed.Store(false)
	s.thinkingMu.Lock()
	s.thinkingStarted = time.Time{}
	s.thinkingMu.Unlock()
	s.status.println("(direction applied; reassessing)")
}

// onToolResult shows a file diff, when the tool produced one.
//
// Only write and edit produce diffs, and only for a person: the model is told
// "Wrote x (n bytes)" and has no use for a diff of its own change. Showing it
// here is the whole point - otherwise a file changes and the only evidence is
// the model's own description of what it did.
func (s *Shell) onToolResult(_ session.Block, result tools.Result) {
	if result.IsError || len(result.Diff) == 0 {
		return
	}

	added, removed := diff.Stats(result.Diff)

	// A write that changed nothing is not worth a heading.
	if added == 0 && removed == 0 {
		return
	}

	s.status.println("")
	s.status.printf("  %s %s  %s %s",
		s.status.brand("✎"), s.status.bold(result.DiffTarget),
		s.status.tone("+"+itoa(added)), s.status.tone("-"+itoa(removed)))

	shown := diff.Condense(result.Diff, diffContextLines)

	// A rewrite of a whole file changes every line, so there is no unchanged run
	// to elide and the diff would be the entire file. The cap is what keeps this
	// to the part that was actually edited.
	hidden := 0
	if len(shown) > maxDiffLines {
		hidden = len(shown) - maxDiffLines
		shown = shown[:maxDiffLines]
	}

	for _, line := range shown {
		switch line.Kind {
		case diff.Add:
			s.status.printf("    %s %s", s.status.added("+"), line.Text)

		case diff.Del:
			s.status.printf("    %s %s", s.status.removed("-"), line.Text)

		case diff.Gap:
			s.status.printf("    %s", s.status.tone("⋮"))

		default:
			s.status.printf("      %s", s.status.tone(line.Text))
		}
	}

	if hidden > 0 {
		s.status.printf("    %s", s.status.tone(fmt.Sprintf("⋮ %d more line(s)", hidden)))
	}

	s.status.println("")
}

// onUsage reports what a model call actually cost, once the endpoint says.
//
// This is where the accurate reasoning figure comes from. The live indicator can
// only estimate, because token counts cannot be recovered from characters.
func (s *Shell) onUsage(usage llm.Usage) {
	s.thinkingMu.Lock()
	started := s.thinkingStarted
	s.thinkingStarted = time.Time{}
	s.thinkingMu.Unlock()

	if usage.ReasoningTokens <= 0 {
		return
	}

	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}

	s.status.reportThinking(elapsed, usage.ReasoningTokens)
}

// onStep updates the status counters. It runs on the agent's goroutine.
func (s *Shell) onStep(step int) {
	meter := s.agent.Meter()
	s.status.setProgress(step, meter.Totals().BilledInputTokens(), meter.SeriesHitRate())
}

// SetPlanMode enters or leaves plan mode, updating both the tools gate and the
// prompt section in one place so they cannot disagree.
func (s *Shell) SetPlanMode(active bool) {
	s.planMode = active

	if s.guard != nil {
		policy := s.guard.Policy()
		policy.PlanMode = active
		s.guard.SetPolicy(policy)
	}

	cfg := s.base
	cfg.Guidance = withSection(cfg.Guidance, assemble.PlanModeSection(active))
	s.agent.SetAssembly(cfg)

	s.status.setMode(s.modeLabel())
	s.status.setPlanMode(active)
}

// SetAutoMode enables or disables audited autonomous execution. Auto mode is
// not an allow-all switch: the permission auditor still refuses forbidden work
// and asks for anything risky or unclear.
func (s *Shell) SetAutoMode(active bool) {
	s.autoMode = active

	if active && s.planMode {
		s.SetPlanMode(false)
	}

	if s.guard != nil {
		policy := s.guard.Policy()
		policy.Auto = active
		s.guard.SetPolicy(policy)
	}

	s.status.setMode(s.modeLabel())
}

// SetThinkingLabel seeds the status line from the runner's flag.
func (s *Shell) SetThinkingLabel(level string) {
	s.thinking = level
	s.status.setThinkingLevel(level)
}

// withSection replaces any existing section with the same name, so toggling a
// mode twice does not accumulate duplicate section names.
func withSection(sections []assemble.Section, section assemble.Section) []assemble.Section {
	out := make([]assemble.Section, 0, len(sections)+1)
	for _, existing := range sections {
		if existing.Name != section.Name {
			out = append(out, existing)
		}
	}

	if section.Text != "" {
		out = append(out, section)
	}

	return out
}

func (s *Shell) modeLabel() string {
	if s.planMode {
		return "plan"
	}
	if s.autoMode {
		return "auto"
	}

	return "act"
}

// Interrupt cancels the turn in flight, if any.
func (s *Shell) Interrupt() {
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// Run reads prompts until the user leaves.
func (s *Shell) Run(ctx context.Context) error {
	s.status.start()
	defer s.status.stopLoop()

	s.status.setThinkingLevel(s.thinkingLabel())

	// Raw mode is what makes Shift+Tab, the arrow keys and Backspace reachable at
	// all: the terminal's line discipline otherwise swallows them and hands over
	// whole lines. It is entered once for the session and restored on every exit
	// path, including the ones that return early - a terminal left in raw mode
	// after the process dies is unusable.
	if s.editor.file != nil {
		if state, err := makeRaw(s.editor.file); err == nil {
			defer state.restore(s.editor.file)
		}
	}

	// The width bounds the status line and wraps streamed text. Without it the
	// pinned row can wrap, and a pinned row that wraps stops being pinned: every
	// redraw after that is off by one row and the screen scrambles.
	s.refreshWidth()

	go s.readLines()

	s.banner()

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		s.status.setMode(s.modeLabel())

		// Anything typed during the previous turn is submitted now, rather than
		// silently dropped.
		if queued := s.takeQueued(); len(queued) > 0 {
			for _, line := range queued {
				if strings.HasPrefix(strings.TrimSpace(line), "/") {
					if err := s.command(ctx, strings.TrimSpace(line)); err != nil {
						if errors.Is(err, ErrExit) {
							return nil
						}
						s.status.printf("error: %v", err)
					}
					continue
				}
				s.turn(ctx, strings.TrimSpace(line))
			}
			continue
		}

		// The shell owns the idle prompt. The editor takes the row over on the
		// first keystroke and repaints exactly this text, so the two agree.
		fmt.Fprint(s.out, s.promptText())

		line, ok := s.nextLine(ctx)
		if !ok {
			fmt.Fprintln(s.out)
			return nil
		}

		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if err := s.command(ctx, text); err != nil {
				if errors.Is(err, ErrExit) {
					return nil
				}
				s.status.printf("error: %v", err)
			}
			continue
		}

		s.turn(ctx, text)
	}
}

// promptText is the text the terminal shows before input.
//
// The editor redraws this line on every keystroke, so it has to return exactly
// what is already on screen or the row would flicker between two variants.
func (s *Shell) promptText() string {
	switch {
	case s.approvalActive.Load():
		return s.status.brand("  choice> ")
	case s.modelSelectionActive.Load():
		return s.status.brand("model> ")
	case s.planMode:
		return s.status.brand("plan> ")
	default:
		return "> "
	}
}

// readLines is the single owner of the terminal's input.
//
// Every keystroke passes through here, which is what lets Shift+Tab switch modes
// from anywhere - including in the middle of typing a prompt.
func (s *Shell) readLines() {
	defer close(s.lines)

	for {
		line, action, ok, err := s.editor.readLine()
		if err != nil || !ok {
			return
		}

		switch action {
		case keyShiftTab, keyTab:
			s.togglePlanMode()
			continue

		case keyCtrlL:
			// Redraw a clean screen. The transcript is not replayed, which is
			// what a scrollback-clearing key usually means in practice.
			s.status.println("")
			continue

		case keyCtrlC:
			s.InterruptOrExit()
			continue
		}

		s.lines <- line
	}
}

// RefreshWidth re-reads the terminal size. It is exported for the SIGWINCH
// handler in the runner.
func (s *Shell) RefreshWidth() { s.refreshWidth() }

// refreshWidth reads the terminal size and applies it.
//
// It is re-read on SIGWINCH too, since a resized terminal that keeps wrapping at
// the old width is the same bug in slower motion.
func (s *Shell) refreshWidth() {
	columns := terminalWidth(s.editor.file)
	if columns <= 0 {
		// A conventional default, so a terminal that will not report its size
		// still avoids unbounded lines.
		columns = defaultColumns
	}

	s.status.setWidth(columns)
	s.editor.width = columns
}

// SetThinking changes how hard the model is asked to think.
//
// The level is a request parameter, not prompt content, so changing it mid
// session costs nothing: the same prompt is sent either way and reads the same
// cached prefix.
func (s *Shell) SetThinking(level string) {
	level = strings.ToLower(strings.TrimSpace(level))

	known := false
	for _, candidate := range []string{"minimal", "low", "medium", "high", "default"} {
		if level == candidate {
			known = true

			break
		}
	}

	if !known {
		s.status.printf("unknown thinking level %q   (one of: minimal, low, medium, high, default)", level)

		return
	}

	value := level
	if value == "default" {
		value = ""
	}

	cfg := s.agent.Assembly()
	cfg.ReasoningEffort = value
	s.agent.SetAssembly(cfg)

	s.thinking = level
	s.status.setThinkingLevel(s.thinkingLabel())

	s.status.printf("thinking set to %s", s.thinkingLabel())
}

// thinkingLabel is the short form shown in the status line.
func (s *Shell) thinkingLabel() string {
	switch s.thinking {
	case "":
		return "default"
	case "minimal":
		return "min"
	case "medium":
		return "med"
	default:
		return s.thinking
	}
}

// togglePlanMode flips the mode and says so.
//
// The message names the exit as well as the entry: the whole complaint about
// plan mode was that once in it, there was no visible way out.
func (s *Shell) togglePlanMode() {
	s.SetPlanMode(!s.planMode)

	if s.planMode {
		s.status.println(s.status.brand("  PLAN MODE on") + "  " +
			s.status.tone("workspace changes are refused. shift+tab to exit."))
		return
	}

	if s.autoMode {
		s.status.println(s.status.brand("  auto mode resumed") + "  " +
			s.status.tone("audited safe work runs unattended. shift+tab for plan mode."))

		return
	}

	s.status.println(s.status.brand("  act mode on") + "  " +
		s.status.tone("workspace changes will ask for approval. shift+tab for plan mode."))
}

// InterruptOrExit cancels the turn in flight, or leaves when nothing is running.
//
// Ctrl+C means "stop this" while work is happening and "let me out" when it is
// not, and a shell that only ever did one of those would be wrong half the time.
func (s *Shell) InterruptOrExit() {
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()

	if cancel != nil {
		s.status.println(s.status.tone("  (interrupting)"))
		cancel()

		return
	}

	s.exitOnce.Do(func() { close(s.exitCh) })
}

// nextLine returns the next prompt line, or false at end of input.
func (s *Shell) nextLine(ctx context.Context) (string, bool) {
	select {
	case line, ok := <-s.lines:
		return line, ok
	case <-s.exitCh:
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

// command handles a slash command.
func (s *Shell) command(ctx context.Context, text string) error {
	fields := strings.Fields(text)

	switch fields[0] {
	case "/exit", "/quit", "/q":
		return ErrExit

	case "/plan":
		s.SetPlanMode(true)
		s.status.println("plan mode ON - workspace changes are refused; the model plans instead.")

	case "/act":
		s.SetPlanMode(false)
		s.SetAutoMode(false)
		s.status.println("act mode ON - workspace changes will ask for approval.")

	case "/auto":
		s.SetAutoMode(true)
		s.status.println("auto mode ON - audited safe work runs unattended; risky work still asks.")

	case "/toggle":
		s.SetPlanMode(!s.planMode)
		s.status.printf("plan mode %s", onOff(s.planMode))

	case "/think":
		if len(fields) < 2 {
			s.status.printf("thinking: %s   (one of: minimal, low, medium, high, default)",
				s.thinkingLabel())

			return nil
		}

		s.SetThinking(fields[1])

	case "/model":
		return s.chooseModel(ctx, fields[1:])

	case "/web":
		mode := ""
		if len(fields) > 1 {
			mode = strings.ToLower(fields[1])
		}
		return s.setWebSearch(mode)

	case "/tools":
		for _, schema := range s.agent.Assembly().Tools {
			s.status.printf("  %s", schema.Name)
		}

	case "/decisions":
		for _, d := range s.guard.Decisions() {
			s.status.printf("  %-6s asked=%-5v granted=%-5v %s", d.Effect, d.Asked, d.Granted, d.Reason)
		}

	case "/help":
		s.help()

	default:
		return fmt.Errorf("unknown command %q; try /help", fields[0])
	}

	return nil
}

// chooseModel lists enabled Gateway models and switches the next request. With
// no argument it opens a numbered picker; `/model pro` is the scriptable form.
func (s *Shell) chooseModel(ctx context.Context, arguments []string) error {
	if s.catalog == nil {
		return errors.New("model catalog is unavailable")
	}

	models, err := s.catalog.Models(ctx)
	if err != nil {
		return err
	}

	choice := ""
	if len(arguments) > 0 {
		choice = strings.TrimSpace(arguments[0])
	} else {
		current := s.agent.Assembly().Model
		s.status.println("")
		s.status.println(s.status.bold("  available models"))
		for index, model := range models {
			marker := " "
			if model.ID == current {
				marker = "*"
			}

			description := strings.TrimSpace(model.Description)
			if description != "" {
				description = "  " + s.status.tone(description)
			}

			s.status.printf("  %s %d. %-12s%s", marker, index+1, model.ID, description)
		}
		s.status.println("")

		s.modelSelectionActive.Store(true)
		defer s.modelSelectionActive.Store(false)

		fmt.Fprint(s.out, s.promptText())
		line, ok := s.nextLine(ctx)
		if !ok {
			return nil
		}

		choice = strings.TrimSpace(line)
		if choice == "" {
			s.status.println("model selection cancelled")

			return nil
		}
	}

	if number, conversionErr := strconv.Atoi(choice); conversionErr == nil {
		if number < 1 || number > len(models) {
			return fmt.Errorf("model number %d is out of range", number)
		}

		choice = models[number-1].ID
	}

	var selected *llm.ModelCard
	for index := range models {
		if models[index].ID == choice {
			selected = &models[index]

			break
		}
	}

	if selected == nil {
		return fmt.Errorf("unknown or disabled model %q", choice)
	}

	config := s.agent.Assembly()
	if config.Model == selected.ID {
		s.status.printf("model remains %s", selected.ID)

		return nil
	}

	config.Model = selected.ID
	s.base.Model = selected.ID
	s.agent.SetAssembly(config)

	if s.onModelChange != nil {
		s.onModelChange(config)
	}

	s.status.printf("model switched to %s", selected.ID)

	return nil
}

func (s *Shell) setWebSearch(mode string) error {
	if mode == "" {
		current := s.agent.Assembly().WebSearch
		if current == "" {
			current = "off"
		}
		s.status.printf("web search: %s   (one of: off, auto, always)", current)

		return nil
	}

	if mode != "off" && mode != "auto" && mode != "always" {
		return fmt.Errorf("web search must be off, auto, or always")
	}

	config := s.agent.Assembly()
	config.WebSearch = normaliseWebSearch(mode)
	s.base.WebSearch = config.WebSearch
	s.agent.SetAssembly(config)
	uses := s.base.WebSearchUses
	if uses <= 0 {
		uses = 10
	}
	s.agent.SetWebSearch(config.WebSearch, uses)

	if s.onModelChange != nil {
		s.onModelChange(config)
	}

	s.status.printf("web search: %s", mode)

	return nil
}

func normaliseWebSearch(mode string) string {
	if mode == "off" {
		return ""
	}

	return mode
}

// turn runs one user message to completion.
//
// The turn runs on its own goroutine so this loop stays free to read the
// keyboard, which is what makes an interruption possible at all.
func (s *Shell) turn(ctx context.Context, text string) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	steering := agent.NewSteeringQueue()

	s.cancelMu.Lock()
	s.cancel = cancel
	s.cancelMu.Unlock()

	defer func() {
		s.cancelMu.Lock()
		s.cancel = nil
		s.cancelMu.Unlock()
	}()

	type outcome struct {
		turn agent.TurnResult
		err  error
	}

	done := make(chan outcome, 1)

	s.status.setStepLimit(s.maxSteps)
	s.streamed.Store(false)
	s.thinkingMu.Lock()
	s.thinkingStarted = time.Time{}
	s.thinkingMu.Unlock()
	s.busy.Store(true)
	s.status.begin(s.modeLabel())

	// Plan mode is stated on the status line, not just implied by the prompt.
	s.status.setPlanMode(s.planMode)

	go func() {
		turn, err := s.agent.RunSteerable(turnCtx, text, steering)
		done <- outcome{turn: turn, err: err}
	}()

	interrupted := false

	// A closed channel is always ready, so the input case is disabled by nil-ing
	// it rather than spinning on it: a spin here would starve the completion and
	// burn a core while the turn finished.
	lines := s.lines

poll:
	for {
		select {
		case res := <-done:
			s.finish(res.turn, res.err, interrupted)
			break poll

		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}

			switch {
			case s.deliverApproval(line):
				// The line answered a pending approval.

			case isStop(line):
				interrupted = true
				cancel()
				s.status.println("(interrupting)")

			case isApprovalAnswer(line):
				s.queue(line)

			case strings.HasPrefix(strings.TrimSpace(line), "/"):
				// Slash commands control the shell, not the model. Preserve them for
				// the idle command loop so /exit, /model and mode changes cannot be
				// mistaken for steering text merely because input was read quickly.
				s.queue(line)

			default:
				if steering.Add(line) {
					s.status.println("(direction received; applying after the current operation)")
				} else {
					// The agent crossed its atomic final boundary just before this
					// select ran. Preserve the line as a normal next turn.
					s.queue(line)
					s.status.println("(current turn finished; queued as the next turn)")
				}
			}
		}
	}

	s.busy.Store(false)
	s.status.end()
}

func (s *Shell) finish(turn agent.TurnResult, err error, interrupted bool) {
	// Release the renderer's held line first. Printing the summary before it
	// would leave the tail of the answer stranded below the summary.
	s.status.flushStream()

	// Only print the final text when it was NOT already streamed. Printing it
	// unconditionally duplicates the whole answer, which looks like the model
	// said everything twice.
	if !s.streamed.Load() && strings.TrimSpace(turn.Text) != "" {
		// A provider that could not stream still gets rendered output.
		s.status.writeRaw("\n" + renderMarkdown(turn.Text, s.status.ansi) + "\n")
	}

	switch {
	case interrupted:
		s.status.printf("\n[interrupted after %d step(s), %d tool call(s)]", turn.Steps, turn.ToolCalls)

	case errors.Is(err, agent.ErrStepLimit):
		// Reaching the ceiling is a normal way for a long turn to stop, not a
		// fault. Reporting it as one both alarms the user and hides the answer
		// already produced, so it is stated plainly with the way forward.
		s.status.printf("\n[stopped at the step limit: %d step(s), %d tool call(s). "+
			"send another message to carry on, or raise it with -max-steps]",
			turn.Steps, turn.ToolCalls)

	case err != nil:
		s.status.printf("\nturn ended: %v", err)
	default:
		s.status.printf("\n[%d step(s), %d tool call(s), cache hit %.0f%%]",
			turn.Steps, turn.ToolCalls, s.agent.Meter().SeriesHitRate()*100)
	}

	s.status.println("")
}

// deliverApproval hands a line to a waiting approval prompt. It reports whether
// one was waiting, so an answer is never mistaken for an interrupt.
func (s *Shell) deliverApproval(line string) bool {
	s.approvalMu.Lock()
	pending := s.approvalPending
	s.approvalMu.Unlock()

	if !pending {
		return false
	}

	s.approvalCh <- line

	return true
}

func (s *Shell) queue(line string) {
	s.queuedMu.Lock()
	defer s.queuedMu.Unlock()

	s.queued = append(s.queued, line)
}

// takeQueued returns lines typed while the agent was busy.
func (s *Shell) takeQueued() []string {
	s.queuedMu.Lock()
	defer s.queuedMu.Unlock()

	out := s.queued
	s.queued = nil

	return out
}

// Ask implements permission.Prompter.
//
// The prompt has to do three things an ordinary line of output does not: stop
// the spinner, make clear that the AGENT is asking rather than the harness
// narrating, and spell the options out. A terse "[y/n/a]" reads like shell
// chrome and users scroll past it.
func (s *Shell) Ask(ctx context.Context, req permission.Request) (bool, bool, error) {
	// Stop the spinner. The harness is blocked on the user, not working.
	s.status.pause()

	// Hand the bottom row back to the editor. Raw mode means the terminal does
	// not echo, so if the editor is suppressed too then the user types blind -
	// which is exactly what happens if this is left to the turn's busy flag.
	s.busy.Store(false)
	s.approvalActive.Store(true)

	defer func() {
		s.approvalActive.Store(false)
		s.busy.Store(true)
		s.status.resume()
	}()

	s.status.println("")
	s.status.println(s.status.brand("  ⏸  approval needed") + "  " +
		s.status.tone("the agent is waiting for your decision"))
	s.status.println("")

	call := s.status.bold(req.Tool)
	if preview := req.ArgumentsPreview(); preview != "" {
		call += " " + s.status.tone(preview)
	}
	s.status.println("      " + call)

	s.status.println("")
	s.status.println("      " + s.status.bold("y") + "  allow this once")
	if req.OneTimeOnly {
		s.status.println("      " + s.status.tone("This high-impact command must be reviewed each time: "+req.RiskReason))
	} else {
		label := req.Tool
		if req.Tool == "bash" {
			label = "similar bash commands"
		}
		s.status.println("      " + s.status.bold("a") + "  always allow " +
			s.status.code(label) + " for this session")
	}
	s.status.println("      " + s.status.bold("n") + "  deny  " +
		s.status.tone("(the agent is told why, and will plan around it)"))
	s.status.println("")

	s.approvalMu.Lock()
	s.approvalPending = true
	s.approvalMu.Unlock()

	defer func() {
		s.approvalMu.Lock()
		s.approvalPending = false
		s.approvalMu.Unlock()
	}()

	// A bare cursor after a block of text gives no clue that input is expected.
	// The editor repaints this exact line on every keystroke.
	fmt.Fprint(s.out, s.promptText())

	// A line may already be waiting: the user cannot see when the model finishes
	// a step, so an answer routinely arrives before the question is drawn.
	// Without this check the answer is parked in the queue and the prompt blocks
	// forever on a keystroke that has already happened.
	if line, ok := s.takeQueuedFirst(); ok {
		return answer(line, !req.OneTimeOnly)
	}

	select {
	case line := <-s.approvalCh:
		return answer(line, !req.OneTimeOnly)
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
}

// answer interprets an approval reply.
//
// The words are accepted alongside the letters: the prompt spells the options
// out, and it would be perverse to reject the spelling it just showed. Anything
// unrecognised, including an empty line, is a refusal - guessing "yes" from a
// stray keystroke is not a mistake worth being able to make.
func answer(line string, allowAlways bool) (bool, bool, error) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "allow", "ok":
		return true, false, nil
	case "a", "always", "all":
		if !allowAlways {
			return false, false, nil
		}
		return true, true, nil
	default:
		return false, false, nil
	}
}

// takeQueuedFirst removes and returns the oldest queued line.
func (s *Shell) takeQueuedFirst() (string, bool) {
	s.queuedMu.Lock()
	defer s.queuedMu.Unlock()

	if len(s.queued) == 0 {
		return "", false
	}

	line := s.queued[0]
	s.queued = s.queued[1:]

	return line, true
}

// renderMarkdown renders a whole message, for the path where nothing streamed.
func renderMarkdown(text string, styled bool) string {
	m := &markdown{styled: styled}

	out := m.feed(strings.TrimRight(text, "\n") + "\n")

	return strings.TrimRight(out+m.flush(), "\n")
}

func (s *Shell) banner() {
	s.status.println("MaxLabs CLI. /help for commands, /exit to leave.")
	s.status.printf("workspace: %s", s.workspace)
	s.status.printf("model:     %s", s.agent.Assembly().Model)
	s.status.println("")
}

func (s *Shell) help() {
	s.status.println(`  /plan       refuse workspace changes and plan instead
  /act        leave plan mode
  /auto       run audited safe work unattended; risky work still asks
  /toggle     switch plan mode
  /model      choose from enabled Gateway models
  /model ID   switch directly to one enabled model
  /web MODE   web grounding: off, auto, or always
  /think L    how hard the model thinks: minimal, low, medium, high, default
  /tools      list the tools the model can see
  /decisions  what was asked and how it was answered
  /exit       leave

  shift+tab   switch between act and plan mode
  up / down   previous and next prompt
  ctrl+c      stop the turn, or exit when idle
  stop|wait|cancel   interrupt the turn in flight`)
}

func onOff(v bool) string {
	if v {
		return "ON"
	}

	return "OFF"
}

// Compile-time proof that the shell is the guard's prompter and that the
// transport can stream.
var (
	_ permission.Prompter = (*Shell)(nil)
	_ llm.Streamer        = (*llm.HTTPProvider)(nil)
)
