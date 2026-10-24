import { ApprovalRequired, allowsReview,                                                 } from "./command-auditor.js";














const readOnlyCommands = new Set([
  "ls", "cat", "head", "tail", "wc", "file", "stat", "tree", "du", "df", "realpath", "basename", "dirname", "readlink", "pwd", "find", "fd",
  "grep", "rg", "ag", "sort", "uniq", "cut", "tr", "sed", "awk", "jq", "diff", "comm", "column", "nl", "rev", "fold", "echo", "printf", "date", "true", "false",
  "whoami", "id", "uname", "hostname", "which", "type", "command", "env", "printenv", "sleep",
]);

const readOnlySubcommands                              = {
  git: new Set(["status", "diff", "log", "show", "branch", "remote", "tag", "describe", "blame", "shortlog", "rev-parse", "ls-files", "cat-file"]),
  go: new Set(["build", "test", "vet", "list", "env", "doc", "version"]),
  npm: new Set(["test", "ls", "list", "view", "outdated"]),
  docker: new Set(["ps", "images", "version", "logs", "inspect"]),
  kubectl: new Set(["get", "describe", "logs", "version"]),
};

const destructive = new Map([
  ["rm", "deletes files"], ["rmdir", "deletes directories"], ["unlink", "deletes a file"], ["shred", "destroys file contents"], ["truncate", "destroys file contents"],
  ["mv", "moves or overwrites files"], ["tee", "writes files"], ["xargs", "constructs another command"], ["parallel", "constructs other commands"],
  ["bash", "runs an opaque script"], ["sh", "runs an opaque script"], ["zsh", "runs an opaque script"], ["powershell", "runs an opaque script"], ["pwsh", "runs an opaque script"],
  ["osascript", "controls desktop applications"],
]);

export class PermissionGuard {
  #queue                   = Promise.resolve();
           #always = new Set        ();
           decisions                                                                 = [];
  mode      ;
  commandReviewer                  ;
           prompt                ;
           projectCommand                                        ;

  constructor(mode      , prompt                , projectCommand                                        ) {
    this.mode = mode;
    this.prompt = prompt;
    this.projectCommand = projectCommand;
  }

  async permit(tool      , arguments_                         , context                       )                              {
    const result = this.#queue.then(() => this.#permit(tool, arguments_, context));
    this.#queue = result.catch(() => {});
    return result;
  }

  async #permit(tool      , arguments_                         , context                       )                              {
    let decision = decide(this.mode, tool, arguments_);
    if (this.mode === 'auto' && !tool.readOnly && tool.schema.name === 'bash' && this.projectCommand) {
      if (await this.projectCommand(String(arguments_.command || ''))) {
        this.decisions.push({effect:'allow', reason:'audited project command within the assigned workspace', oneTimeOnly:false, scope:decision.scope, tool:'bash', granted:true});
        return;
      }
      decision={...decision,effect:'ask',reason:freshApprovalRisk('bash',arguments_)||'the command’s scope or effects require review',oneTimeOnly:true};
    }
    if (this.mode === 'auto' && tool.schema.name === 'bash' && decision.effect === 'ask' && this.commandReviewer) {
      let result;
      try { result = await this.commandReviewer(arguments_, context); } catch { /* confirmation fallback */ }
      context?.signal?.throwIfAborted();
      if (result?.decision === 'stop') {
        this.decisions.push({ ...decision, effect:'deny', oneTimeOnly:true, reason:'Cenmax AI command auditor stopped for explicit user approval', tool:'bash', granted:false });
        throw new ApprovalRequired(String(arguments_.command || ''), result.reviewedWorkspace);
      }
      decision = { ...decision, effect: allowsReview(result) ? 'allow' : 'ask', oneTimeOnly: true,
        reason: allowsReview(result) ? 'Cenmax AI command auditor approved: effects and scope match the user request' : 'Cenmax AI command auditor needs confirmation: authorization, scope, or effects are uncertain' };
    }
    if (decision.effect === "allow") {
      this.decisions.push({ ...decision, tool: tool.schema.name, granted: true });
      return undefined;
    }
    if (decision.effect === "deny") {
      this.decisions.push({ ...decision, tool: tool.schema.name, granted: false });
      return decision.reason;
    }
    if (!decision.oneTimeOnly && this.#always.has(decision.scope)) return undefined;
    const answer = await this.prompt(tool.schema.name, arguments_, decision);
    const granted = answer !== "deny";
    if (answer === "always" && !decision.oneTimeOnly) this.#always.add(decision.scope);
    this.decisions.push({ ...decision, tool: tool.schema.name, granted });
    return granted ? undefined : "the user denied this operation";
  }
}

