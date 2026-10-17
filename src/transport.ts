import type { OAuthSession } from "./auth.ts";
import type { Block, Message, ModelCard, ToolSchema, Usage } from "./types.ts";
import { emptyUsage } from "./types.ts";

export interface CompletionRequest {
  model: string;
  messages: Message[];
  tools: ToolSchema[];
  maxTokens: number;
  thinking: string;
  webSearch: string;
  webSearchUses: number;
}

export interface CompletionResponse {
  message: Message;
  usage: Usage;
}

export interface StreamDelta {
  text?: string;
  reasoning?: string;
}

export class ServiceError extends Error {
  readonly retryable:boolean;
  window?:string;
  resetsAt?:string;
  readonly code:string;
  get resumable():boolean{return this.code==='request_limit'||this.code==='temporary_rate_limit';}
  constructor(message:string,retryable:boolean,code='service_unavailable'){super('MaxLabs: '+message);this.name='ServiceError';this.retryable=retryable;this.code=code;}
}
function eventError(payload:Record<string,unknown>):ServiceError|undefined {
 if(payload.error===undefined||payload.error===null)return;
 const fields=payload.error as Record<string,unknown>,code=String(fields?.code||'')+' '+String(fields?.type||'');
 if(/quota/i.test(code)){
  const error=new ServiceError('The request limit has been reached. Check account limits before retrying.',false,'request_limit');
  if(['h5','weekly'].includes(String(fields.window))){error.window=String(fields.window);error.message='MaxLabs: The '+(error.window==='h5'?'five-hour':'weekly')+' allowance cannot cover this request.';
   if(typeof fields.resets_at==='string'&&/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(fields.resets_at)&&Number.isFinite(Date.parse(fields.resets_at))){error.resetsAt=new Date(fields.resets_at).toISOString();error.message+=' Resets at '+error.resetsAt+'.';}
  }return error;
 }
 if(/rate_limit|concurrency_limit/i.test(code))return new ServiceError('Request traffic is temporarily limited. Retry after current requests finish or the rate window resets.',true,'temporary_rate_limit');
 if(/auth|api_key|permission|access_denied/i.test(code))return new ServiceError('The request was not authorized. Check your sign-in and model access.',false);
 if(/context_length|context_window/i.test(code))return new ServiceError('This conversation exceeds the model context limit. Reduce the context before continuing.',false);
 if(/invalid_request|invalid_argument/i.test(code))return new ServiceError('The service could not accept this request. Review the request before retrying.',false);
 return new ServiceError('The service could not complete this request. Please retry shortly.',true);
}

interface TransportConfig {
  baseUrl?: string;
  apiKey?: string;
  authUrl?: string;
  oauth?: OAuthSession;
  sessionId: string;
}

export class GatewayTransport {
  readonly config: TransportConfig;

  constructor(config: TransportConfig) { this.config = config; }

  async models(signal?: AbortSignal): Promise<ModelCard[]> {
    const response = await this.#request("/models", { method: "GET", signal });
    const payload = await response.json() as { data?: ModelCard[] };
    const models = (payload.data || []).filter((model) => model.id?.trim());
    if (!models.length) throw new Error("Gateway returned no enabled models");
    return models;
  }

  async complete(request: CompletionRequest, onDelta?: (delta: StreamDelta) => void, signal?: AbortSignal): Promise<CompletionResponse> {
    const body = JSON.stringify({
      model: request.model,
      messages: request.messages.map(outboundMessage),
      tools: request.tools.map((tool) => ({ type: "function", function: tool })),
      max_tokens: request.maxTokens,
      web_search: request.webSearch === "off" ? undefined : request.webSearch,
      web_search_uses: request.webSearchUses || undefined,
      reasoning: request.thinking === "default" ? undefined : { effort: request.thinking },
      stream: true,
      stream_options: { include_usage: true },
    });
    const response = await this.#request("/chat/completions", { method: "POST", body, signal, headers: { accept: "text/event-stream" } });
    if (!(response.headers.get("content-type") || "").toLowerCase().includes("text/event-stream")) {
      return parseBuffered(await response.json() as Record<string, unknown>, onDelta);
    }
    return consumeStream(response, onDelta);
  }

