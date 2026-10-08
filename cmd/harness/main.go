// Command harness runs one turn of the agent against a configured endpoint.
//
// It is the smallest thing that makes the harness runnable end to end: build a
// workspace, register the tools, assemble a request, drive the loop, and report
// what happened. A full terminal interface replaces it later.
//
// Everything that identifies a service comes from a flag or the environment, so
// the harness itself names no host, no vendor, and no model.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"censi/harness/internal/agent"
	"censi/harness/internal/assemble"
	"censi/harness/internal/llm"
	"censi/harness/internal/oauth"
	"censi/harness/internal/permission"
	"censi/harness/internal/session"
	"censi/harness/internal/subagent"
	"censi/harness/internal/tools"
	"censi/harness/internal/tui"
)

// toolOrder is explicit rather than alphabetical.
//
// Sorting by name is stable across process starts but not across set changes: a
// tool that sorts into the middle shifts every schema after it and invalidates
// the cached prefix. An explicit order is append-only as long as new tools go at
// the end.
var toolOrder = []string{"read", "write", "edit", "glob", "grep", "bash", "webfetch"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		prompt     = flag.String("prompt", "", "The user message for this turn.")
		workspace  = flag.String("workspace", ".", "Directory the tools are confined to.")
		model      = flag.String("model", os.Getenv("HARNESS_MODEL"), "Model id to request.")
		baseURL    = flag.String("base-url", os.Getenv("HARNESS_BASE_URL"), "API root; /chat/completions is appended.")
		apiKey     = flag.String("api-key", os.Getenv("HARNESS_API_KEY"), "Credential for the endpoint.")
		authURL    = flag.String("auth-url", firstNonEmpty(os.Getenv("HARNESS_AUTH_URL"), "http://127.0.0.1:8000"), "OAuth gateway URL.")
		oauthPath  = flag.String("oauth-store", defaultOAuthStore(), "File used to store the OAuth session.")
		oauthLogin = flag.Bool("oauth-login", true, "Start OAuth device sign-in when no saved OAuth session exists; set false to use API-key fallback directly.")
		autoMode   = flag.Bool("auto", false, "Start in audited auto mode; safe work runs unattended and risky work still asks.")
		webSearch  = flag.String("web-search", "off", "Web grounding mode: off, auto, or always.")
		webUses    = flag.Int("web-search-uses", 10, "Maximum provider web searches per turn.")
		sessionID  = flag.String("session", "", "Stable conversation key; pins provider affinity.")
		maxSteps   = flag.Int("max-steps", 100, "Tool-calling steps allowed in this turn.")
		maxRetry   = flag.Int("max-retries", 3, "Additional attempts after a failed model request.")
		thinking   = flag.String("thinking", "medium", "How hard the model thinks: minimal, low, medium, high, or default to let the endpoint decide.")
		maxTokens  = flag.Int("max-tokens", 4096, "Output cap per request. Sent explicitly: an endpoint default can exceed its own remaining window.")
		timeout    = flag.Duration("timeout", 10*time.Minute, "Overall deadline for the turn.")
	)
	flag.Parse()
	*webSearch = strings.ToLower(strings.TrimSpace(*webSearch))
	if *webSearch != "off" && *webSearch != "auto" && *webSearch != "always" {
		return fmt.Errorf("-web-search must be off, auto, or always")
	}

	if *model == "" || (*baseURL == "" && *authURL == "") {
		return fmt.Errorf("-model and either -auth-url or -base-url are required")
	}

	oauthSession := oauth.NewSession(*authURL, *oauthPath)
	if shouldLoginOAuth(*authURL, *oauthLogin, *apiKey, oauthSession.SignedIn()) {
		if err := oauthSession.LoginDevice(context.Background(), os.Stderr); err != nil {
			if *apiKey == "" {
				return fmt.Errorf("OAuth sign-in: %w", err)
			}
			fmt.Fprintln(os.Stderr, "warning: OAuth unavailable; using API key fallback:", err)
		}
	}

	ws, err := tools.NewWorkspace(*workspace)
	if err != nil {
		return err
	}

	registry, err := buildRegistry(ws)
	if err != nil {
		return err
	}

	if *sessionID == "" {
		*sessionID = "harness-" + fmt.Sprint(time.Now().UnixNano())
	}

	log := session.New(nil)

	// One provider instance is shared by the parent and by every subagent, so
	// both sides send the same session key. That key is what keeps requests on
	// one endpoint, and an endpoint switch discards the cached prefix.
	provider := &llm.HTTPProvider{
		BaseURL:   *baseURL,
		APIKey:    *apiKey,
		Model:     *model,
		SessionID: *sessionID,
	}
	if strings.TrimSpace(*authURL) != "" {
		provider.OAuthBaseURL = strings.TrimRight(*authURL, "/") + "/app/v1"
		provider.OAuth = oauthSession
	}

	// The guard is shared with the shell and with every subagent: a delegated
	// task must not become a way around a refusal the user gave.
	guard := permission.New(permission.Policy{Auto: *autoMode}, nil)

	// The delegation tool is registered before the parent's tool list is
	// complete, then told the complete list. It has to inherit its own
	// declaration, or its children would declare one tool fewer than the parent
	// and their S0 - the first thing the provider caches - would not match.
	delegate, err := subagent.New(subagent.Config{
		Provider: provider,
		Registry: registry,
		Guard:    guard,
		MaxSteps: subagent.DefaultMaxSteps,
		MaxDepth: 1,
	})
	if err != nil {
		return err
	}

	if err := registry.Register(delegate); err != nil {
		return err
	}

	toolsInOrder, err := assemble.CanonicalizeTools(
		registry.Schemas(), append(append([]string{}, toolOrder...), subagent.ToolName))
	if err != nil {
		return err
	}

	assembly := assemble.Config{
		Provider:      "gateway",
		Model:         *model,
		Identity:      identity,
		Guidance:      guidance(),
		Tools:         toolsInOrder,
		MaxTokens:     *maxTokens,
		WebSearch:     normaliseWebSearch(*webSearch),
		WebSearchUses: max(0, *webUses),

		// Chosen explicitly rather than left to the endpoint: the default on at
		// least one live route is to reason until the output budget is gone,
		// which turns a two-second answer into a two-minute one.
		ReasoningEffort: normaliseThinking(*thinking),
	}

	// The child inherits the finished configuration, which is what lets it share
	// the parent's cached prefix.
	delegate.SetAssembly(assembly)

	a, err := agent.New(agent.Config{
		Assembly:      assembly,
		Guard:         guard,
		Provider:      provider,
		Tools:         registry,
		MaxSteps:      *maxSteps,
		MaxRetries:    *maxRetry,
		WebSearchUses: max(0, *webUses),
	}, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// One-shot mode: the prompt is the whole session.
	if strings.TrimSpace(*prompt) != "" {
		return oneShot(ctx, a, ws, *prompt, *model, *sessionID)
	}

	shell, err := tui.New(tui.Config{
		In:        os.Stdin,
		Out:       os.Stdout,
		Agent:     a,
		Guard:     guard,
		Base:      assembly,
		Workspace: ws.Root(),
		Ansi:      isTerminal(os.Stdout),
		Thinking:  normaliseThinking(*thinking),
		MaxSteps:  *maxSteps,
		Auto:      *autoMode,
		Catalog:   provider,
		OnModelChange: func(config assemble.Config) {
			provider.Model = config.Model
			delegate.SetAssembly(config)
		},
	})
	if err != nil {
		return err
	}

	// The guard asks through the shell, so approvals arrive on the same terminal
	// the conversation is running in.
	guard.SetPrompter(shell)

	// Ctrl+C interrupts the turn in flight rather than killing the process: a
	// long-running tool is exactly when someone reaches for it, and losing the
	// session at that moment would be the least helpful response available.
	// A second Ctrl+C, while idle, exits.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		for range signals {
			// Ctrl+C stops the turn if one is running, and exits if not.
			shell.InterruptOrExit()
		}
	}()

	// A resized window must be picked up, or the status line goes back to
	// wrapping and the pin breaks in slow motion.
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, syscall.SIGWINCH)

	go func() {
		for range resizes {
			shell.RefreshWidth()
		}
	}()

	defer signal.Stop(resizes)
	defer signal.Stop(signals)

	return shell.Run(ctx)
}