export function decide(mode      , tool      , arguments_                         )                     {
  const scope = approvalScope(tool.schema.name, arguments_);
  if (tool.readOnly) return { effect: "allow", reason: "reads only", oneTimeOnly: false, scope };
  if (mode === "plan") return { effect: "deny", reason: "plan mode refuses workspace changes", oneTimeOnly: false, scope };
  const risk = freshApprovalRisk(tool.schema.name, arguments_);
  if (mode === "auto") {
    if (tool.schema.name === "bash" && isReadOnlyCommand(String(arguments_.command || ""))) {
      return { effect: "allow", reason: "audited read-only command", oneTimeOnly: false, scope };
    }
    if (risk) return { effect: "ask", reason: risk, oneTimeOnly: true, scope };
    if (tool.schema.name === "write" || tool.schema.name === "edit") {
      return { effect: "allow", reason: "audited workspace edit", oneTimeOnly: false, scope };
    }
  }
  return { effect: "ask", reason: risk || "changes state", oneTimeOnly: Boolean(risk), scope };
}

export function freshApprovalRisk(tool        , arguments_                         )                     {
  if (tool !== "bash") return tool === "webfetch" ? "reaches the network" : undefined;
  const command = String(arguments_.command || "").trim();
  if (!command) return "empty command";
  if (/\.env\b|\.aws(?:[\/\s]|$)|\.ssh(?:[\/\s]|$)|\.(?:npmrc|netrc|pypirc)\b/.test(command)) return "accesses private environment or credential files";
  if (command.includes("$(`") || command.includes("$(") || command.includes("`")) return "contains hidden command substitution";
  if (hasUnsafeWriteRedirect(command)) return "redirects output to a file";
  if (/(^|[;&|]\s*)cd\s+(?:\/|~|\.\.)/.test(command)) return "changes outside the working directory before continuing";
  for (const segment of splitCommands(command)) {
    const words = shellWords(segment);
    const name = basename(words[0] || "");
    const reason = destructive.get(name);
    if (reason) return `${name} ${reason}`;
    if (name === "find" && words.some((word) => ["-delete", "-exec", "-execdir", "-ok", "-okdir"].includes(word))) return "find can delete files or execute commands";
    if (name === "git" && words.some((word) => ["clean", "reset", "push", "restore", "checkout"].includes(word))) return "Git operation can discard or publish work";
    if (["python", "python3", "node", "php", "ruby", "perl"].includes(name) && words.some((word) => ["-c", "-e", "-r", "--eval"].includes(word))) return `${name} executes inline code`;
  }
  return undefined;
}

export function isReadOnlyCommand(command        )          {
  if (freshApprovalRisk("bash", { command })) return false;
  const segments = splitCommands(command);
  if (!segments.length) return false;
  return segments.every((segment) => {
    const words = shellWords(segment);
    while (['env','command'].includes(basename(words[0] || ''))) words.shift();
    while (words[0]?.includes("=") && !words[0].startsWith("=")) words.shift();
    const name = basename(words[0] || "");
    if (name === 'git' && words.some(word => ['-D','--delete'].includes(word))) return false;
    if (name === "sed" && words.some((word) => word === "-i" || word.startsWith("--in-place"))) return false;
    if (name === "find" && words.some((word) => ["-delete", "-exec", "-execdir", "-ok", "-okdir"].includes(word))) return false;
    if (readOnlyCommands.has(name)) return true;
    return Boolean(readOnlySubcommands[name]?.has(words[1] || ""));
  });
}

function approvalScope(tool        , arguments_                         )         {
  if (tool !== "bash") return tool;
  const families = splitCommands(String(arguments_.command || "")).map((segment) => {
    const words = shellWords(segment);
    const name = basename(words[0] || "unknown");
    if (name === "php" && basename(words[1] || "") === "artisan") return `php artisan ${words[2] || ""}`.trim();
    if (["npm", "pnpm", "yarn"].includes(name) && words[1] === "run") return `${name} run ${words[2] || ""}`.trim();
    if (["git", "go", "npm", "pnpm", "yarn", "composer", "docker", "kubectl", "cargo", "make"].includes(name)) return `${name} ${words[1] || ""}`.trim();
    return name;
  });
  return `bash:${families.join("+") || "empty"}`;
}

function splitCommands(command        )           {
  return command.split(/(?:&&|\|\||;|\n|(?<!\|)\|(?!\|))/).map((part) => part.trim()).filter(Boolean);
}

function hasUnsafeWriteRedirect(command        )          {
  for (const match of command.matchAll(/(?:^|\s)(?:\d*)>>?\s*([^\s;&|]+)/g)) {
    const target = (match[1] || "").replace(/^['"]|['"]$/g, "");
    if (!["/dev/null", "/dev/stdout", "/dev/stderr", "NUL"].includes(target)) return true;
  }
  return false;
}

function shellWords(command        )           {
  return command.match(/(?:[^\s"']+|"[^"]*"|'[^']*')+/g)?.map((word) => word.replace(/^["']|["']$/g, "")) || [];
}

function basename(path        )         {
  return path.replaceAll("\\", "/").split("/").at(-1) || "";
}


//# sourceURL=../src/permission.ts