import type { Message } from "./types.ts";

export interface SessionEvent {
  seq: number;
  at: number;
  type: "system" | "user" | "assistant" | "tool" | "steering";
  message: Message;
}

export class SessionLog {
  readonly #events: SessionEvent[] = [];

  append(type: SessionEvent["type"], message: Message): SessionEvent {
    const event = { seq: this.#events.length + 1, at: Date.now(), type, message };
    this.#events.push(structuredClone(event));
    return event;
  }

  ensureSystem(text: string): void {
    if (this.#events.length === 0) {
      this.append("system", { role: "system", content: [{ type: "text", text }], source: "system" });
    }
  }

  messages(): Message[] {
    return this.#events.map((event) => structuredClone(event.message));
  }

  events(): SessionEvent[] {
    return structuredClone(this.#events);
  }
}

export class SteeringQueue {
  readonly #pending: string[] = [];

  push(text: string): void {
    const trimmed = text.trim();
    if (trimmed) this.#pending.push(trimmed);
  }

  drain(): string[] {
    return this.#pending.splice(0);
  }
}