// shouldLoginOAuth starts a device flow only when this process has no saved
// session to reuse. The oauth-login flag enables first-run login; it must not
// turn every launch into a new authorization request.
func shouldLoginOAuth(authURL string, oauthLogin bool, apiKey string, signedIn bool) bool {
	if strings.TrimSpace(authURL) == "" || signedIn {
		return false
	}

	return oauthLogin || strings.TrimSpace(apiKey) == ""
}

// dim and reset style streamed reasoning in one-shot mode.
const (
	dim   = "\x1b[2m"
	reset = "\x1b[0m"
)

// thinkingLevels are the effort values the route accepts, plus the option to say
// nothing.
//
// "off" is deliberately absent. Disabling reasoning outright is rejected with a
// 400 by at least one live route, so the lowest rung is "minimal" rather than
// something that does not exist.
var thinkingLevels = []string{"minimal", "low", "medium", "high", "default"}

// normaliseThinking validates a level, falling back to the default.
func normaliseThinking(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))

	for _, known := range thinkingLevels {
		if level == known {
			if known == "default" {
				return ""
			}

			return known
		}
	}

	return "medium"
}

// isTerminal reports whether the writer is an interactive terminal.
//
// The status line is built from carriage returns and cursor moves, so writing it
// to a pipe or a file would fill the output with control characters. Piped runs
// therefore get clean text and no spinner.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// oneShot runs a single turn and prints the outcome, for scripting and tests.
func oneShot(ctx context.Context, a *agent.Agent, ws *tools.Workspace, prompt, model, sessionID string) error {
	fmt.Printf("workspace : %s\n", ws.Root())
	fmt.Printf("model     : %s\n", model)
	fmt.Printf("session   : %s\n", sessionID)
	fmt.Printf("tools     : %s\n\n", strings.Join(toolOrder, ", "))

	// One-shot mode streams straight to stdout: the same assembly, the same
	// cache behaviour, just without a status line to pin.
	// Reasoning is shown only to a terminal. It is distinguished by being dimmed,
	// and dimming is lost the moment the output is piped - at which point the
	// reasoning would read as part of the answer in a captured log.
	showReasoning := isTerminal(os.Stdout)

	a.SetHooks(func(delta llm.Delta) {
		switch {
		case delta.Reasoning != "":
			if showReasoning {
				fmt.Print(dim + delta.Reasoning + reset)
			}
		case delta.Text != "":
			fmt.Print(delta.Text)
		}
	}, nil)

	fmt.Println()

	started := time.Now()
	turn, runErr := a.Run(ctx, prompt)
	elapsed := time.Since(started)

	report(a, turn, elapsed)

	if runErr != nil {
		return runErr
	}

	return nil
}

