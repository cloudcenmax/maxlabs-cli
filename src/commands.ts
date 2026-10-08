import type { AgentSettings, ModelCard } from "./types.ts";

export const thinkingLevels: AgentSettings["thinking"][] = ["minimal", "low", "medium", "high", "default"];

export function resolveThinkingLevel(values: string[]): AgentSettings["thinking"] | undefined {
  return [...values].reverse().find((value): value is AgentSettings["thinking"] =>
    thinkingLevels.includes(value as AgentSettings["thinking"]));
}

export function defaultModel(requested: string, models: ModelCard[]): string {
  if (requested.trim()) return requested.trim();
  return models.find((model) => model.id === "worker")?.id || models[0]?.id || "auto";
}