  async #request(path: string, init: RequestInit): Promise<Response> {
    const token = await this.config.oauth?.accessToken(init.signal || undefined);
    const baseUrl = token && this.config.authUrl ? `${this.config.authUrl.replace(/\/$/, "")}/app/v1` : this.config.baseUrl?.replace(/\/$/, "");
    const bearer = token || this.config.apiKey;
    if (!baseUrl) throw new Error("No OAuth session or API base URL is configured");
    const response = await fetch(`${baseUrl}/${path.replace(/^\//, "")}`, {
      ...init,
      headers: {
        "content-type": "application/json",
        ...(bearer ? { authorization: `Bearer ${bearer}` } : {}),
        "x-session-id": this.config.sessionId,
        ...init.headers,
      },
    });
    if (!response.ok) {
      if(response.status===429){const payload=await response.clone().json().catch(()=>undefined);const failure=payload&&eventError(payload);if(failure?.resumable){failure.message=await gatewayError(response);throw failure;}}
      throw new Error(await gatewayError(response));
    }
    return response;
  }
}

async function gatewayError(response: Response): Promise<string> {
  const reader = response.body?.getReader();
  const decoder = new TextDecoder();
  let text = "";
  let remaining = 4_096;
  try {
    while (reader && remaining > 0) {
      const { value, done } = await reader.read();
      if (done) break;
      const chunk = value.subarray(0, remaining);
      text += decoder.decode(chunk, { stream: true });
      remaining -= chunk.byteLength;
    }
    text += decoder.decode();
  } finally {
    await reader?.cancel().catch(() => {});
  }
  const fallback = response.statusText || ({
    400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found",
    408: "Request Timeout", 413: "Content Too Large", 422: "Unprocessable Content",
    429: "Too Many Requests", 500: "Internal Server Error", 502: "Bad Gateway",
    503: "Service Unavailable", 504: "Gateway Timeout",
  } as Record<number, string>)[response.status] || "Request failed";
  let detail = fallback;
  const markup = /<[!/?a-zA-Z][^>]*/;
  if ((response.headers.get("content-type") || "").toLowerCase().includes("text/html") || markup.test(text)) {
    detail += ". The gateway returned an HTML error page. Please retry shortly.";
  } else {
    try {
      const envelope = JSON.parse(text);
      const fields = envelope?.error ?? envelope;
      if (typeof fields === "string" && fields.trim()) detail = fields;
      else if (fields && typeof fields === "object") {
        if (typeof fields.message === "string" && fields.message.trim()) detail = fields.message;
        for (const key of ["code", "type", "window", "resets_at"]) {
          if (typeof fields[key] === "string" && fields[key].trim()) detail += `; ${key}=${fields[key]}`;
        }
      }
    } catch {
      if (text.trim() && !text.trim().startsWith("{") && !text.trim().startsWith("[")) detail = text;
    }
  }
  if (markup.test(detail)) detail = fallback + ". The gateway returned an HTML error page. Please retry shortly.";
  detail = detail.replace(/\s+/g, " ").trim();
  const chars = [...detail];
  if (chars.length > 512) detail = chars.slice(0, 509).join("") + "...";
  return `Gateway returned ${response.status}: ${detail}`;
}

function outboundMessage(message: Message): Record<string, unknown> {
  const calls = message.content.filter((block): block is Extract<Block, { type: "tool_call" }> => block.type === "tool_call");
  const result = message.content.find((block): block is Extract<Block, { type: "tool_result" }> => block.type === "tool_result");
  const text = message.content.filter((block): block is Extract<Block, { type: "text" }> => block.type === "text").map((block) => block.text).join("\n");
  if (result && !calls.length) return { role: "tool", tool_call_id: result.callId, content: result.text };
  if (calls.length) return {
    role: message.role,
    content: text || null,
    tool_calls: calls.map((call) => ({ id: call.callId, type: "function", function: { name: call.name, arguments: call.arguments } })),
  };
  return { role: message.role, content: text };
}

