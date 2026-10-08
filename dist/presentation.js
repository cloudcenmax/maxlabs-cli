const reset = "\x1b[0m";
const boldOn = "\x1b[1m";
const dimOn = "\x1b[2m";
const italicOn = "\x1b[3m";
const pinkTruecolor = "\x1b[38;2;255;102;178m";
const pink256 = "\x1b[38;5;205m";
const bandTruecolor = "\x1b[48;2;255;102;178m\x1b[38;2;0;0;0m";
const band256 = "\x1b[48;5;205m\x1b[38;5;16m";
const green = "\x1b[32m";
const red = "\x1b[31m";
const cyan = "\x1b[36m";

export class Presentation {
           ansi         ;
           truecolor         ;
           promptBand         ;

  constructor(ansi         , environment                    = process.env) {
    const colorPreference = (environment.MAXLABS_COLOR || "").trim().toLowerCase();
    const forceColor = /^(1|true|yes|on|always)$/.test(colorPreference);
    const disableColor = /^(0|false|no|off|never)$/.test(colorPreference);
    this.ansi = ansi && !disableColor && (forceColor || !("NO_COLOR" in environment));
    this.truecolor = /truecolor|24bit/i.test(environment.COLORTERM || "");
    this.promptBand = !/^(0|false|no|off)$/i.test((environment.MAXLABS_PROMPT_BAND || "").trim());
  }

  style(code        , text        )         { return this.ansi ? `${code}${text}${reset}` : text; }
  brand(text        )         { return this.style(this.truecolor ? pinkTruecolor : pink256, text); }
  bold(text        )         { return this.style(boldOn, text); }
  tone(text        )         { return this.style(dimOn, text); }
  thought(text        )         { return this.style(dimOn + italicOn, text); }
  success(text        )         { return this.style(green, text); }
  error(text        )         { return this.style(red, text); }
  code(text        )         { return this.style(cyan, text); }

  submittedPrompt(text        )         {
    if (!this.ansi) return `> ${text}`;
    if (this.promptBand) {
      const band = this.truecolor ? bandTruecolor : band256;
      return `${band}${boldOn} ❯ ${text} ${reset}`;
    }
    return `${boldOn}${this.truecolor ? pinkTruecolor : pink256}❯${reset} ${boldOn}${text}${reset}`;
  }

  inputPrompt(mode                         , kind                                  = "normal")         {
    if (kind === "approval") return this.brand("  choice> ");
    if (kind === "model") return this.brand("model> ");
    if (mode === "plan") return this.brand("plan> ");
    return "> ";
  }

  modeNotice(mode                         )         {
    if (mode === "plan") return `${this.brand("  PLAN MODE on")}  ${this.tone("workspace changes are refused. shift+tab to exit.")}`;
    if (mode === "auto") return `${this.brand("  auto mode on")}  ${this.tone("audited safe work runs unattended. shift+tab for plan mode.")}`;
    return `${this.brand("  act mode on")}  ${this.tone("workspace changes ask for approval. shift+tab for plan mode.")}`;
  }

  markdownLine(line        , inFence         )         {
    if (!this.ansi) return line;
    if (/^```/.test(line)) return this.tone(line);
    if (inFence) return this.style(dimOn, `  ${line}`);
    const heading = /^(#{1,6})\s+(.+)$/.exec(line);
    if (heading) return this.bold(heading[2]);
    const bullet = /^(\s*)[-*]\s+(.+)$/.exec(line);
    if (bullet) return `${bullet[1]}${this.brand("•")} ${this.inline(bullet[2])}`;
    return this.inline(line);
  }

  inline(line        )         {
    if (!this.ansi) return line;
    return line
      .replace(/`([^`]+)`/g, (_match, code) => this.code(code))
      .replace(/\*\*([^*]+)\*\*/g, (_match, strong) => this.bold(strong));
  }
}

                        
                
                   
               
 

                       
                   
                               
 

export async function renderStartupBrand(
  presentation              ,
  details              ,
  output              = process.stdout,
  environment                    = process.env,
)                {
  const animationEnabled = presentation.ansi
    && !environment.CI
    && environment.TERM !== "dumb"
    && !/^(0|false|no|off)$/i.test((environment.MAXLABS_ANIMATION || "").trim());

  if (animationEnabled) {
    const frames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧"];
    const phases = ["starting", "loading workspace", "connecting", "ready"];
    for (const [index, frame] of frames.entries()) {
      output.write(`\r\x1b[2K  ${presentation.brand(frame)} ${presentation.bold("maxlabs/cli")} ${presentation.tone(phases[Math.floor(index / 2)])}`);
      await new Promise((resolve) => setTimeout(resolve, 45));
    }
    output.write("\r\x1b[2K");
  }

  const columns = output.columns || 80;
  if (columns < 52) {
    output.write(`${presentation.bold(presentation.brand("◆ maxlabs/cli"))}\n`);
    output.write(`${presentation.tone(`${details.model} · think:${details.thinking} · ${details.auth}`)}\n`);
    return;
  }

  const width = Math.min(72, columns - 2);
  const label = "─[ maxlabs/cli ]";
  const top = `╭${label}${"─".repeat(Math.max(0, width - label.length - 1))}╮`;
  const status = `${details.model}  ·  think:${details.thinking}  ·  ${details.auth}`;
  const middlePadding = " ".repeat(Math.max(1, width - status.length - 3));
  const hint = "/help  ·  shift+tab  ·  live steering ";
  const bottom = `╰─ ${hint}${"─".repeat(Math.max(0, width - hint.length - 3))}╯`;

  output.write(`${presentation.brand(top)}\n`);
  output.write(`${presentation.brand("│")}  ${presentation.bold(status)}${middlePadding}${presentation.brand("│")}\n`);
  output.write(`${presentation.brand(bottom)}\n`);
}

export class StreamingMarkdown {
  #pending = "";
  #inFence = false;
           presentation              ;

  constructor(presentation              ) { this.presentation = presentation; }

  feed(fragment        )         {
    this.#pending += fragment;
    const lines = this.#pending.split("\n");
    this.#pending = lines.pop() || "";
    return lines.map((line) => this.#render(line)).join("\n") + (lines.length ? "\n" : "");
  }

  flush()         {
    if (!this.#pending) return "";
    const rendered = this.#render(this.#pending);
    this.#pending = "";
    return rendered;
  }

  get hasPending()          { return this.#pending.length > 0; }

  flushPartial()         {
    const partial = this.#pending;
    this.#pending = "";
    return partial;
  }

  #render(line        )         {
    const rendered = this.presentation.markdownLine(line, this.#inFence);
    if (/^```/.test(line)) this.#inFence = !this.#inFence;
    return rendered;
  }
}


//# sourceURL=../src/presentation.ts