// buildRegistry registers the tools in a fixed set. Ordering is applied later,
// because registration order is a load-time artifact and must not reach the
// wire.
func buildRegistry(ws *tools.Workspace) (*tools.Registry, error) {
	registry := tools.NewRegistry()

	for _, tool := range []tools.Tool{
		tools.NewReadTool(ws),
		tools.NewWriteTool(ws),
		tools.NewEditTool(ws),
		tools.NewGlobTool(ws),
		tools.NewGrepTool(ws),
		tools.NewWebFetchTool(),
		tools.NewBashTool(ws),
	} {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}

	return registry, nil
}

// report prints what the turn did, including the cache accounting that is the
// whole point of the design.
func report(a *agent.Agent, turn agent.TurnResult, elapsed time.Duration) {
	fmt.Println("─── transcript ───────────────────────────────────────────")
	for _, e := range a.Log().Events() {
		message, ok := e.Message()
		if !ok {
			continue
		}
		fmt.Printf("%-9s %s\n", e.Type, truncate(preview(message), 100))
	}

	fmt.Println()
	fmt.Println("─── result ───────────────────────────────────────────────")
	fmt.Printf("steps         : %d\n", turn.Steps)
	fmt.Printf("tool calls    : %d\n", turn.ToolCalls)
	fmt.Printf("elapsed       : %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("series        : %s\n", turn.Series)

	total := a.Meter().Totals()
	series := a.Meter().SeriesTotals()

	// The buckets are disjoint: uncached is uncached input only, and billed
	// input is the sum of the three.
	fmt.Printf("input  billed : %d tokens\n", total.BilledInputTokens())
	fmt.Printf("  cache read  : %d\n", total.CacheReadTokens)
	fmt.Printf("  cache write : %d\n", total.CacheWriteTokens)
	fmt.Printf("  uncached    : %d\n", total.UncachedInputTokens)
	fmt.Printf("output        : %d tokens\n", total.OutputTokens)
	fmt.Printf("hit rate      : %.1f%% overall\n", total.HitRate()*100)

	if series.BilledInputTokens() > 0 {
		fmt.Printf("                %.1f%% after the first request (cached=%d uncached=%d)\n",
			series.HitRate()*100, series.CacheReadTokens, series.UncachedInputTokens)
	}

	if strings.TrimSpace(turn.Text) != "" {
		fmt.Println()
		fmt.Println("─── final answer ─────────────────────────────────────────")
		fmt.Println(turn.Text)
	}
}

func preview(m session.Message) string {
	parts := make([]string, 0, len(m.Content))
	for _, b := range m.Content {
		switch b.Type {
		case session.BlockToolCall:
			parts = append(parts, fmt.Sprintf("[call %s %s]", b.Name, string(b.Arguments)))
		case session.BlockToolResult:
			parts = append(parts, "[result] "+b.Text)
		default:
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func normaliseWebSearch(mode string) string {
	if mode == "off" {
		return ""
	}

	return mode
}

func defaultOAuthStore() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "censi-cli-session.json")
	}

	return filepath.Join(home, ".censi", "cli-session.json")
}

// identity is deliberately plain and vendor-neutral. A deployment overrides it;
// the harness ships no opinion about which model it is talking to.
const identity = "You are a coding agent working inside a single workspace directory. " +
	"You can read and write files, search the workspace, and run shell commands. " +
	"Read a file before you change it. Prefer small, verifiable changes over large ones. " +
	"When a task is a request for a plan, produce a concrete plan and say what you would do first."

// guidance is the ordered set of static prompt sections.
func guidance() []assemble.Section {
	return []assemble.Section{
		{
			Name:  "tool:bash",
			Order: assemble.OrderGuidance,
			Text: "Use bash for commands that inspect the workspace. " +
				"Long output is truncated, so prefer targeted commands over dumping whole trees.",
		},
		{
			Name:  "tool:edit",
			Order: assemble.OrderGuidance + 10,
			Text: "Use edit for a targeted change and write for a new file. " +
				"An edit fails unless its old text appears exactly once, so include enough context.",
		},
	}
}