async function consumeStream(response: Response, onDelta?: (delta: StreamDelta) => void): Promise<CompletionResponse> {
  if (!response.body) throw new Error("Gateway stream had no body");
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  let text = "";
  let usage = emptyUsage();
  const calls = new Map<number, { id: string; name: string; arguments: string }>();
  let sawFrame = false;
  let eventName='',buffered:CompletionResponse|undefined;
  let finished=false;
  try {
  while (true) {
    const { value, done } = await reader.read();
    buffer += value || "";
    if(done&&buffer&&!buffer.endsWith('\n'))buffer+='\n';
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || "";
    for (const line of lines) {
      if(line.startsWith('event:')){eventName=line.slice(6).trim();continue;}
      if (!line.startsWith("data:")) continue;
      const payload = line.slice(5).trim();
      if (!payload) continue;
      if(payload==='[DONE]'){finished=true;break;}
      let chunk: Record<string, unknown>;
      try { chunk = JSON.parse(payload) as Record<string, unknown>; } catch { continue; }
      const failure=eventError(eventName==='error'?{error:chunk}:chunk);eventName='';if(failure)throw failure;
      sawFrame = true;
      if (chunk.usage) usage = parseUsage(chunk.usage as Record<string, unknown>);
      if((chunk.choices as Array<Record<string,unknown>>)?.[0]?.message){
        if(buffered||text||calls.size)throw new ServiceError('The service returned conflicting completion data. Please retry.',true);
        buffered=parseBuffered(chunk,onDelta);usage=buffered.usage;continue;
      }
      for (const choice of (chunk.choices as Array<Record<string, unknown>> || [])) {
        const delta = choice.delta as Record<string, unknown> || {};
        const reasoning = String(delta.reasoning || delta.reasoning_content || "");
        const content = typeof delta.content === "string" ? delta.content : "";
        if (reasoning) onDelta?.({ reasoning });
        if (content) { text += content; onDelta?.({ text: content }); }
        for (const fragment of delta.tool_calls as Array<Record<string, unknown>> || []) {
          const index = Number(fragment.index || 0);
          const existing = calls.get(index) || { id: "", name: "", arguments: "" };
          const fn = fragment.function as Record<string, unknown> || {};
          existing.id ||= String(fragment.id || "");
          existing.name ||= String(fn.name || "");
          existing.arguments += String(fn.arguments || "");
          calls.set(index, existing);
        }
      }
    }
    if (done||finished) break;
  }
  } finally {await reader.cancel().catch(()=>{});reader.releaseLock();}
  if (!sawFrame) throw new Error("Gateway stream contained no usable frames");
  if(buffered){if(text||calls.size)throw new ServiceError('The service returned conflicting completion data. Please retry.',true);return {...buffered,usage};}
  const blocks: Block[] = [];
  if (text) blocks.push({ type: "text", text });
  for (const call of [...calls.entries()].sort(([a], [b]) => a - b).map(([, value]) => value)) {
    const arguments_ = call.arguments.trim() || "{}";
    JSON.parse(arguments_);
    if (call.name) blocks.push({ type: "tool_call", callId: call.id, name: call.name, arguments: arguments_ });
  }
  if (!blocks.length) throw new Error("Gateway stream contained neither text nor a tool call");
  return { message: { role: "assistant", content: blocks }, usage };
}

function parseBuffered(payload: Record<string, unknown>, onDelta?: (delta: StreamDelta) => void): CompletionResponse {
  const failure=eventError(payload);if(failure)throw failure;
  const choice = (payload.choices as Array<Record<string, unknown>> || [])[0];
  const message = choice?.message as Record<string, unknown> || {};
  const blocks: Block[] = [];
  const content = decodeContent(message.content);
  if (content) { blocks.push({ type: "text", text: content }); onDelta?.({ text: content }); }
  for (const call of message.tool_calls as Array<Record<string, unknown>> || []) {
    const fn = call.function as Record<string, unknown> || {};
    const arguments_ = String(fn.arguments || "{}");
    JSON.parse(arguments_);
    blocks.push({ type: "tool_call", callId: String(call.id || ""), name: String(fn.name || ""), arguments: arguments_ });
  }
  if (!blocks.length) throw new Error("Gateway response contained neither text nor a tool call");
  return { message: { role: "assistant", content: blocks }, usage: parseUsage(payload.usage as Record<string, unknown> || {}) };
}

function decodeContent(value: unknown): string {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) return value.filter((block) => block && typeof block === "object" && (block as { type?: string }).type === "text").map((block) => String((block as { text?: string }).text || "")).join("");
  return "";
}

export function parseUsage(raw: Record<string, unknown>): Usage {
  const prompt = numberAt(raw, "prompt_tokens", "input_tokens");
  const details = raw.prompt_tokens_details as Record<string, unknown> || {};
  const cacheRead = numberAt(raw, "prompt_cache_hit_tokens", "cache_read_input_tokens") || numberAt(details, "cached_tokens");
  const cacheWrite = numberAt(raw, "cache_write_tokens", "cache_creation_input_tokens") || numberAt(details, "cache_write_tokens");
  const server = raw.server_tool_use as Record<string, unknown> || {};
  return {
    uncachedInputTokens: Math.max(0, prompt - cacheRead - cacheWrite),
    cacheReadTokens: cacheRead,
    cacheWriteTokens: cacheWrite,
    outputTokens: numberAt(raw, "completion_tokens", "output_tokens"),
    reasoningTokens: numberAt(raw, "reasoning_tokens") || numberAt(raw.completion_tokens_details as Record<string, unknown> || {}, "reasoning_tokens"),
    webSearches: numberAt(raw, "web_search_requests") || numberAt(server, "web_search_requests"),
  };
}

function numberAt(object: Record<string, unknown>, ...keys: string[]): number {
  for (const key of keys) if (Number.isFinite(Number(object[key])) && Number(object[key]) >= 0) return Number(object[key]);
  return 0;
}
