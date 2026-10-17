import { randomUUID } from "node:crypto";
import process from "node:process";
import { dirname } from 'node:path';
import { createInterface,                } from "node:readline";
import { Agent, addUsage, systemPrompt } from "./agent.js";
import { defaultAuthStore, OAuthSession,                                         } from "./auth.js";
import { defaultModel, resolveThinkingLevel } from "./commands.js";
import {                      PermissionGuard,           } from "./permission.js";
import { Presentation, renderStartupBrand, StreamingMarkdown } from "./presentation.js";
import { SessionLog, SteeringQueue } from "./session.js";
import { GatewayTransport, ServiceError } from "./transport.js";
import { QuotaCheckpointStore, quotaIsAvailable } from './quota-pause.js';
import { createTools, Workspace } from "./tools.js";
import { TeamStore, runApprovedTasks, environmentCommands, allowedCheck, createTeamChild, teamInstructions, teamTool } from "./greenfield.js";
import { TeamDirectionReview, teamDirectionPrompt, TeamExecutionReview, teamExecutionPrompt } from "./team-steering.js";
import { referenceNamed } from "./team-reference.js";
import { TeamPreview } from "./team-preview.js";
import { projectAutoCommand } from "./team-permissions.js";
import { teamStatusText, teamArtifactText, teamNext } from "./team-view.js";
import { emptyUsage } from "./types.js";


const ansi = process.stdout.isTTY;
const ui = new Presentation(ansi);

function repaintSubmitted(text        )       {
  if (ansi) process.stdout.write(`\x1b[1A\r\x1b[2K${ui.submittedPrompt(text)}\n\n`);
  else process.stdout.write(`${ui.submittedPrompt(text)}\n\n`);
}





















class InputRouter {
           rl           ;
           steering = new SteeringQueue();
  #waiting                                            = [];
  #queued           = [];
  #approval                         ;
  #busyTyping = false;
  busy = false;
  onToggleMode             ;
  onInterrupt             ;
  onBusyTypingStart             ;
  onBusyTypingEnd             ;

