                                              
                                                                               
import { emptyUsage } from "./types.js";

                                    
                
                      
                      
                    
                   
                    
                        
 

                                     
                   
               
 

                              
                
                     
 

                           
                   
                  
                   
                       
                    
 

export class GatewayTransport {
           config                 ;

  constructor(config                 ) { this.config = config; }

  async models(signal              )                       {
    const response = await this.#request("/models", { method: "GET", signal });
    const payload = await response.json()                          ;
    const models = (payload.data || []).filter((model) => model.id?.trim());
    if (!models.length) throw new Error("Gateway returned no enabled models");
    return models;
  }

  async complete(request                   , onDelta                               , signal              )                              {
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
      return parseBuffered(await response.json()                           , onDelta);
    }
    return consumeStream(response, onDelta);
  }

  async #request(path        , init             )                    {
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
      const excerpt = (await response.text()).slice(0, 4_096).trim();
      throw new Error(`Gateway returned ${response.status}: ${excerpt}`);
    }
    return response;
  }
}

function outboundMessage(message         )                          {
  const calls = message.content.filter((block)                                                 => block.type === "tool_call");
  const result = message.content.find((block)                                                   => block.type === "tool_result");
  const text = message.content.filter((block)                                            => block.type === "text").map((block) => block.text).join("\n");
  if (result && !calls.length) return { role: "tool", tool_call_id: result.callId, content: result.text };
  if (calls.length) return {
    role: message.role,
    content: text || null,
    tool_calls: calls.map((call) => ({ id: call.callId, type: "function", function: { name: call.name, arguments: call.arguments } })),
  };
  return { role: message.role, content: text };
}

async function consumeStream(response          , onDelta                               )                              {
  if (!response.body) throw new Error("Gateway stream had no body");
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  let text = "";
  let usage = emptyUsage();
  const calls = new Map                                                         ();
  let sawFrame = false;
  while (true) {
    const { value, done } = await reader.read();
    buffer += value || "";
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || "";
    for (const line of lines) {
      if (!line.startsWith("data:")) continue;
      const payload = line.slice(5).trim();
      if (!payload || payload === "[DONE]") continue;
      let chunk                         ;
      try { chunk = JSON.parse(payload)                           ; } catch { continue; }
      sawFrame = true;
      if (chunk.usage) usage = parseUsage(chunk.usage                           );
      for (const choice of (chunk.choices                                   || [])) {
        const delta = choice.delta                            || {};
        const reasoning = String(delta.reasoning || delta.reasoning_content || "");
        const content = typeof delta.content === "string" ? delta.content : "";
        if (reasoning) onDelta?.({ reasoning });
        if (content) { text += content; onDelta?.({ text: content }); }
        for (const fragment of delta.tool_calls                                   || []) {
          const index = Number(fragment.index || 0);
          const existing = calls.get(index) || { id: "", name: "", arguments: "" };
          const fn = fragment.function                            || {};
          existing.id ||= String(fragment.id || "");
          existing.name ||= String(fn.name || "");
          existing.arguments += String(fn.arguments || "");
          calls.set(index, existing);
        }
      }
    }
    if (done) break;
  }
  if (!sawFrame) throw new Error("Gateway stream contained no usable frames");
  const blocks          = [];
  if (text) blocks.push({ type: "text", text });
  for (const call of [...calls.entries()].sort(([a], [b]) => a - b).map(([, value]) => value)) {
    const arguments_ = call.arguments.trim() || "{}";
    JSON.parse(arguments_);
    if (call.name) blocks.push({ type: "tool_call", callId: call.id, name: call.name, arguments: arguments_ });
  }
  if (!blocks.length) throw new Error("Gateway stream contained neither text nor a tool call");
  return { message: { role: "assistant", content: blocks }, usage };
}

function parseBuffered(payload                         , onDelta                               )                     {
  const choice = (payload.choices                                   || [])[0];
  const message = choice?.message                            || {};
  const blocks          = [];
  const content = decodeContent(message.content);
  if (content) { blocks.push({ type: "text", text: content }); onDelta?.({ text: content }); }
  for (const call of message.tool_calls                                   || []) {
    const fn = call.function                            || {};
    const arguments_ = String(fn.arguments || "{}");
    JSON.parse(arguments_);
    blocks.push({ type: "tool_call", callId: String(call.id || ""), name: String(fn.name || ""), arguments: arguments_ });
  }
  if (!blocks.length) throw new Error("Gateway response contained neither text nor a tool call");
  return { message: { role: "assistant", content: blocks }, usage: parseUsage(payload.usage                            || {}) };
}

function decodeContent(value         )         {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) return value.filter((block) => block && typeof block === "object" && (block                     ).type === "text").map((block) => String((block                     ).text || "")).join("");
  return "";
}

export function parseUsage(raw                         )        {
  const prompt = numberAt(raw, "prompt_tokens", "input_tokens");
  const details = raw.prompt_tokens_details                            || {};
  const cacheRead = numberAt(raw, "prompt_cache_hit_tokens", "cache_read_input_tokens") || numberAt(details, "cached_tokens");
  const cacheWrite = numberAt(raw, "cache_write_tokens", "cache_creation_input_tokens") || numberAt(details, "cache_write_tokens");
  const server = raw.server_tool_use                            || {};
  return {
    uncachedInputTokens: Math.max(0, prompt - cacheRead - cacheWrite),
    cacheReadTokens: cacheRead,
    cacheWriteTokens: cacheWrite,
    outputTokens: numberAt(raw, "completion_tokens", "output_tokens"),
    reasoningTokens: numberAt(raw, "reasoning_tokens") || numberAt(raw.completion_tokens_details                            || {}, "reasoning_tokens"),
    webSearches: numberAt(raw, "web_search_requests") || numberAt(server, "web_search_requests"),
  };
}

function numberAt(object                         , ...keys          )         {
  for (const key of keys) if (Number.isFinite(Number(object[key])) && Number(object[key]) >= 0) return Number(object[key]);
  return 0;
}


//# sourceURL=../src/transport.ts