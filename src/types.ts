export type Role = "system" | "user" | "assistant" | "tool";

export interface TextBlock {
  type: "text";
  text: string;
}

export interface ToolCallBlock {
  type: "tool_call";
  callId: string;
  name: string;
  arguments: string;
}

export interface ToolResultBlock {
  type: "tool_result";
  callId: string;
  text: string;
  isError?: boolean;
}

export type Block = TextBlock | ToolCallBlock | ToolResultBlock;

export interface Message {
  role: Role;
  content: Block[];
  source?: string;
}

export interface Usage {
  uncachedInputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  webSearches: number;
}

export interface ToolSchema {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
}

export interface ToolResult {
  text: string;
  isError?: boolean;
  diff?: string;
}

export interface ModelCard {
  id: string;
  description?: string;
  context_window?: number;
  max_output_tokens?: number;
}

export interface AgentSettings {
  model: string;
  thinking: "minimal" | "low" | "medium" | "high" | "default";
  webSearch: "off" | "auto" | "always";
  webSearchUses: number;
  maxTokens: number;
  maxSteps: number;
}

export const emptyUsage = (): Usage => ({
  uncachedInputTokens: 0,
  cacheReadTokens: 0,
  cacheWriteTokens: 0,
  outputTokens: 0,
  reasoningTokens: 0,
  webSearches: 0,
});
