import { randomUUID } from "node:crypto";
import process from "node:process";
import { createInterface, type Interface } from "node:readline";
import { Agent } from "./agent.ts";
import { defaultAuthStore, OAuthSession, type AccountContext, type StoredAccount } from "./auth.ts";
import { defaultModel, resolveThinkingLevel } from "./commands.ts";
import { type ApprovalAnswer, PermissionGuard, type Mode } from "./permission.ts";
import { Presentation, renderStartupBrand, StreamingMarkdown } from "./presentation.ts";
import { SessionLog, SteeringQueue } from "./session.ts";
import { GatewayTransport } from "./transport.ts";
import { createTools, Workspace } from "./tools.ts";
import type { AgentSettings, ModelCard, ToolResult } from "./types.ts";

const ansi = process.stdout.isTTY;
const ui = new Presentation(ansi);

function repaintSubmitted(text: string): void {
  if (ansi) process.stdout.write(`\x1b[1A\r\x1b[2K${ui.submittedPrompt(text)}\n\n`);
  else process.stdout.write(`${ui.submittedPrompt(text)}\n\n`);
}

interface Options {
  prompt?: string;
  workspace: string;
  model: string;
  baseUrl?: string;
  apiKey?: string;
  authUrl: string;
  oauthStore: string;
  oauthLogin: boolean;
  mode: Mode;
  webSearch: "off" | "auto" | "always";
  webSearchUses: number;
  thinking: AgentSettings["thinking"];
  maxTokens: number;
  maxSteps: number;
  demo: boolean;
}

class InputRouter {
  readonly rl: Interface;
  readonly steering = new SteeringQueue();
  #waiting: Array<(line: string | undefined) => void> = [];
  #queued: string[] = [];
  #approval?: (line: string) => void;
  #busyTyping = false;
  busy = false;
  onToggleMode?: () => void;
  onInterrupt?: () => void;
  onBusyTypingStart?: () => void;
  onBusyTypingEnd?: () => void;