  get isTyping()          { return this.#busyTyping; }

  beginExternalOutput()          {
    if (!this.#busyTyping || !process.stdout.isTTY) return false;
    process.stdout.write("\r\x1b[2K");
    return true;
  }

  restoreExternalInput(wasTyping         )       {
    if (!wasTyping || !this.#busyTyping) return;
    this.rl.prompt(true);
  }

  constructor() {
    this.rl = createInterface({ input: process.stdin, output: process.stdout, terminal: process.stdin.isTTY });
    process.stdin.on("keypress", (_text, key                                                                    ) => {
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
          if (/^\/team\s+approve\b/.test(line.trim())) {
            process.stdout.write("  Wait for the current turn to finish, inspect its artifacts, then approve the revision.\n");
          } else if (line.trimStart().startsWith("/")) {
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

  next(prompt = "> ")                              {
    const queued = this.#queued.shift();
    if (queued !== undefined) return Promise.resolve(queued);
    this.#busyTyping = false;
    this.rl.setPrompt(prompt);
    this.rl.prompt();
    return new Promise((resolve) => this.#waiting.push(resolve));
  }

  approval(prompt        )                  {
    this.rl.setPrompt(prompt);
    this.rl.prompt();
    return new Promise((resolve) => { this.#approval = resolve; });
  }

  close()       { this.rl.close(); }
}

async function main()                {
  const options = parseOptions(process.argv.slice(2));
  if (options.demo) { await renderDemo(options); return; }
  const workspace = await Workspace.open(options.workspace);
  const teamStore = new TeamStore(workspace);
  const teamPreview = new TeamPreview(teamStore);
  let teamMode = options.team;
  let teamWorkerModel = options.workerModel;
  const oauth = new OAuthSession(options.authUrl, options.oauthStore);
  const quotaCheckpoints=new QuotaCheckpointStore(dirname(options.oauthStore),workspace.root);
  let quotaResuming=false;
  const router = new InputRouter();
  const guard = new PermissionGuard(options.team ? "auto" : options.mode, async (tool, args, decision) => {
    if (teamMode && tool === "webfetch") return "once";
    clearStatusForApproval?.();
    process.stdout.write(`\n${ui.brand("  ⏸  approval needed")}  ${ui.tone("the agent is waiting for your decision")}\n\n`);
    process.stdout.write(`      ${ui.bold(tool)} ${ui.tone(JSON.stringify(args).slice(0, 500))}\n\n`);
    process.stdout.write(`      ${ui.bold("y")}  allow this once\n`);
    if (decision.oneTimeOnly) {
      process.stdout.write(`      ${ui.tone(`Review required: ${decision.reason}`)}\n`);
    } else {
      process.stdout.write(`      ${ui.bold("a")}  always allow ${ui.code(decision.scope)} for this session\n`);
    }
    process.stdout.write(`      ${ui.bold("n")}  deny  ${ui.tone("(the agent is told why, and will plan around it)")}\n\n`);
    const answer = (await router.approval(ui.inputPrompt(guard.mode, "approval"))).trim().toLowerCase();
    restartStatusAfterApproval?.();
    if (answer === "a" || answer === "always" || answer === "all") return "always";
    if (["y", "yes", "allow", "ok"].includes(answer)) return "once";
    return "deny";
  }, command => projectAutoCommand(workspace.root, command));
  router.onToggleMode = () => {
    teamMode = false;
    guard.mode = guard.mode === "plan" ? "act" : "plan";
    process.stdout.write(`\n${ui.modeNotice(guard.mode)}\n`);
  };

  if (options.oauthLogin) {
    const hadSavedSession = await oauth.signedIn();
    try {
      const token = hadSavedSession ? await oauth.accessToken() : undefined;
      if (!token) {
        if (hadSavedSession) process.stderr.write("Your saved OAuth session has expired. Sign in again to continue.\n");
        await oauth.login(process.stderr);
      }
    }
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
  const settings                = {
    model: defaultModel(options.model, models),
    thinking: options.thinking,
    webSearch: options.webSearch,
    webSearchUses: options.webSearchUses,
    maxTokens: options.maxTokens,
    maxSteps: options.maxSteps,
  };
  const tools = createTools(workspace);
  let childUsage = emptyUsage();
  const workflow = teamTool(teamStore, createTeamChild(teamStore, transport, guard, settings, () => teamWorkerModel, (name, args, result) => agentHooks.onTool(name, args, result), usage => { childUsage=addUsage(childUsage,usage); }), async (command, signal) => {
    if (guard.mode === "plan" && !environmentCommands.includes(command)) return { text: "Plan mode refuses task execution", isError: true };
    const tool = tools.get("bash") ;
    const args = { command, timeout_ms: 300_000 };
    const denied = allowedCheck(command) || environmentCommands.includes(command) ? undefined : await guard.permit(tool, args);
    return denied ? { text: denied, isError: true } : tool.execute(args, signal);
  }, path => agent.log.events().some(e => e.message.role === "user" && e.message.source === "user" && e.message.content.some(b => b.type === "text" && referenceNamed(path, b.text))));
  tools.register(workflow);
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
  let spinner                            ;
  let step = 0;
  let reasoningCharacters = 0;
  let reasoningStarted = 0;
  let streamRenderer = new StreamingMarkdown(ui);
  let streamNeedsLeadingBreak = false;
  let streamLineOpen = false;
  let partialFlushTimer                            ;
  const frames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
  let frame = 0;
  const clearStatus = ()       => {
    const wasSpinning = Boolean(spinner);
    if (spinner) clearInterval(spinner);
    spinner = undefined;
    if (ansi && wasSpinning) process.stdout.write("\r\x1b[2K");
  };
  const startStatus = ()       => {
    if (!ansi || spinner || router.isTyping) return;
    spinner = setInterval(() => {
      const thinking = reasoningCharacters > 0 ? `${ui.thought(`✻ thinking ~${Math.floor(reasoningCharacters / 4)} tokens`)} · ` : "";
      const mode = teamMode ? "Greenfield SDE Team · BETA" : guard.mode === "plan" ? `${ui.bold(ui.brand("PLAN MODE"))} · shift+tab to exit` : `${guard.mode} · shift+tab`;
      process.stdout.write(`\r\x1b[2K${thinking}${ui.brand(frames[frame++ % frames.length])} step ${step}/${settings.maxSteps} · think:${settings.thinking} · ${mode} · ctrl+c`);
    }, 80);
  };
  let clearStatusForApproval                           = clearStatus;
  let restartStatusAfterApproval                           = startStatus;
  const writeStream = (rendered        )       => {
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
  const cancelPartialFlush = ()       => {
    if (partialFlushTimer) clearTimeout(partialFlushTimer);
    partialFlushTimer = undefined;
  };
  const flushPartialStream = ()       => {
    cancelPartialFlush();
    writeStream(streamRenderer.flushPartial());
  };
  const schedulePartialFlush = ()       => {
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
  let plannerFetches = 0;
  const agentHooks = {
    toolResultLimit: () => teamMode ? 16_000 : 0,
    systemInstructions: () => teamMode ? `\n\n${teamInstructions}` : "",
    visibleTool: (name        ) => teamMode ? !["write", "edit", "bash", "delegate"].includes(name) : name !== "team",
    beforeTool(name        ) {
      if (teamMode && name === "webfetch" && ++plannerFetches > 6) return "Documentation budget reached (6 fetches per turn). Use saved evidence or record unknowns.";
      if (!teamMode && name === "team") return "Enable Greenfield SDE Team (Beta) with /team first.";
      if (teamMode && ["write", "edit", "bash", "delegate"].includes(name)) return "Greenfield planning uses team save/research/run; direct application mutation and unscoped delegation are disabled.";
      return undefined;
    },
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
      if (!teamMode) renderTool(name, args, result);
      else if (result.isError) process.stdout.write(ui.error(`  ! ${name}: ${result.text.slice(0, 1200)}\n`));
      else if (["write", "edit"].includes(name)) process.stdout.write(ui.tone(`  Updated ${args.path || "file"}\n`));
      else if (name === "team" && ["save", "research", "run", "attach_reference"].includes(String(args.action))) process.stdout.write(ui.tone(`  ${args.action === "save" ? `Saved ${args.kind}: ${args.id}` : args.action === "research" ? "Research saved" : args.action === "attach_reference" ? "Design reference added" : `Verified ${args.id}`}\n`));
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

  let activeAbort                             ;
  const interrupt = ()       => {
    if (activeAbort) { activeAbort.abort(new Error("Interrupted")); process.stdout.write("\n  interrupting…\n"); }
    else router.close();
  };
  router.onInterrupt = interrupt;
  process.on("SIGINT", interrupt);

  const startupAccount = await oauth.activeAccount();
  const savedQuotaPause = await quotaCheckpoints.load(workspace.root,startupAccount?.id||'default').catch(()=>undefined);
  await renderStartupBrand(ui, { model: settings.model, thinking: settings.thinking, auth: accountLabel(startupAccount) });
  process.stdout.write(`${ui.tone(workspace.root)}\n\n`);
  process.stdout.write(`${ui.brand("Commands")}  ${ui.code("/account")} ${ui.code("/model")} ${ui.code("/think")} ${ui.code("/auto")} ${ui.code("/plan")} ${ui.code("/act")} ${ui.code("/team")} ${ui.tone("Greenfield SDE Team · Beta")} ${ui.code("/web")} ${ui.code("/help")} ${ui.code("/exit")}\n`);
  if (teamMode) process.stdout.write(teamStatusText(await teamStore.status()) + "\n\n");
  process.stdout.write(`${ui.tone("Shift+Tab toggles plan mode · type during work to steer the next operation")}\n\n`);
  if(savedQuotaPause)process.stdout.write(ui.tone('Quota pause saved for this workspace. /resume checks allowance and continues preserved work.\n\n'));

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
  } finally { clearStatus(); router.close(); teamPreview.close(); }

  async function runTurn(text        , preserveTeamBudget = false)                {
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
      childUsage = emptyUsage();
      if (!preserveTeamBudget) { workflow.resetBudget(); plannerFetches = 0; }
      if (!teamMode && agent.log.events().some(e => e.message.content.some(b => b.type === "text" && b.text.includes("[team:instructions:v1]")))) text = "[Greenfield SDE Team is disabled for this turn. Follow ordinary CLI mode and tools.]\n\n" + text;
      if (teamMode) {
        agent.log.ensureSystem(systemPrompt(guard.mode));
        if (!agent.log.events().some(e => e.message.content.some(b => b.type === "text" && b.text.includes("[team:instructions:v1]")))) {
          agent.log.append("steering", { role: "user", content: [{ type: "text", text: "[team:instructions:v1] Greenfield workflow instructions are in the system policy for this mode." }], source: "user" });
        }
        const status = await teamStore.status();
        text = `[Greenfield SDE Team (Beta): stage=${status.stage}, revision=${status.revision}. Use team status for artifacts and readiness. Pending approvals are user-owned.]\n\n${text}`;
      }
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
      const combined = addUsage(result.usage, childUsage);
      const billed = combined.cacheReadTokens + combined.cacheWriteTokens + combined.uncachedInputTokens;
      const hit = billed ? Math.round(combined.cacheReadTokens / billed * 100) : 0;
      process.stdout.write(`\n${ui.tone(`[${result.steps} step(s), ${result.toolCalls} tool call(s), cache hit ${hit}%]`)}\n\n`);
      if (teamMode) process.stdout.write(ui.tone(teamNext(await teamStore.status())) + "\n\n");
      if(quotaResuming){await quotaCheckpoints.clear();quotaResuming=false;}
      router.restoreExternalInput(restoreInput);
    } catch (error) {
      cancelPartialFlush();
      clearStatus();
      const restoreInput = router.beginExternalOutput();
      if (activeAbort.signal.aborted) process.stdout.write(ui.tone("\nTurn stopped.\n\n"));
      else if(!await saveQuotaPause(error,false))process.stderr.write(ui.error(`\n${messageOf(error)}\n\n`));
      router.restoreExternalInput(restoreInput);
    } finally { router.busy = false; activeAbort = undefined; }
  }

  async function executeApproved()                {
    childUsage = emptyUsage();
    let reviewPrompt                 ;
    router.busy = true; activeAbort = new AbortController();
    try { process.stdout.write((await runApprovedTasks(teamStore, workflow, activeAbort.signal, () => { const directions = router.steering.drain(); if (directions.length) { for (const text of directions) agent.log.append("steering", { role: "user", content: [{ type: "text", text }], source: "user" }); throw new TeamDirectionReview(directions); } }, event => { clearStatus(); process.stdout.write(`${event.status === "passed" ? ui.success("✓") : ui.brand("→")} ${event.completed}/${event.total} · ${event.summary}\n`); })) + "\n"); }
    catch(error) { if(error instanceof TeamDirectionReview)reviewPrompt=teamDirectionPrompt(error.directions);else if(error instanceof TeamExecutionReview)reviewPrompt=teamExecutionPrompt(error.summary);else if(!await saveQuotaPause(error,true))throw error; }
    finally { clearStatus(); router.busy = false; activeAbort = undefined; process.stdout.write(teamStatusText(await teamStore.status()) + "\n"); const input=childUsage.uncachedInputTokens+childUsage.cacheReadTokens+childUsage.cacheWriteTokens; if(input)process.stdout.write(ui.tone(`[${input.toLocaleString()} input tokens · ${childUsage.outputTokens.toLocaleString()} output tokens · cache hit ${Math.round(childUsage.cacheReadTokens/input*100)}%]`)+"\n"); }
    if(reviewPrompt)await runTurn(reviewPrompt,true);
    else if(quotaResuming){const status=await teamStore.status();if(!Object.values(status.runs).some(run=>run.status==='paused')){await quotaCheckpoints.clear();quotaResuming=false;}}
  }

  async function saveQuotaPause(error        ,teamExecute        )                 {
    if(!(error instanceof ServiceError)||!error.resumable)return false;
    await quotaCheckpoints.save({version:1,workspace:workspace.root,accountId:(await oauth.activeAccount())?.id||'default',sessionId:transport.config.sessionId,events:agent.log.events(),settings:{...agent.settings},mode:guard.mode,teamMode,teamExecute,workerModel:teamWorkerModel,window:error.window,resetsAt:error.resetsAt});
    quotaResuming=true;
    process.stdout.write(ui.tone(`\nPaused · usage limit. Progress and conversation saved. ${error.resetsAt?'Resets at '+error.resetsAt+'. ':''}Use /resume when allowance is available.\n\n`));
    return true;
  }

  async function command(text        )                   {
    const [name, ...arguments_] = text.trim().split(/\s+/);
    const argument = arguments_[0] || "";
    switch (name) {
      case '/resume': {
        try {
          const saved=await quotaCheckpoints.load(workspace.root,(await oauth.activeAccount())?.id||'default');
          if(!saved){process.stdout.write('No quota pause is saved for this workspace.\n');return true;}
          if(guard.mode==='plan'&&saved.mode!=='plan')throw new Error('Switch out of Plan mode before resuming execution. The saved pause is preserved.');
          const token=await oauth.accessToken();
          const response=await fetch(token?options.authUrl.replace(/\/$/,'')+'/app/v1/usage/windows':String(options.baseUrl).replace(/\/$/,'')+'/usage/me',{headers:{accept:'application/json',authorization:'Bearer '+(token||options.apiKey||'')}});
          if(!response.ok)throw new Error('Could not check allowance. The saved pause is preserved.');
          const usage=await response.json()                                                                                     ;
          if(!Array.isArray(usage.windows))throw new Error('Allowance response is unavailable. The saved pause is preserved.');
          if(!quotaIsAvailable(usage.windows)){process.stdout.write('Quota is still exhausted or unavailable. Progress is saved; try /resume after reset or a top-up.\n');return true;}
          if(!models.some(model=>model.id===saved.settings.model))saved.settings.model=agent.settings.model;
          if(saved.workerModel&&!models.some(model=>model.id===saved.workerModel))saved.workerModel=saved.settings.model;
          agent.log.restore(saved.events);Object.assign(agent.settings,saved.settings);transport.config.sessionId=saved.sessionId;guard.mode=saved.mode;teamMode=saved.teamMode;teamWorkerModel=saved.workerModel;quotaResuming=true;workflow.resetBudget();plannerFetches=0;
          if(saved.teamExecute)await executeApproved();else await runTurn('Resume from the last completed action. Use saved tool results and current files; preserve completed work and finish the outstanding plan.',true);
        }catch(error){process.stderr.write(ui.error(messageOf(error)+'\n'));}
        return true;
      }
      case "/team": {
        try {
          const action = argument || "on";
          if (action === "off") { teamMode = false; agent.log.ensureSystem(systemPrompt(guard.mode)); agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Greenfield SDE Team workflow is disabled. Follow ordinary CLI instructions and permission mode until explicitly re-enabled." }], source: "user" }); process.stdout.write("Greenfield SDE Team disabled. Artifacts are preserved.\n"); }
          else if (action === "on") { teamMode = true; guard.mode = "auto"; process.stdout.write("Greenfield SDE Team · Beta\nDescribe your idea. We’ll refine the scope and design together.\nShare an existing design in chat whenever useful. /team status shows the next step.\n"); }
          else if (action === "mode") { const status = await teamStore.setExecutionMode(arguments_[1]); process.stdout.write(`Execution: ${status.execution_mode}. Review and reapprove the plan before running workers.\n`); }
          else if (action === "workspace") { const status = await teamStore.status(); process.stdout.write(status.execution ? `Combined build: ${await teamStore.path(status.execution.path)}\nBranch: ${status.execution.branch}\nEach worker uses its own branch. Source applies on release acceptance.\n` : "No build workspace yet. Approve a plan to begin.\n"); }
          else if (action === "reference") { const status = await teamStore.attach(arguments_.slice(1).join(" ")); process.stdout.write("Design reference added. You can ask the team to use it now.\n"); }
          else if (action === "approve") { const status = await teamStore.status(); const gate = arguments_[1]; const approved = await teamStore.approve(gate, status.gates[gate]); process.stdout.write(`Approved ${gate}. Stage: ${approved.stage}.\n`); if (gate !== "release") { teamMode = true; guard.mode = "auto"; if (gate === "spec") await executeApproved(); else await runTurn(`Continue Greenfield SDE Team from the approved ${gate}. Use team status and follow the workflow.`); } }
          else if (action === "run") { teamMode = true; guard.mode = "auto"; await executeApproved(); }
          else if (action === "status") { const status = await teamStore.status(); process.stdout.write((arguments_.includes("--json") ? JSON.stringify(status, null, 2) : teamStatusText(status)) + "\n"); }
          else if (action === "show") { const status = await teamStore.status(); const id = arguments_[1] === "--json" ? "" : arguments_[1]; const artifacts = id ? status.artifacts.filter(a => a.id === id) : status.artifacts.filter(a => ["brief", "spec", "task", "release"].includes(a.kind)); if (!artifacts.length) throw new Error(id ? "Unknown artifact ID" : "No plan saved yet. Continue discovery."); for (const a of artifacts) { const content = await teamStore.content(a); delete content.prototype_html; const run = status.runs[a.id]; process.stdout.write(`\n${ui.bold(a.kind + " · " + a.id)}\n` + (arguments_.includes("--json") ? JSON.stringify({ ...content, ...(run ? { execution: run } : {}) }, null, 2) : teamArtifactText({ ...content, ...(run ? { execution: run } : {}) })) + "\n"); } }
          else if (action === "preview") { const preview = await teamPreview.show(); process.stdout.write(`${preview.opened ? "Design opened in your browser" : "Open this design in your browser"}:\n${preview.url}\nKeep this CLI session open while previewing.\n`); }
          else if (action === "model") { const id = arguments_[1] || ""; if (id && !models.some(m => m.id === id)) throw new Error("Choose an enabled model from /model"); teamWorkerModel = id; process.stdout.write(`Worker model: ${id || "same as planner"}\n`); }
          else process.stdout.write("/team [on|off|mode flexible|scoped|workspace|preview|run|status [--json]|show [ID] [--json]|reference PATH|approve design|spec|release]\n");
        } catch (e) { process.stdout.write(`${ui.error(messageOf(e))}\n`); }
        break;
      }
      case "/exit": case "/quit": case "/q": return false;
      case "/plan": teamMode = false; guard.mode = "plan"; agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Plan mode is now active; inspect freely but refuse all workspace mutations." }], source: "user" }); process.stdout.write(`${ui.modeNotice("plan")}\n`); break;
      case "/act": teamMode = false; guard.mode = "act"; agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Act mode is now active; workspace changes require approval." }], source: "user" }); process.stdout.write(`${ui.modeNotice("act")}\n`); break;
      case "/auto": teamMode = false; guard.mode = "auto"; agent.log.append("steering", { role: "user", content: [{ type: "text", text: "Audited auto mode is now active; safe work may proceed, but risky drift still requires approval." }], source: "user" }); process.stdout.write(`${ui.modeNotice("auto")}\n`); break;
      case "/think": {
        const requested = resolveThinkingLevel(arguments_);
        if (!requested) process.stdout.write(`${ui.brand("thinking")} ${settings.thinking} ${ui.tone("(minimal, low, medium, high, default)")}\n`);
        else { settings.thinking = requested                             ; process.stdout.write(`${ui.brand("thinking set")} ${ui.bold(requested)}\n`); }
        break;
      }
      case "/model": await selectModel(argument); break;
      case "/web": {
        if (!["off", "auto", "always"].includes(argument)) process.stdout.write(`${ui.brand("web search")} ${settings.webSearch} ${ui.tone("(off, auto, always)")}\n`);
        else { settings.webSearch = argument                              ; process.stdout.write(`${ui.brand("web search set")} ${ui.bold(argument)}\n`); }
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

  async function accountCommand(arguments_          )                {
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

  async function resetForAccount(_account                )                {
    // Clear history before any request under the newly selected credential.
    agent = new Agent(new SessionLog(), transport, tools, guard, settings, agentHooks);
    models = await transport.models();
    if (!models.some((model) => model.id === settings.model)) settings.model = defaultModel("", models);
  }

  async function selectModel(requested        )                {
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

async function safeModels(transport                  )                       {
  try { return await transport.models(); }
  catch (error) {
    if (!process.env.MAXLABS_MODEL && !process.env.HARNESS_MODEL && !process.argv.some((arg) => arg === "--model" || arg.startsWith("--model="))) throw error;
    return [];
  }
}

function help()       {
  process.stdout.write(`
  /plan       inspect and plan; refuse mutations
  /act        ask before state-changing operations
  /auto       run audited safe work; still catch destructive drift
  /resume     continue a saved quota pause when allowance is available
  /team       Greenfield SDE Team (Beta), idea through verified delivery
  /team mode flexible|scoped  choose worker autonomy (reapproval required)
  /team workspace  show the combined build folder and branch
  /team status  inspect stages, artifacts, task results and readiness errors
  /team show ID  inspect a saved artifact before approval
  /team preview  export the current interactive design preview
  /team model ID  select an enabled model for implementation workers
  /team reference PATH  attach a read-only design reference directory
  /team approve design|spec|release  accept the current artifact revision
  /team off   leave the workflow; preserve its artifacts
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

function accountLabel(account                 )         {
  if (!account) return "oauth-first";
  return `${account.type === "personal" ? "Personal" : account.name} · oauth`;
}

function renderAccounts(accounts                 )       {
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

function renderTool(name        , args                         , result            )       {
  const target = typeof args.path === "string" ? ` ${args.path}` : name === "bash" ? ` ${String(args.command || "").slice(0, 100)}` : "";
  const icons                         = { read: "◇", write: "✎", edit: "✎", glob: "⌕", grep: "⌕", bash: "$", webfetch: "◎", delegate: "↗" };
  process.stdout.write(`\n  ${ui.brand(icons[name] || "•")} ${ui.bold(name)}${ui.tone(target)}\n`);
  if (result.diff) for (const line of result.diff.split("\n")) process.stdout.write(`${line.startsWith("+") ? ui.success(`    ${line}`) : ui.error(`    ${line}`)}\n`);
  if (result.isError) process.stdout.write(`${ui.error(`    ${result.text.split("\n")[0]}`)}\n`);
}

function renderMarkdown(text        )         {
  const renderer = new StreamingMarkdown(ui);
  return `${renderer.feed(`${text}\n`)}${renderer.flush()}`;
}

async function renderDemo(options         )                {
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

function parseOptions(args          )          {
  const value = (name        , fallback = "")         => {
    const direct = args.find((arg) => arg.startsWith(`--${name}=`));
    if (direct) return direct.slice(name.length + 3);
    const index = args.indexOf(`--${name}`);
    return index >= 0 ? args[index + 1] || fallback : fallback;
  };
  const flag = (name        )          => args.includes(`--${name}`);
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
    webSearch: webSearch                        ,
    webSearchUses: Number(value("web-search-uses", "10")),
    thinking: thinking                             ,
    maxTokens: Number(value("max-tokens", flag("team") ? "16384" : "4096")),
    maxSteps: Number(value("max-steps", "100")),
    demo: flag("demo"),
    team: flag("team"),
    workerModel: value("worker-model"),
  };
}

function messageOf(error         )         { return error instanceof Error ? error.message : String(error); }

main().catch((error) => { process.stderr.write(`error: ${messageOf(error)}\n`); process.exitCode = 1; });


//# sourceURL=../src/cli.ts