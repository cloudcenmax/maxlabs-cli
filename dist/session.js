                                          

                               
              
             
                                                              
                   
 

export class SessionLog {
           #events                 = [];

  append(type                      , message         )               {
    const event = { seq: this.#events.length + 1, at: Date.now(), type, message };
    this.#events.push(structuredClone(event));
    return event;
  }

  ensureSystem(text        )       {
    if (this.#events.length === 0) {
      this.append("system", { role: "system", content: [{ type: "text", text }], source: "system" });
    }
  }

  messages()            {
    return this.#events.map((event) => structuredClone(event.message));
  }

  events()                 {
    return structuredClone(this.#events);
  }
}

export class SteeringQueue {
           #pending           = [];

  push(text        )       {
    const trimmed = text.trim();
    if (trimmed) this.#pending.push(trimmed);
  }

  drain()           {
    return this.#pending.splice(0);
  }
}


//# sourceURL=../src/session.ts