  get isTyping(): boolean { return this.#busyTyping; }

  beginExternalOutput(): boolean {
    if (!this.#busyTyping || !process.stdout.isTTY) return false;
    process.stdout.write("\r\x1b[2K");
    return true;
  }

  restoreExternalInput(wasTyping: boolean): void {
    if (!wasTyping || !this.#busyTyping) return;
    this.rl.prompt(true);
  }

  constructor() {
    this.rl = createInterface({ input: process.stdin, output: process.stdout, terminal: process.stdin.isTTY });
    process.stdin.on("keypress", (_text, key: { name?: string; shift?: boolean; ctrl?: boolean; meta?: boolean }) => {
      if (key.name === "tab" && key.shift) this.onToggleMode?.();
      if (this.busy && !this.#approval && !this.#busyTyping && !key.ctrl && !key.meta
        && key.name !== "return" && key.name !== "enter" && key.name !== "tab") {
        this.#busyTyping = true;
        setImmediate(() => {
          this.onBusyTypingStart?.();
          this.rl.setPrompt(ui.brand("↳ "));
          this.rl.prompt(true);
        });
      }
    });
    this.rl.on("SIGINT", () => this.onInterrupt?.());
    this.rl.on("line", (line) => {
      if (line.trim()) repaintSubmitted(line);
      if (this.#approval) { const resolve = this.#approval; this.#approval = undefined; resolve(line); return; }
      if (this.busy) {
        this.#busyTyping = false;
        if (line.trim()) {
          if (line.trimStart().startsWith("/")) {
            this.#queued.push(line);
            process.stdout.write(`${ui.brand("  ↳ command queued")}  ${ui.tone("running after the current turn")}\n`);
          } else {
            this.steering.push(line);
            process.stdout.write(`${ui.brand("  ↳ direction queued")}  ${ui.tone("applying after the current operation")}\n`);
          }
        }
        this.onBusyTypingEnd?.();
        return;
      }
      const waiter = this.#waiting.shift();
      if (waiter) waiter(line); else this.#queued.push(line);
    });
    this.rl.on("close", () => this.#waiting.splice(0).forEach((resolve) => resolve(undefined)));
  }

  next(prompt = "> "): Promise<string | undefined> {
    const queued = this.#queued.shift();
    if (queued !== undefined) return Promise.resolve(queued);
    this.#busyTyping = false;
    this.rl.setPrompt(prompt);
    this.rl.prompt();
    return new Promise((resolve) => this.#waiting.push(resolve));
  }

  approval(prompt: string): Promise<string> {
    this.rl.setPrompt(prompt);
    this.rl.prompt();
    return new Promise((resolve) => { this.#approval = resolve; });
  }

  close(): void { this.rl.close(); }
}

async function main(): Promise<void> {
  const options = parseOptions(process.argv.slice(2));
  if (options.demo) { await renderDemo(options); return; }
  const workspace = await Workspace.open(options.workspace);
  const oauth = new OAuthSession(options.authUrl, options.oauthStore);
  const router = new InputRouter();
  const guard = new PermissionGuard(options.mode, async (tool, args, decision) => {
    clearStatusForApproval?.();
    process.stdout.write(`\n${ui.brand("  ⏸  approval needed")}  ${ui.tone("the agent is waiting for your decision")}\n\n`);
    process.stdout.write(`      ${ui.bold(tool)} ${ui.tone(JSON.stringify(args).slice(0, 500))}\n\n`);
    process.stdout.write(`      ${ui.bold("y")}  allow this once\n`);
    if (decision.oneTimeOnly) {
      process.stdout.write(`      ${ui.tone(`This high-impact command must be reviewed each time: ${decision.reason}`)}\n`);
    } else {
      process.stdout.write(`      ${ui.bold("a")}  always allow ${ui.code(decision.scope)} for this session\n`);
    }
    process.stdout.write(`      ${ui.bold("n")}  deny  ${ui.tone("(the agent is told why, and will plan around it)")}\n\n`);
    const answer = (await router.approval(ui.inputPrompt(guard.mode, "approval"))).trim().toLowerCase();
    restartStatusAfterApproval?.();
    if (answer === "a" || answer === "always" || answer === "all") return "always";
    if (["y", "yes", "allow", "ok"].includes(answer)) return "once";
    return "deny";
  });
  router.onToggleMode = () => {
    guard.mode = guard.mode === "plan" ? "act" : "plan";
    process.stdout.write(`\n${ui.modeNotice(guard.mode)}\n`);
  };

  if (!(await oauth.signedIn()) && options.oauthLogin) {
    try { await oauth.login(process.stderr); }
    catch (error) {
      if (!options.apiKey) throw error;
      process.stderr.write(`OAuth unavailable; using API key fallback: ${messageOf(error)}\n`);
    }
  }

  const transport = new GatewayTransport({
    baseUrl: options.baseUrl,
    apiKey: options.apiKey,
    authUrl: options.authUrl,
    oauth,
    sessionId: `cli-${randomUUID()}`,
  });
  let models = await safeModels(transport);
  const settings: AgentSettings = {
    model: defaultModel(options.model, models),
    thinking: options.thinking,
    webSearch: options.webSearch,
    webSearchUses: options.webSearchUses,
    maxTokens: options.maxTokens,
    maxSteps: options.maxSteps,
  };
  const tools = createTools(workspace);
  let delegationDepth = 0;
  tools.register({
    schema: {
      name: "delegate",
      description: "Delegate one bounded, self-contained investigation to a child agent and return only its final answer.",
      parameters: {
        type: "object",
        properties: { task: { type: "string", description: "A complete task with the desired output." } },
        required: ["task"],
        additionalProperties: false,
      },
    },
    readOnly: false,
    async execute(arguments_, signal) {
      const task = typeof arguments_.task === "string" ? arguments_.task.trim() : "";
      if (!task) return { text: "delegate: task is required", isError: true };
      if (delegationDepth >= 1) return { text: "delegate: nested delegation is not available", isError: true };
      delegationDepth++;
      try {
        const child = new Agent(new SessionLog(), transport, tools, guard, { ...settings, maxSteps: Math.min(settings.maxSteps, 40) });
        const result = await child.run(task, new SteeringQueue(), signal);
        return { text: result.text || "The child completed without a textual answer." };
      } catch (error) {
        return { text: `delegate: ${messageOf(error)}`, isError: true };
      } finally { delegationDepth--; }
    },
  });
  let streamed = false;
  let spinner: NodeJS.Timeout | undefined;
  let step = 0;
  let reasoningCharacters = 0;
  let reasoningStarted = 0;
  let streamRenderer = new StreamingMarkdown(ui);
  let streamNeedsLeadingBreak = false;
  let streamLineOpen = false;
  let partialFlushTimer: NodeJS.Timeout | undefined;
  const frames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
  let frame = 0;
  const clearStatus = (): void => {
    const wasSpinning = Boolean(spinner);
    if (spinner) clearInterval(spinner);
    spinner = undefined;
    if (ansi && wasSpinning) process.stdout.write("\r\x1b[2K");
  };
  const startStatus = (): void => {
    if (!ansi || spinner || router.isTyping) return;
    spinner = setInterval(() => {
      const thinking = reasoningCharacters > 0 ? `${ui.thought(`✻ thinking ~${Math.floor(reasoningCharacters / 4)} tokens`)} · ` : "";
      const mode = guard.mode === "plan" ? `${ui.bold(ui.brand("PLAN MODE"))} · shift+tab to exit` : `${guard.mode} · shift+tab`;
      process.stdout.write(`\r\x1b[2K${thinking}${ui.brand(frames[frame++ % frames.length])} step ${step}/${settings.maxSteps} · think:${settings.thinking} · ${mode} · ctrl+c`);
    }, 80);
  };
  let clearStatusForApproval: (() => void) | undefined = clearStatus;
  let restartStatusAfterApproval: (() => void) | undefined = startStatus;
  const writeStream = (rendered: string): void => {
    if (!rendered) return;
    const restoreInput = router.beginExternalOutput();
    if (streamNeedsLeadingBreak) {
      process.stdout.write("\n");
      streamNeedsLeadingBreak = false;
    }
    process.stdout.write(rendered);
    streamLineOpen = !rendered.endsWith("\n");
    if (restoreInput && streamLineOpen) {
      process.stdout.write("\n");
      streamLineOpen = false;
    }
    router.restoreExternalInput(restoreInput);
  };
  const cancelPartialFlush = (): void => {
    if (partialFlushTimer) clearTimeout(partialFlushTimer);
    partialFlushTimer = undefined;
  };
  const flushPartialStream = (): void => {
    cancelPartialFlush();
    writeStream(streamRenderer.flushPartial());
  };
  const schedulePartialFlush = (): void => {
    if (partialFlushTimer || !streamRenderer.hasPending) return;
    partialFlushTimer = setTimeout(flushPartialStream, 55);
  };
  router.onBusyTypingStart = () => {
    clearStatus();
    if (streamLineOpen) {
      process.stdout.write("\n");
      streamLineOpen = false;
    }
  };
  router.onBusyTypingEnd = startStatus;
  let streamStatusCleared = false;
  const agentHooks = {
    onStep(value) {
      step = value;
      streamStatusCleared = false;
      startStatus();
    },
    onDelta(delta) {
      if (delta.reasoning) {
        reasoningCharacters += [...delta.reasoning].length;
        reasoningStarted ||= Date.now();
      }
      if (!delta.text) return;
      if (!streamStatusCleared) {
        clearStatus();
        if (!streamed) streamNeedsLeadingBreak = true;
        streamStatusCleared = true;
        streamed = true;
      }
      const rendered = streamRenderer.feed(delta.text);
      if (rendered) {
        cancelPartialFlush();
        writeStream(rendered);
      }
      schedulePartialFlush();
    },
    onTool(name, args, result) {
      flushPartialStream();
      clearStatus();
      const restoreInput = router.beginExternalOutput();
      renderTool(name, args, result);
      router.restoreExternalInput(restoreInput);
      startStatus();
    },
    onSteer(text) {
      flushPartialStream();
      clearStatus();
      const restoreInput = router.beginExternalOutput();
      process.stdout.write(`\n${ui.brand("  ↳ applying")}  ${ui.bold(text)}\n`);
      router.restoreExternalInput(restoreInput);
      startStatus();
    },
  };
  let agent = new Agent(new SessionLog(), transport, tools, guard, settings, agentHooks);

  let activeAbort: AbortController | undefined;
  const interrupt = (): void => {
    if (activeAbort) { activeAbort.abort(new Error("Interrupted")); process.stdout.write("\n  interrupting…\n"); }
    else router.close();
  };
  router.onInterrupt = interrupt;
  process.on("SIGINT", interrupt);

  const startupAccount = await oauth.activeAccount();
  await renderStartupBrand(ui, { model: settings.model, thinking: settings.thinking, auth: accountLabel(startupAccount) });
  process.stdout.write(`${ui.tone(workspace.root)}\n\n`);
  process.stdout.write(`${ui.brand("Commands")}  ${ui.code("/account")} ${ui.code("/model")} ${ui.code("/think")} ${ui.code("/auto")} ${ui.code("/plan")} ${ui.code("/act")} ${ui.code("/web")} ${ui.code("/tools")} ${ui.code("/help")} ${ui.code("/exit")}\n`);
  process.stdout.write(`${ui.tone("Shift+Tab toggles plan mode · type during work to steer the next operation")}\n\n`);

  try {
    if (options.prompt) { repaintSubmitted(options.prompt); await runTurn(options.prompt); }
    else while (true) {
      const line = await router.next(ui.inputPrompt(guard.mode));
      if (line === undefined) break;
      const text = line.trim();
      if (!text) continue;
      if (text.startsWith("/")) {
        const keepGoing = await command(text);
        if (!keepGoing) break;
      } else await runTurn(text);
    }
  } finally { clearStatus(); router.close(); }

  async function runTurn(text: string): Promise<void> {
    activeAbort = new AbortController();
    router.busy = true;
    streamed = false;
    step = 0;
    reasoningCharacters = 0;
    reasoningStarted = 0;
    streamRenderer = new StreamingMarkdown(ui);
    streamStatusCleared = false;
    streamNeedsLeadingBreak = false;
    streamLineOpen = false;
    cancelPartialFlush();
    startStatus();
    try {
      const result = await agent.run(text, router.steering, activeAbort.signal);
      cancelPartialFlush();
      clearStatus();
      const restoreInput = router.beginExternalOutput();
      if (streamed) {
        const tail = streamRenderer.flush();
        if (tail && streamNeedsLeadingBreak) process.stdout.write("\n");
        process.stdout.write(tail);
        streamNeedsLeadingBreak = false;
      }
      else if (result.text) process.stdout.write(`\n${renderMarkdown(result.text)}`);
      if (result.usage.reasoningTokens > 0) {
        const seconds = reasoningStarted ? ((Date.now() - reasoningStarted) / 1_000).toFixed(1) : "0.0";
        process.stdout.write(`\n${ui.thought(`✻ thought for ${seconds}s · ${result.usage.reasoningTokens} tokens`)}\n`);
      }
      const billed = result.usage.cacheReadTokens + result.usage.cacheWriteTokens + result.usage.uncachedInputTokens;
      const hit = billed ? Math.round(result.usage.cacheReadTokens / billed * 100) : 0;
      process.stdout.write(`\n${ui.tone(`[${result.steps} step(s), ${result.toolCalls} tool call(s), cache hit ${hit}%]`)}\n\n`);
      router.restoreExternalInput(restoreInput);
    } catch (error) {
      cancelPartialFlush();
      clearStatus();
      const restoreInput = router.beginExternalOutput();
      if (activeAbort.signal.aborted) process.stdout.write(ui.tone("\nTurn stopped.\n\n"));
      else process.stderr.write(ui.error(`\n${messageOf(error)}\n\n`));
      router.restoreExternalInput(restoreInput);
    } finally { router.busy = false; activeAbort = undefined; }
  }

  async function command(text: string): Promise<boolean> {
    const [name, ...arguments_] = text.trim().split(/\s+/);
    const argument = arguments_[0] || "";
    switch (name) {
      case "/exit": case "/quit": case "/q": return false;
      case "/plan": guard.mode = "plan"; agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Plan mode is now active; inspect freely but refuse all workspace mutations." }], source: "user" }); process.stdout.write(`${ui.modeNotice("plan")}\n`); break;
      case "/act": guard.mode = "act"; agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Act mode is now active; workspace changes require approval." }], source: "user" }); process.stdout.write(`${ui.modeNotice("act")}\n`); break;
      case "/auto": guard.mode = "auto"; agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Audited auto mode is now active; safe work may proceed, but risky drift still requires approval." }], source: "user" }); process.stdout.write(`${ui.modeNotice("auto")}\n`); break;
      case "/think": {
        const requested = resolveThinkingLevel(arguments_);
        if (!requested) process.stdout.write(`${ui.brand("thinking")} ${settings.thinking} ${ui.tone("(minimal, low, medium, high, default)")}\n`);
        else { settings.thinking = requested as AgentSettings["thinking"]; process.stdout.write(`${ui.brand("thinking set")} ${ui.bold(requested)}\n`); }
        break;
      }
      case "/model": await selectModel(argument); break;
      case "/web": {
        if (!["off", "auto", "always"].includes(argument)) process.stdout.write(`${ui.brand("web search")} ${settings.webSearch} ${ui.tone("(off, auto, always)")}\n`);
        else { settings.webSearch = argument as AgentSettings["webSearch"]; process.stdout.write(`${ui.brand("web search set")} ${ui.bold(argument)}\n`); }
        break;
      }
      case "/tools": process.stdout.write(`${tools.names().join("\n")}\n`); break;
      case "/decisions": process.stdout.write(`${guard.decisions.map((decision) => `${decision.granted ? "allow" : "deny"} ${decision.tool}: ${decision.reason}`).join("\n") || "No decisions yet."}\n`); break;
      case "/account": await accountCommand(arguments_); break;
      case "/login": {
        const account = await oauth.login(process.stderr);
        await resetForAccount(account);
        process.stdout.write(`Connected to ${ui.bold(account.name)}.\n`);
        break;
      }
      case "/logout": await oauth.logout(); agent = new Agent(new SessionLog(), transport, tools, guard, settings, agentHooks); process.stdout.write("Signed out of all OAuth accounts. API key fallback remains available when configured.\n"); break;
      case "/help": help(); break;
      default: process.stdout.write(`Unknown command ${name}. Try /help.\n`);
    }
    return true;
  }

  async function accountCommand(arguments_: string[]): Promise<void> {
    const action = (arguments_[0] || "list").toLocaleLowerCase();
    try {
      if (["list", "ls", "status"].includes(action)) {
        const accounts = await oauth.accounts();
        renderAccounts(accounts);
        return;
      }
      if (action === "add" || action === "connect") {
        const account = await oauth.login(process.stderr);
        await resetForAccount(account);
        process.stdout.write(`${ui.success("Account connected")}  ${ui.bold(account.name)} ${ui.tone(account.type === "personal" ? "Personal" : "Work")}\n`);
        return;
      }
      if (action === "remove" || action === "disconnect") {
        const next = await oauth.removeAccount(arguments_.slice(1).join(" "));
        agent = new Agent(new SessionLog(), transport, tools, guard, settings, agentHooks);
        process.stdout.write(next
          ? `Account disconnected. Now using ${ui.bold(next.name)}; conversation context was cleared.\n`
          : "Account disconnected. No OAuth account remains.\n");
        return;
      }
      const account = await oauth.switchAccount(arguments_.join(" "));
      await resetForAccount(account);
      process.stdout.write(`${ui.success("Account switched")}  ${ui.bold(account.name)} ${ui.tone(`(${account.type === "personal" ? "Personal" : "Work"})`)}\n`);
      process.stdout.write(`${ui.tone("Conversation context was cleared so personal and work prompts cannot cross accounts.")}\n`);
    } catch (error) {
      process.stdout.write(`${ui.error(messageOf(error))}\n`);
    }
  }

  async function resetForAccount(_account: AccountContext): Promise<void> {
    // Clear history before any request under the newly selected credential.
    agent = new Agent(new SessionLog(), transport, tools, guard, settings, agentHooks);
    models = await transport.models();
    if (!models.some((model) => model.id === settings.model)) settings.model = defaultModel("", models);
  }

  async function selectModel(requested: string): Promise<void> {
    const available = models.length ? models : await transport.models();
    if (!requested) {
      process.stdout.write(`\n${ui.bold("  available models")}\n`);
      available.forEach((model, index) => process.stdout.write(`  ${model.id === settings.model ? ui.brand("*") : " "} ${index + 1}. ${ui.bold(model.id)}${model.description ? ui.tone(`  ${model.description}`) : ""}\n`));
      requested = (await router.next(ui.inputPrompt(guard.mode, "model")) || "").trim();
    }
    const number = Number(requested);
    const selected = Number.isInteger(number) && number > 0 ? available[number - 1] : available.find((model) => model.id === requested);
    if (!selected) { process.stdout.write(`Unknown or disabled model ${JSON.stringify(requested)}.\n`); return; }
    settings.model = selected.id;
    process.stdout.write(`Model switched to ${selected.id}.\n`);
  }
}

async function safeModels(transport: GatewayTransport): Promise<ModelCard[]> {
  try { return await transport.models(); }
  catch (error) {
    if (!process.env.MAXLABS_MODEL && !process.env.HARNESS_MODEL && !process.argv.some((arg) => arg === "--model" || arg.startsWith("--model="))) throw error;
    return [];
  }
}

function help(): void {
  process.stdout.write(`
  /plan       inspect and plan; refuse mutations
  /act        ask before state-changing operations
  /auto       run audited safe work; still catch destructive drift
  /model ID   list or switch enabled Gateway models
  /think LEVEL  minimal, low, medium, high, or default
  /web MODE   off, auto, or always
  /tools      list available tools
  /decisions  show permission decisions
  /account     list personal and work accounts
  /account N   switch to a connected account (also accepts its name or ID)
  /account add connect another account through browser consent
  /account remove [N|ID]  forget one saved account
  /login      connect OAuth
  /logout     clear every saved OAuth account
  /exit       leave MaxLabs CLI

  While a turn is running, type a new direction and press Enter. It is applied
  after the current model or tool operation reaches a safe boundary.

`);
}

function accountLabel(account?: AccountContext): string {
  if (!account) return "oauth-first";
  return `${account.type === "personal" ? "Personal" : account.name} · oauth`;
}

function renderAccounts(accounts: StoredAccount[]): void {
  process.stdout.write(`\n${ui.bold("  accounts")}\n`);
  if (!accounts.length) {
    process.stdout.write(`  ${ui.tone("No OAuth accounts are available. Use /account add to connect one.")}\n\n`);
    return;
  }
  accounts.forEach((account, index) => {
    const marker = account.active ? ui.brand("*") : " ";
    const kind = account.type === "personal" ? "Personal" : "Work";
    const state = account.connected ? (account.active ? "active" : "connected") : "not connected";
    const tier = account.tier?.name ? ` · ${account.tier.name}` : "";
    process.stdout.write(`  ${marker} ${index + 1}. ${ui.bold(account.name)}  ${ui.tone(`${kind} · ${state}${tier}`)}\n`);
  });
  process.stdout.write(`\n  ${ui.tone("Switch: /account 2 · Connect another: /account add")}\n\n`);
}

function renderTool(name: string, args: Record<string, unknown>, result: ToolResult): void {
  const target = typeof args.path === "string" ? ` ${args.path}` : name === "bash" ? ` ${String(args.command || "").slice(0, 100)}` : "";
  const icons: Record<string, string> = { read: "◇", write: "✎", edit: "✎", glob: "⌕", grep: "⌕", bash: "$", webfetch: "◎", delegate: "↗" };
  process.stdout.write(`\n  ${ui.brand(icons[name] || "•")} ${ui.bold(name)}${ui.tone(target)}\n`);
  if (result.diff) for (const line of result.diff.split("\n")) process.stdout.write(`${line.startsWith("+") ? ui.success(`    ${line}`) : ui.error(`    ${line}`)}\n`);
  if (result.isError) process.stdout.write(`${ui.error(`    ${result.text.split("\n")[0]}`)}\n`);
}

function renderMarkdown(text: string): string {
  const renderer = new StreamingMarkdown(ui);
  return `${renderer.feed(`${text}\n`)}${renderer.flush()}`;
}

async function renderDemo(options: Options): Promise<void> {
  process.stdout.write("\n");
  await renderStartupBrand(ui, { model: "worker", thinking: "medium", auth: "oauth-first" });
  process.stdout.write(`${ui.tone(options.workspace)}\n\n`);
  process.stdout.write(`${ui.brand("Commands")}  ${ui.code("/account")} ${ui.code("/model")} ${ui.code("/think")} ${ui.code("/auto")} ${ui.code("/plan")} ${ui.code("/act")} ${ui.code("/web")} ${ui.code("/tools")} ${ui.code("/help")} ${ui.code("/exit")}\n`);
  process.stdout.write(`${ui.tone("Shift+Tab toggles plan mode · type during work to steer the next operation")}\n\n`);
  process.stdout.write(`${ui.submittedPrompt("Review this project and run the narrow tests")}\n\n`);
  process.stdout.write(`${ui.thought("✻ thinking ~120 tokens")}\n`);
  process.stdout.write(`${ui.brand("⠹")} step 2/100 · think:medium · auto · shift+tab · ctrl+c\n`);
  renderTool("read", { path: "package.json" }, { text: "read" });
  renderTool("bash", { command: "npm test" }, { text: "Command exited 0." });
  process.stdout.write(`${ui.success("\nAll focused tests pass. The package is ready for a local npm-link preview.")}\n`);
  process.stdout.write(`${ui.tone("[2 step(s), 2 tool call(s), cache hit 86%]")}\n\n`);
  process.stdout.write(`${ui.brand("Tip:")} ${ui.tone("type ‘focus on OAuth persistence’ and press Enter to steer running work.")}\n\n`);
}

function parseOptions(args: string[]): Options {
  const value = (name: string, fallback = ""): string => {
    const direct = args.find((arg) => arg.startsWith(`--${name}=`));
    if (direct) return direct.slice(name.length + 3);
    const index = args.indexOf(`--${name}`);
    return index >= 0 ? args[index + 1] || fallback : fallback;
  };
  const flag = (name: string): boolean => args.includes(`--${name}`);
  const webSearch = value("web-search", "off");
  const thinking = value("thinking", "medium");
  if (!["off", "auto", "always"].includes(webSearch)) throw new Error("--web-search must be off, auto, or always");
  if (!["minimal", "low", "medium", "high", "default"].includes(thinking)) throw new Error("--thinking must be minimal, low, medium, high, or default");
  return {
    prompt: value("prompt") || undefined,
    workspace: value("workspace", process.cwd()),
    model: value("model", process.env.MAXLABS_MODEL || process.env.HARNESS_MODEL || ""),
    baseUrl: value("base-url", process.env.MAXLABS_BASE_URL || process.env.HARNESS_BASE_URL || "https://api.maxlabs.cenmax.in/v1") || undefined,
    apiKey: value("api-key", process.env.MAXLABS_API_KEY || process.env.HARNESS_API_KEY || "") || undefined,
    authUrl: value("auth-url", process.env.MAXLABS_AUTH_URL || process.env.HARNESS_AUTH_URL || "https://console.maxlabs.cenmax.in").replace(/\/$/, ""),
    oauthStore: value("oauth-store", defaultAuthStore()),
    oauthLogin: !flag("no-oauth-login"),
    mode: flag("auto") ? "auto" : "act",
    webSearch: webSearch as Options["webSearch"],
    webSearchUses: Number(value("web-search-uses", "10")),
    thinking: thinking as AgentSettings["thinking"],
    maxTokens: Number(value("max-tokens", "4096")),
    maxSteps: Number(value("max-steps", "100")),
    demo: flag("demo"),
  };
}

function messageOf(error: unknown): string { return error instanceof Error ? error.message : String(error); }

main().catch((error) => { process.stderr.write(`error: ${messageOf(error)}\n`); process.exitCode = 1; });
